// Licensed to Apache Software Foundation (ASF) under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Apache Software Foundation (ASF) licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
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

	"github.com/pkg/errors"

	commonv1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/common/v1"
	databasev1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/database/v1"
	"github.com/apache/skywalking-banyandb/api/validate"
	"github.com/apache/skywalking-banyandb/banyand/internal/storage"
	"github.com/apache/skywalking-banyandb/banyand/internal/wqueue"
	"github.com/apache/skywalking-banyandb/banyand/liaison/grpc"
	"github.com/apache/skywalking-banyandb/banyand/metadata"
	"github.com/apache/skywalking-banyandb/banyand/metadata/schema"
	"github.com/apache/skywalking-banyandb/banyand/observability"
	"github.com/apache/skywalking-banyandb/banyand/protector"
	"github.com/apache/skywalking-banyandb/pkg/logger"
	resourceSchema "github.com/apache/skywalking-banyandb/pkg/schema"
	"github.com/apache/skywalking-banyandb/pkg/timestamp"
)

var (
	metadataScope = traceScope.SubScope("metadata")
	// ErrTraceNotFound is returned when a trace is not found.
	ErrTraceNotFound = errors.New("trace not found")
)

// SchemaService allows querying schema information.
type SchemaService interface {
	Query
	Close()
}

type schemaRepo struct {
	resourceSchema.Repository
	l        *logger.Logger
	metadata metadata.Repo
	path     string
}

func newSchemaRepo(path string, svc *standalone, nodeLabels map[string]string) schemaRepo {
	sr := schemaRepo{
		l:        svc.l,
		path:     path,
		metadata: svc.metadata,
		Repository: resourceSchema.NewRepository(
			svc.metadata,
			svc.l,
			newSupplier(path, svc, nodeLabels),
			resourceSchema.NewMetrics(svc.omr.With(metadataScope)),
		),
	}
	sr.start()
	return sr
}

func newLiaisonSchemaRepo(path string, svc *liaison, traceDataNodeRegistry grpc.NodeRegistry) schemaRepo {
	sr := schemaRepo{
		l:        svc.l,
		path:     path,
		metadata: svc.metadata,
		Repository: resourceSchema.NewRepository(
			svc.metadata,
			svc.l,
			newQueueSupplier(path, svc, traceDataNodeRegistry),
			resourceSchema.NewMetrics(svc.omr.With(metadataScope)),
		),
	}
	sr.start()
	return sr
}

func (sr *schemaRepo) start() {
	sr.l.Info().Str("path", sr.path).Msg("starting trace metadata repository")
}

func (sr *schemaRepo) Trace(metadata *commonv1.Metadata) (*trace, bool) {
	sm, ok := sr.Repository.LoadResource(metadata)
	if !ok {
		return nil, false
	}
	t, ok := sm.Delegated().(*trace)
	return t, ok
}

func (sr *schemaRepo) GetRemovalSegmentsTimeRange(_ string) *timestamp.TimeRange {
	panic("not implemented")
}

func (sr *schemaRepo) OnInit(kinds []schema.Kind) (bool, []int64) {
	if len(kinds) != 4 {
		logger.Panicf("invalid kinds: %v", kinds)
		return false, nil
	}
	_, revs := sr.Repository.Init(schema.KindStream)
	return true, revs
}

func (sr *schemaRepo) OnAddOrUpdate(metadata schema.Metadata) {
	switch metadata.Kind {
	case schema.KindGroup:
		g := metadata.Spec.(*commonv1.Group)
		if g.Catalog != commonv1.Catalog_CATALOG_STREAM {
			return
		}
		if err := validate.GroupForStreamOrMeasure(g); err != nil {
			sr.l.Warn().Err(err).Msg("group is ignored")
			return
		}
		sr.SendMetadataEvent(resourceSchema.MetadataEvent{
			Typ:      resourceSchema.EventAddOrUpdate,
			Kind:     resourceSchema.EventKindGroup,
			Metadata: g,
		})
	case schema.KindStream:
		if err := validate.Stream(metadata.Spec.(*databasev1.Stream)); err != nil {
			sr.l.Warn().Err(err).Msg("stream is ignored")
			return
		}
		sr.SendMetadataEvent(resourceSchema.MetadataEvent{
			Typ:      resourceSchema.EventAddOrUpdate,
			Kind:     resourceSchema.EventKindResource,
			Metadata: metadata.Spec.(*databasev1.Stream),
		})
	case schema.KindIndexRuleBinding:
		if irb, ok := metadata.Spec.(*databasev1.IndexRuleBinding); ok {
			if err := validate.IndexRuleBinding(irb); err != nil {
				sr.l.Warn().Err(err).Msg("index rule binding is ignored")
				return
			}
			if irb.GetSubject().Catalog == commonv1.Catalog_CATALOG_STREAM {
				sr.SendMetadataEvent(resourceSchema.MetadataEvent{
					Typ:      resourceSchema.EventAddOrUpdate,
					Kind:     resourceSchema.EventKindIndexRuleBinding,
					Metadata: irb,
				})
			}
		}
	case schema.KindIndexRule:
		if ir, ok := metadata.Spec.(*databasev1.IndexRule); ok {
			if err := validate.IndexRule(metadata.Spec.(*databasev1.IndexRule)); err != nil {
				sr.l.Warn().Err(err).Msg("index rule is ignored")
				return
			}
			sr.SendMetadataEvent(resourceSchema.MetadataEvent{
				Typ:      resourceSchema.EventAddOrUpdate,
				Kind:     resourceSchema.EventKindIndexRule,
				Metadata: ir,
			})
		}
	default:
	}
}

func (sr *schemaRepo) OnDelete(metadata schema.Metadata) {
	switch metadata.Kind {
	case schema.KindGroup:
		g := metadata.Spec.(*commonv1.Group)
		if g.Catalog != commonv1.Catalog_CATALOG_STREAM {
			return
		}
		sr.SendMetadataEvent(resourceSchema.MetadataEvent{
			Typ:      resourceSchema.EventDelete,
			Kind:     resourceSchema.EventKindGroup,
			Metadata: g,
		})
	case schema.KindStream:
		sr.SendMetadataEvent(resourceSchema.MetadataEvent{
			Typ:      resourceSchema.EventDelete,
			Kind:     resourceSchema.EventKindResource,
			Metadata: metadata.Spec.(*databasev1.Stream),
		})
	case schema.KindIndexRuleBinding:
		if binding, ok := metadata.Spec.(*databasev1.IndexRuleBinding); ok {
			if binding.GetSubject().Catalog == commonv1.Catalog_CATALOG_MEASURE {
				sr.SendMetadataEvent(resourceSchema.MetadataEvent{
					Typ:      resourceSchema.EventDelete,
					Kind:     resourceSchema.EventKindIndexRuleBinding,
					Metadata: metadata.Spec.(*databasev1.IndexRuleBinding),
				})
			}
		}
	case schema.KindIndexRule:
		if rule, ok := metadata.Spec.(*databasev1.IndexRule); ok {
			sr.SendMetadataEvent(resourceSchema.MetadataEvent{
				Typ:      resourceSchema.EventDelete,
				Kind:     resourceSchema.EventKindIndexRule,
				Metadata: rule,
			})
		}
	default:
	}
}

func (sr *schemaRepo) loadTrace(metadata *commonv1.Metadata) (*trace, bool) {
	r, ok := sr.LoadResource(metadata)
	if !ok {
		return nil, false
	}
	s, ok := r.Delegated().(*trace)
	return s, ok
}

func (sr *schemaRepo) loadTSDB(groupName string) (storage.TSDB[*tsTable, option], error) {
	g, ok := sr.LoadGroup(groupName)
	if !ok {
		return nil, fmt.Errorf("group %s not found", groupName)
	}
	db := g.SupplyTSDB()
	if db == nil {
		return nil, fmt.Errorf("tsdb for group %s not found", groupName)
	}
	return db.(storage.TSDB[*tsTable, option]), nil
}

func (sr *schemaRepo) loadQueue(groupName string) (*wqueue.Queue[*tsTable, option], error) {
	g, ok := sr.LoadGroup(groupName)
	if !ok {
		return nil, fmt.Errorf("group %s not found", groupName)
	}
	db := g.SupplyTSDB()
	if db == nil {
		return nil, fmt.Errorf("queue for group %s not found", groupName)
	}
	return db.(*wqueue.Queue[*tsTable, option]), nil
}

var _ resourceSchema.ResourceSupplier = (*supplier)(nil)

// supplier is the supplier for standalone service.
type supplier struct {
	metadata   metadata.Repo
	omr        observability.MetricsRegistry
	pm         protector.Memory
	l          *logger.Logger
	schemaRepo *schemaRepo
	nodeLabels map[string]string
	path       string
	option     option
}

func newSupplier(path string, svc *standalone, nodeLabels map[string]string) *supplier {
	return &supplier{
		metadata:   svc.metadata,
		omr:        svc.omr,
		pm:         svc.pm,
		l:          svc.l,
		nodeLabels: nodeLabels,
		path:       path,
		option:     svc.option,
	}
}

func (s *supplier) OpenResource(spec resourceSchema.Resource) (resourceSchema.IndexListener, error) {
	traceSchema := spec.Schema().(*databasev1.Trace)
	return openTrace(traceSchema, s.l, s.pm, s.schemaRepo), nil
}

func (s *supplier) ResourceSchema(md *commonv1.Metadata) (resourceSchema.ResourceSchema, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.metadata.TraceRegistry().GetTrace(ctx, md)
}

func (s *supplier) OpenDB(_ *commonv1.Group) (resourceSchema.DB, error) {
	panic("not implemented")
}

// queueSupplier is the supplier for liaison service.
type queueSupplier struct {
	metadata              metadata.Repo
	omr                   observability.MetricsRegistry
	pm                    protector.Memory
	traceDataNodeRegistry grpc.NodeRegistry
	l                     *logger.Logger
	schemaRepo            *schemaRepo
	path                  string
	option                option
}

func newQueueSupplier(path string, svc *liaison, traceDataNodeRegistry grpc.NodeRegistry) *queueSupplier {
	return &queueSupplier{
		metadata:              svc.metadata,
		omr:                   svc.omr,
		pm:                    svc.pm,
		traceDataNodeRegistry: traceDataNodeRegistry,
		l:                     svc.l,
		path:                  path,
		option:                svc.option,
	}
}

func (qs *queueSupplier) OpenResource(spec resourceSchema.Resource) (resourceSchema.IndexListener, error) {
	traceSchema := spec.Schema().(*databasev1.Trace)
	return openTrace(traceSchema, qs.l, qs.pm, qs.schemaRepo), nil
}

func (qs *queueSupplier) ResourceSchema(md *commonv1.Metadata) (resourceSchema.ResourceSchema, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return qs.metadata.TraceRegistry().GetTrace(ctx, md)
}

func (qs *queueSupplier) OpenDB(_ *commonv1.Group) (resourceSchema.DB, error) {
	panic("not implemented")
}
