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

	"go.uber.org/multierr"

	commonv1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/common/v1"
	modelv1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/model/v1"
	tracev1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/trace/v1"
	"github.com/apache/skywalking-banyandb/pkg/iter"
	"github.com/apache/skywalking-banyandb/pkg/iter/sort"
	"github.com/apache/skywalking-banyandb/pkg/query/executor"
	"github.com/apache/skywalking-banyandb/pkg/query/logical"
	"github.com/apache/skywalking-banyandb/pkg/query/model"
)

var _ logical.UnresolvedPlan = (*unresolvedTraceMerger)(nil)

type unresolvedTraceMerger struct {
	criteria      *tracev1.QueryRequest
	metadata      []*commonv1.Metadata
	ecc           []executor.TraceExecutionContext
	tagProjection [][]*logical.Tag
}

func (utm *unresolvedTraceMerger) Analyze(s logical.Schema) (logical.Plan, error) {
	ss := s.Children()
	if len(ss) != len(utm.metadata) {
		return nil, fmt.Errorf("number of schemas %d not equal to metadata count %d", len(ss), len(utm.metadata))
	}
	if len(utm.tagProjection) > 0 {
		projectionTagRefs, err := s.CreateTagRef(utm.tagProjection...)
		if err != nil {
			return nil, err
		}
		s = s.ProjTags(projectionTagRefs...)
	}
	mp := &traceMergePlan{
		s: s,
	}

	for i := range utm.metadata {
		subPlan := parseTraceTags(utm.criteria, utm.metadata[i], utm.ecc[i], utm.tagProjection, "", "")
		sp, err := subPlan.Analyze(ss[i])
		if err != nil {
			return nil, err
		}
		mp.subPlans = append(mp.subPlans, sp)
	}
	if utm.criteria.OrderBy == nil {
		mp.sortByTime = true
		return mp, nil
	}
	if utm.criteria.OrderBy.IndexRuleName == "" {
		mp.sortByTime = true
		if utm.criteria.OrderBy.Sort == modelv1.Sort_SORT_DESC {
			mp.desc = true
		}
		return mp, nil
	}

	ok, indexRule := s.IndexRuleDefined(utm.criteria.OrderBy.IndexRuleName)
	if !ok {
		return nil, fmt.Errorf("index rule %s not found", utm.criteria.OrderBy.IndexRuleName)
	}
	if len(indexRule.Tags) != 1 {
		return nil, fmt.Errorf("index rule %s should have one tag", utm.criteria.OrderBy.IndexRuleName)
	}
	sortTagSpec := s.FindTagSpecByName(indexRule.Tags[0])
	if sortTagSpec == nil {
		return nil, fmt.Errorf("tag %s not found", indexRule.Tags[0])
	}
	mp.sortTagSpec = *sortTagSpec
	if utm.criteria.OrderBy.Sort == modelv1.Sort_SORT_DESC {
		mp.desc = true
	}
	return mp, nil
}

// traceMergePlan implements merge plan for multiple trace data sources.
var (
	_ logical.Plan             = (*traceMergePlan)(nil)
	_ executor.TraceExecutable = (*traceMergePlan)(nil)
)

type traceMergePlan struct {
	s           logical.Schema
	subPlans    []logical.Plan
	sortTagSpec logical.TagSpec
	sortByTime  bool
	desc        bool
}

func (tmp *traceMergePlan) Close() {
	for _, p := range tmp.subPlans {
		p.(executor.TraceExecutable).Close()
	}
}

func (tmp *traceMergePlan) Execute(ctx context.Context) (iter.Iterator[model.TraceResult], error) {
	var allErr error
	var see []sort.Iterator[*comparableTraceResult]

	for _, sp := range tmp.subPlans {
		resultIter, err := sp.(executor.TraceExecutable).Execute(ctx)
		if err != nil {
			allErr = multierr.Append(allErr, err)
			continue
		}
		iter := &sortableTraceResultsFromIterator{
			resultIter:  resultIter,
			sortTagSpec: tmp.sortTagSpec,
			sortByTime:  tmp.sortByTime,
		}
		see = append(see, iter)
	}

	if allErr != nil {
		return iter.Empty[model.TraceResult](), allErr
	}

	sortedIter := sort.NewItemIter(see, tmp.desc)
	return &sortedTraceIterator{
		Iterator: sortedIter,
		seen:     make(map[string]bool),
	}, nil
}

func (tmp *traceMergePlan) Children() []logical.Plan {
	return tmp.subPlans
}

func (tmp *traceMergePlan) Schema() logical.Schema {
	return tmp.s
}

func (tmp *traceMergePlan) String() string {
	return fmt.Sprintf("TraceMergePlan: subPlans=%d, sortByTime=%t, desc=%t",
		len(tmp.subPlans), tmp.sortByTime, tmp.desc)
}
