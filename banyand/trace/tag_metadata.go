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
	"fmt"

	"github.com/apache/skywalking-banyandb/pkg/convert"
	"github.com/apache/skywalking-banyandb/pkg/encoding"
	pbv1 "github.com/apache/skywalking-banyandb/pkg/pb/v1"
	"github.com/apache/skywalking-banyandb/pkg/pool"
)

type tagMetadata struct {
	name string
	min  []byte
	max  []byte
	dataBlock
	valueType   pbv1.ValueType
	filterBlock dataBlock
}

func (tm *tagMetadata) reset() {
	tm.name = ""
	tm.valueType = 0
	tm.dataBlock.reset()
	tm.min = nil
	tm.max = nil
	tm.filterBlock.reset()
}

func (tm *tagMetadata) copyFrom(src *tagMetadata) {
	tm.name = src.name
	tm.valueType = src.valueType
	tm.dataBlock.copyFrom(&src.dataBlock)
	tm.min = append(tm.min[:0], src.min...)
	tm.max = append(tm.max[:0], src.max...)
	tm.filterBlock.copyFrom(&src.filterBlock)
}

func (tm *tagMetadata) marshal(dst []byte) []byte {
	dst = encoding.EncodeBytes(dst, convert.StringToBytes(tm.name))
	dst = append(dst, byte(tm.valueType))
	dst = tm.dataBlock.marshal(dst)
	dst = encoding.EncodeBytes(dst, tm.min)
	dst = encoding.EncodeBytes(dst, tm.max)
	dst = tm.filterBlock.marshal(dst)
	return dst
}

func (tm *tagMetadata) unmarshal(src []byte) ([]byte, error) {
	src, nameBytes, err := encoding.DecodeBytes(src)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal tagMetadata.name: %w", err)
	}
	tm.name = string(nameBytes)
	if len(src) < 1 {
		return nil, fmt.Errorf("cannot unmarshal tagMetadata.valueType: src is too short")
	}
	tm.valueType = pbv1.ValueType(src[0])
	src = src[1:]
	src = tm.dataBlock.unmarshal(src)
	src, tm.min, err = encoding.DecodeBytes(src)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal tagMetadata.min: %w", err)
	}
	src, tm.max, err = encoding.DecodeBytes(src)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal tagMetadata.max: %w", err)
	}
	src = tm.filterBlock.unmarshal(src)
	return src, nil
}

func generateTagMetadata() *tagMetadata {
	v := tagMetadataPool.Get()
	if v == nil {
		v = &tagMetadata{}
	}
	return v
}

func releaseTagMetadata(tm *tagMetadata) {
	tm.reset()
	tagMetadataPool.Put(tm)
}

var tagMetadataPool = pool.Register[*tagMetadata]("trace-tagMetadata")
