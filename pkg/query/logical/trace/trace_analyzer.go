// Licensed to Apache Software Foundation (ASF) under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Apache Software Foundation (ASF) licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package trace

import (
	"context"
	"fmt"

	commonv1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/common/v1"
	tracev1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/trace/v1"
	"github.com/apache/skywalking-banyandb/pkg/convert"
	"github.com/apache/skywalking-banyandb/pkg/iter"
	"github.com/apache/skywalking-banyandb/pkg/iter/sort"
	pbv1 "github.com/apache/skywalking-banyandb/pkg/pb/v1"
	"github.com/apache/skywalking-banyandb/pkg/query/executor"
	"github.com/apache/skywalking-banyandb/pkg/query/logical"
	"github.com/apache/skywalking-banyandb/pkg/query/model"
)

const defaultLimit uint32 = 20

// Analyze converts logical expressions to executable operation tree represented by Plan.
func Analyze(criteria *tracev1.QueryRequest, metadata []*commonv1.Metadata, ss []logical.Schema,
	ecc []executor.TraceExecutionContext, traceIDTagName, timestampTagName string,
) (logical.Plan, error) {
	// parse fields
	if len(metadata) != len(ss) {
		return nil, fmt.Errorf("number of schemas %d not equal to number of metadata %d", len(ss), len(metadata))
	}
	var orderByTag string
	if criteria.OrderBy != nil {
		indexRuleName := criteria.OrderBy.IndexRuleName
		ok, indexRule := ss[0].IndexRuleDefined(indexRuleName)
		if !ok {
			return nil, fmt.Errorf("index rule %s not found", indexRuleName)
		}
		ot := indexRule.Tags[len(indexRule.Tags)-1]
		if ot != timestampTagName {
			orderByTag = ot
		}
	}
	var plan logical.UnresolvedPlan
	var s logical.Schema
	tagProjection := convertStringProjectionToTags(criteria.GetTagProjection())
	if len(metadata) == 1 {
		plan = parseTraceTags(criteria, metadata[0], ecc[0], tagProjection, traceIDTagName, orderByTag)
		s = ss[0]
	} else {
		var err error
		if s, err = mergeSchema(ss); err != nil {
			return nil, err
		}
		plan = &unresolvedTraceMerger{
			criteria:      criteria,
			metadata:      metadata,
			ecc:           ecc,
			tagProjection: tagProjection,
		}
	}

	// parse limit
	limitParameter := criteria.GetLimit()
	if limitParameter == 0 {
		limitParameter = defaultLimit
	}
	plan = newTraceLimit(plan, criteria.GetOffset(), limitParameter)

	p, err := plan.Analyze(s)
	if err != nil {
		return nil, err
	}
	rules := []logical.OptimizeRule{
		logical.NewPushDownOrder(criteria.OrderBy),
		logical.NewPushDownMaxSize(int(limitParameter + criteria.GetOffset())),
	}
	if err := logical.ApplyRules(p, rules...); err != nil {
		return nil, err
	}
	return p, nil
}

// DistributedAnalyze converts logical expressions to executable operation tree represented by Plan.
func DistributedAnalyze(criteria *tracev1.QueryRequest, ss []logical.Schema) (logical.Plan, error) {
	// parse fields
	var s logical.Schema
	if len(ss) == 1 {
		s = ss[0]
	} else {
		var err error
		if s, err = mergeSchema(ss); err != nil {
			return nil, err
		}
	}
	plan := newUnresolvedTraceDistributed(criteria)

	// parse limit
	limitParameter := criteria.GetLimit()
	if limitParameter == 0 {
		limitParameter = defaultLimit
	}
	plan = newDistributedTraceLimit(plan, criteria.Offset, limitParameter)
	return plan.Analyze(s)
}

// Parent refers to a parent node in the execution tree(plan).
type Parent struct {
	UnresolvedInput logical.UnresolvedPlan
	Input           logical.Plan
}

func parseTraceTags(criteria *tracev1.QueryRequest, metadata *commonv1.Metadata,
	ec executor.TraceExecutionContext, tagProjection [][]*logical.Tag, traceIDTagName, orderByTag string,
) logical.UnresolvedPlan {
	timeRange := criteria.GetTimeRange()
	return &unresolvedTraceTagFilter{
		startTime:      timeRange.GetBegin().AsTime(),
		endTime:        timeRange.GetEnd().AsTime(),
		metadata:       metadata,
		criteria:       criteria.Criteria,
		projectionTags: tagProjection,
		ec:             ec,
		traceIDTagName: traceIDTagName,
		orderByTag:     orderByTag,
	}
}

// convertStringProjectionToTags converts a string array projection to tag projection format.
// For trace, we create a single tag family with empty name since traces don't have families.
func convertStringProjectionToTags(tagNames []string) [][]*logical.Tag {
	if len(tagNames) == 0 {
		return nil
	}

	// For trace, create a single tag family (empty name) with all tags
	tags := make([]*logical.Tag, len(tagNames))
	for i, tagName := range tagNames {
		tags[i] = logical.NewTag("", tagName) // Empty family name for trace
	}

	return [][]*logical.Tag{tags}
}

// traceLimit implements limit for local trace queries.
var (
	_ logical.Plan             = (*traceLimit)(nil)
	_ logical.UnresolvedPlan   = (*traceLimit)(nil)
	_ executor.TraceExecutable = (*traceLimit)(nil)
)

type traceLimit struct {
	*Parent
	limitNum  uint32
	offsetNum uint32
}

func newTraceLimit(input logical.UnresolvedPlan, offset, num uint32) logical.UnresolvedPlan {
	return &traceLimit{
		Parent: &Parent{
			UnresolvedInput: input,
		},
		offsetNum: offset,
		limitNum:  num,
	}
}

func (l *traceLimit) Analyze(s logical.Schema) (logical.Plan, error) {
	var err error
	l.Input, err = l.UnresolvedInput.Analyze(s)
	if err != nil {
		return nil, err
	}
	return l, nil
}

func (l *traceLimit) Close() {
	l.Parent.Input.(executor.TraceExecutable).Close()
}

func (l *traceLimit) Execute(ctx context.Context) (iter.Iterator[model.TraceResult], error) {
	// Apply offset and limit to trace results (not spans within each trace)
	resultIterator, err := l.Parent.Input.(executor.TraceExecutable).Execute(ctx)
	if err != nil {
		return iter.Empty[model.TraceResult](), err
	}

	// Return a lazy iterator that handles offset and limit at the result level
	return &traceLimitIterator{
		sourceIterator: resultIterator,
		offset:         int(l.offsetNum),
		limit:          int(l.limitNum),
		currentIndex:   0,
		returned:       0,
	}, nil
}

func (l *traceLimit) Schema() logical.Schema {
	return l.Input.Schema()
}

func (l *traceLimit) String() string {
	return fmt.Sprintf("%s Trace Limit: %d, %d", l.Input.String(), l.offsetNum, l.limitNum)
}

func (l *traceLimit) Children() []logical.Plan {
	return []logical.Plan{l.Input}
}

// distributedTraceLimit implements distributed limit for trace queries.
var _ executor.TraceExecutable = (*distributedTraceLimit)(nil)

type distributedTraceLimit struct {
	*Parent
	offsetNum uint32
	limitNum  uint32
}

func newDistributedTraceLimit(input logical.UnresolvedPlan, offset, num uint32) logical.UnresolvedPlan {
	return &distributedTraceLimit{
		Parent: &Parent{
			UnresolvedInput: input,
		},
		offsetNum: offset,
		limitNum:  num,
	}
}

func (dtl *distributedTraceLimit) Analyze(s logical.Schema) (logical.Plan, error) {
	var err error
	dtl.Input, err = dtl.UnresolvedInput.Analyze(s)
	if err != nil {
		return nil, err
	}
	return dtl, nil
}

func (dtl *distributedTraceLimit) Close() {
	dtl.Parent.Input.(executor.TraceExecutable).Close()
}

func (dtl *distributedTraceLimit) Execute(ctx context.Context) (iter.Iterator[model.TraceResult], error) {
	resultIterator, err := dtl.Parent.Input.(executor.TraceExecutable).Execute(ctx)
	if err != nil {
		return iter.Empty[model.TraceResult](), err
	}

	return &traceLimitIterator{
		sourceIterator: resultIterator,
		offset:         int(dtl.offsetNum),
		limit:          int(dtl.limitNum),
		currentIndex:   0,
		returned:       0,
	}, nil
}

func (dtl *distributedTraceLimit) Schema() logical.Schema {
	return dtl.Input.Schema()
}

func (dtl *distributedTraceLimit) String() string {
	return fmt.Sprintf("%s Distributed Trace Limit: %d, %d", dtl.Input.String(), dtl.offsetNum, dtl.limitNum)
}

func (dtl *distributedTraceLimit) Children() []logical.Plan {
	return []logical.Plan{dtl.Input}
}

// comparableTraceResult implements sort.Comparable for trace results.
var _ sort.Comparable = (*comparableTraceResult)(nil)

type comparableTraceResult struct {
	result    model.TraceResult
	sortField []byte
}

func newComparableTraceResult(tr model.TraceResult, sortByTime bool, sortTagSpec logical.TagSpec) (*comparableTraceResult, error) {
	var sortField []byte
	if sortByTime {
		// For trace, we might need to extract timestamp from span data
		// For now, use a simple hash of trace ID
		sortField = convert.StringToBytes(tr.TID)
	} else if len(tr.Tags) > 0 && sortTagSpec.TagIdx < len(tr.Tags) {
		// Extract sort field from tags
		tag := tr.Tags[sortTagSpec.TagIdx]
		if len(tag.Values) > 0 {
			var err error
			sortField, err = pbv1.MarshalTagValue(tag.Values[0])
			if err != nil {
				return nil, err
			}
		}
	}

	return &comparableTraceResult{
		result:    tr,
		sortField: sortField,
	}, nil
}

func (ctr *comparableTraceResult) SortedField() []byte {
	return ctr.sortField
}

// sortableTraceResults implements sort.Iterator for trace results.
var _ sort.Iterator[*comparableTraceResult] = (*sortableTraceResults)(nil)

type sortableTraceResults struct {
	cur          *comparableTraceResult
	traceResults []model.TraceResult
	sortTagSpec  logical.TagSpec
	index        int
	isSortByTime bool
}

func newSortableTraceResults(traceResults []model.TraceResult, isSortByTime bool, sortTagSpec logical.TagSpec) *sortableTraceResults {
	return &sortableTraceResults{
		traceResults: traceResults,
		isSortByTime: isSortByTime,
		sortTagSpec:  sortTagSpec,
	}
}

func (*sortableTraceResults) Close() error {
	return nil
}

func (str *sortableTraceResults) Next() bool {
	return str.iter(func(tr model.TraceResult) (*comparableTraceResult, error) {
		return newComparableTraceResult(tr, str.isSortByTime, str.sortTagSpec)
	})
}

func (str *sortableTraceResults) Val() *comparableTraceResult {
	return str.cur
}

func (str *sortableTraceResults) iter(fn func(model.TraceResult) (*comparableTraceResult, error)) bool {
	if str.index >= len(str.traceResults) {
		return false
	}
	cur, err := fn(str.traceResults[str.index])
	str.index++
	if err != nil {
		return str.iter(fn)
	}
	str.cur = cur
	return str.index <= len(str.traceResults)
}

// sortedTraceIterator implements iter.Iterator[model.TraceResult] with deduplication.
type sortedTraceIterator struct {
	sort.Iterator[*comparableTraceResult]
	seen map[string]bool
}

func (sti *sortedTraceIterator) Next() (model.TraceResult, bool) {
	for sti.Iterator.Next() {
		result := sti.Iterator.Val().result
		if !sti.seen[result.TID] {
			sti.seen[result.TID] = true
			return result, true
		}
	}
	return model.TraceResult{}, false
}

// sortableTraceResultsFromIterator adapts a TraceResult iterator to sort.Iterator.
type sortableTraceResultsFromIterator struct {
	resultIter  iter.Iterator[model.TraceResult]
	current     *comparableTraceResult
	sortTagSpec logical.TagSpec
	sortByTime  bool
}

func (stri *sortableTraceResultsFromIterator) Next() bool {
	result, hasNext := stri.resultIter.Next()
	if !hasNext {
		return false
	}

	var err error
	stri.current, err = newComparableTraceResult(result, stri.sortByTime, stri.sortTagSpec)
	return err == nil
}

func (stri *sortableTraceResultsFromIterator) Val() *comparableTraceResult {
	return stri.current
}

func (stri *sortableTraceResultsFromIterator) Close() error {
	return nil
}

// traceLimitIterator implements iter.Iterator[model.TraceResult] by applying
// offset and limit to the number of trace results (not spans within results).
type traceLimitIterator struct {
	sourceIterator iter.Iterator[model.TraceResult]
	offset         int
	limit          int
	currentIndex   int
	returned       int
}

func (tli *traceLimitIterator) Next() (model.TraceResult, bool) {
	// Skip until we reach the offset
	for tli.currentIndex < tli.offset {
		if _, hasNext := tli.sourceIterator.Next(); !hasNext {
			return model.TraceResult{}, false
		}
		tli.currentIndex++
	}

	// Check if we've already returned enough results
	if tli.returned >= tli.limit {
		return model.TraceResult{}, false
	}

	// Get the next result
	result, hasNext := tli.sourceIterator.Next()
	if !hasNext {
		return model.TraceResult{}, false
	}

	tli.currentIndex++
	tli.returned++
	return result, true
}
