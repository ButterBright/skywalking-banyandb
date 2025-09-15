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
	"time"

	"go.uber.org/multierr"
	"google.golang.org/protobuf/proto"

	"github.com/apache/skywalking-banyandb/api/data"
	modelv1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/model/v1"
	tracev1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/trace/v1"
	"github.com/apache/skywalking-banyandb/pkg/bus"
	"github.com/apache/skywalking-banyandb/pkg/convert"
	"github.com/apache/skywalking-banyandb/pkg/iter"
	"github.com/apache/skywalking-banyandb/pkg/iter/sort"
	"github.com/apache/skywalking-banyandb/pkg/logger"
	"github.com/apache/skywalking-banyandb/pkg/query"
	"github.com/apache/skywalking-banyandb/pkg/query/executor"
	"github.com/apache/skywalking-banyandb/pkg/query/logical"
	"github.com/apache/skywalking-banyandb/pkg/query/model"
)

const defaultTraceQueryTimeout = 30 * time.Second

var _ logical.UnresolvedPlan = (*unresolvedTraceDistributed)(nil)

type unresolvedTraceDistributed struct {
	criteria *tracev1.QueryRequest
}

func newUnresolvedTraceDistributed(criteria *tracev1.QueryRequest) logical.UnresolvedPlan {
	return &unresolvedTraceDistributed{criteria: criteria}
}

func (utd *unresolvedTraceDistributed) Analyze(s logical.Schema) (logical.Plan, error) {
	tagProjection := convertStringProjectionToTags(utd.criteria.GetTagProjection())
	if len(tagProjection) > 0 {
		var err error
		projTagsRefs, err := s.CreateTagRef(tagProjection...)
		if err != nil {
			return nil, err
		}
		s = s.ProjTags(projTagsRefs...)
	}

	limit := utd.criteria.GetLimit()
	if limit == 0 {
		limit = defaultLimit
	}
	temp := &tracev1.QueryRequest{
		TagProjection: utd.criteria.TagProjection,
		Name:          utd.criteria.Name,
		Groups:        utd.criteria.Groups,
		Criteria:      utd.criteria.Criteria,
		Limit:         limit + utd.criteria.Offset,
		OrderBy:       utd.criteria.OrderBy,
	}

	if utd.criteria.OrderBy == nil {
		return &traceDistributedPlan{
			queryTemplate: temp,
			s:             s,
			sortByTime:    true,
		}, nil
	}
	if utd.criteria.OrderBy.IndexRuleName == "" {
		result := &traceDistributedPlan{
			queryTemplate: temp,
			s:             s,
			sortByTime:    true,
		}
		if utd.criteria.OrderBy.Sort == modelv1.Sort_SORT_DESC {
			result.desc = true
		}
		return result, nil
	}

	ok, indexRule := s.IndexRuleDefined(utd.criteria.OrderBy.IndexRuleName)
	if !ok {
		return nil, fmt.Errorf("index rule %s not found", utd.criteria.OrderBy.IndexRuleName)
	}
	if len(indexRule.Tags) != 1 {
		return nil, fmt.Errorf("index rule %s should have only one tag", utd.criteria.OrderBy.IndexRuleName)
	}
	sortTagSpec := s.FindTagSpecByName(indexRule.Tags[0])
	if sortTagSpec == nil {
		return nil, fmt.Errorf("tag %s not found", indexRule.Tags[0])
	}

	result := &traceDistributedPlan{
		queryTemplate: temp,
		s:             s,
		sortByTime:    false,
		sortTagSpec:   *sortTagSpec,
	}
	if utd.criteria.OrderBy.Sort == modelv1.Sort_SORT_DESC {
		result.desc = true
	}
	return result, nil
}

// traceDistributedPlan implements the main distributed query plan for traces.
var _ executor.TraceExecutable = (*traceDistributedPlan)(nil)

type traceDistributedPlan struct {
	s             logical.Schema
	queryTemplate *tracev1.QueryRequest
	sortTagSpec   logical.TagSpec
	sortByTime    bool
	desc          bool
	maxTraceSize  uint32
}

func (t *traceDistributedPlan) Close() {}

func (t *traceDistributedPlan) Execute(ctx context.Context) (iter.Iterator[model.TraceResult], error) {
	dctx := executor.FromDistributedExecutionContext(ctx)
	queryRequest := proto.Clone(t.queryTemplate).(*tracev1.QueryRequest)
	queryRequest.TimeRange = dctx.TimeRange()
	if t.maxTraceSize > 0 {
		queryRequest.Limit = t.maxTraceSize
	}

	tracer := query.GetTracer(ctx)
	var span *query.Span
	if tracer != nil {
		span, _ = tracer.StartSpan(ctx, "distributed-trace-client")
		queryRequest.Trace = true
		span.Tag("request", convert.BytesToString(logger.Proto(queryRequest)))
		span.Tag("node_selectors", fmt.Sprintf("%v", dctx.NodeSelectors()))
		span.Tag("time_range", dctx.TimeRange().String())
		defer func() {
			span.Stop()
		}()
	}

	ff, err := dctx.Broadcast(defaultTraceQueryTimeout, data.TopicTraceQuery,
		bus.NewMessageWithNodeSelectors(bus.MessageID(dctx.TimeRange().Begin.Nanos), dctx.NodeSelectors(), dctx.TimeRange(), queryRequest))
	if err != nil {
		return iter.Empty[model.TraceResult](), err
	}

	var allErr error
	var see []sort.Iterator[*comparableTraceResult]
	for _, f := range ff {
		if m, getErr := f.Get(); getErr != nil {
			allErr = multierr.Append(allErr, getErr)
		} else {
			d := m.Data()
			if d == nil {
				continue
			}
			resp := d.(*tracev1.QueryResponse)
			if span != nil {
				span.AddSubTrace(resp.TraceQueryResult)
			}

			// Convert traces to trace results and aggregate by trace ID
			traceMap := make(map[string]*model.TraceResult)
			for _, trace := range resp.Traces {
				traceID := extractTraceID(trace)
				if traceID == "" {
					continue
				}

				// Get or create trace result for this trace ID
				traceResult, exists := traceMap[traceID]
				if !exists {
					traceResult = &model.TraceResult{
						TID:   traceID,
						Spans: make([][]byte, 0),
						Tags:  make([]model.Tag, 0),
					}
					traceMap[traceID] = traceResult
				}

				// Aggregate all tags from all spans in this trace
				tagMap := make(map[string]*model.Tag)

				// First, collect existing tags
				for _, tag := range traceResult.Tags {
					tagMap[tag.Name] = &model.Tag{
						Name:   tag.Name,
						Values: append([]*modelv1.TagValue{}, tag.Values...),
					}
				}

				// Add spans and their tags to the trace result
				for _, span := range trace.Spans {
					traceResult.Spans = append(traceResult.Spans, span.Span)

					// Extract tags from this span
					for _, tag := range span.Tags {
						tagName := tag.Key
						if tagMap[tagName] == nil {
							tagMap[tagName] = &model.Tag{
								Name:   tagName,
								Values: make([]*modelv1.TagValue, 0),
							}
						}
						tagMap[tagName].Values = append(tagMap[tagName].Values, tag.Value)
					}
				}

				// Update the tags in the trace result
				traceResult.Tags = make([]model.Tag, 0, len(tagMap))
				for _, tag := range tagMap {
					traceResult.Tags = append(traceResult.Tags, *tag)
				}
			}

			// Convert map to slice
			var traceResults []model.TraceResult
			for _, traceResult := range traceMap {
				traceResults = append(traceResults, *traceResult)
			}

			see = append(see,
				newSortableTraceResults(traceResults, t.sortByTime, t.sortTagSpec))
		}
	}

	iter := sort.NewItemIter(see, t.desc)
	return &sortedTraceIterator{
		Iterator: iter,
		seen:     make(map[string]bool),
	}, allErr
}

func (t *traceDistributedPlan) String() string {
	return fmt.Sprintf("distributed-trace:%s", t.queryTemplate.String())
}

func (t *traceDistributedPlan) Children() []logical.Plan {
	return []logical.Plan{}
}

func (t *traceDistributedPlan) Schema() logical.Schema {
	return t.s
}

func (t *traceDistributedPlan) Limit(maxVal int) {
	t.maxTraceSize = uint32(maxVal)
}

func extractTraceID(trace *tracev1.Trace) string {
	if len(trace.Spans) > 0 {
		span := trace.Spans[0]
		if len(span.Tags) > 0 {
			for _, tag := range span.Tags {
				if tag.Key == "trace_id" && tag.Value != nil {
					if strVal := tag.Value.GetStr(); strVal != nil {
						return strVal.Value
					}
				}
			}
		}
	}
	return ""
}
