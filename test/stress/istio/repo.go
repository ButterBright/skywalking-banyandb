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

package istio

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	commonv1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/common/v1"
	databasev1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/database/v1"
	"github.com/apache/skywalking-banyandb/banyand/metadata/schema"
	"github.com/apache/skywalking-banyandb/pkg/logger"
)

//go:embed testdata/*
var store embed.FS

func extractData() string {
	// Get the system's temporary directory
	tmpDir := os.TempDir()

	// Create a subdirectory called "tmp" in the temporary directory
	tmpSubDir := filepath.Join(tmpDir, "testdata")
	target := filepath.Join(tmpSubDir, "access.log")
	if _, err := os.Stat(target); err == nil {
		absPath, err := filepath.Abs(target)
		if err != nil {
			fmt.Printf("Error getting absolute path: %v\n", err)
			os.Exit(1)
		}
		return absPath
	}
	err := os.MkdirAll(tmpSubDir, 0o755)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating tmp directory: %v\n", err)
		os.Exit(1)
	}
	var data []byte
	if data, err = store.ReadFile("testdata/access.tar.bz2"); err != nil {
		fmt.Printf("Error reading file: %v\n", err)
		os.Exit(1)
	}
	filePath, err := extractTarGz(data, tmpSubDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error extracting file: %v\n", err)
		os.Exit(1)
	}
	return filePath
}

func extractTarGz(src []byte, dest string) (string, error) {
	bzReader := bzip2.NewReader(io.Reader(bytes.NewReader(src)))
	tarReader := tar.NewReader(bzReader)

	for {
		header, err := tarReader.Next()

		switch {
		case errors.Is(err, io.EOF):
			return "", fmt.Errorf("no file found in tar archive")
		case err != nil:
			return "", err
		}

		// the path is already checked. https://snyk.io/research/zip-slip-vulnerability#go
		// nolint:gosec
		target := filepath.Join(dest, header.Name)

		if _, err := os.Stat(target); err == nil {
			fmt.Printf("Skipping existing file: %s\n", target)
			absPath, err := filepath.Abs(target)
			if err != nil {
				return "", err
			}
			return absPath, nil
		}

		if header.Typeflag == tar.TypeReg {
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}

			file, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
			if err != nil {
				return "", err
			}
			defer file.Close()

			for {
				_, errCopy := io.CopyN(file, tarReader, 1024)
				if errCopy != nil {
					if errors.Is(errCopy, io.EOF) {
						break
					}
					return "", errCopy
				}
			}

			absPath, err := filepath.Abs(target)
			if err != nil {
				return "", err
			}
			return absPath, nil
		}
	}
}

type preloadService struct {
	registry schema.Registry
	name     string
}

func (p *preloadService) Name() string {
	return "preload-" + p.name
}

func (p *preloadService) PreRun(ctx context.Context) error {
	e := p.registry
	if err := loadSchema(ctx, groupDir, &commonv1.Group{}, func(ctx context.Context, group *commonv1.Group) error {
		return e.CreateGroup(ctx, group)
	}); err != nil {
		return errors.WithStack(err)
	}
	if err := loadSchema(ctx, measureDir, &databasev1.Measure{}, func(ctx context.Context, measure *databasev1.Measure) error {
		_, innerErr := e.CreateMeasure(ctx, measure)
		if innerErr != nil {
			logger.Errorf("failed to create measure %s: %v", measure.Metadata.Name, innerErr)
		}
		return nil
	}); err != nil {
		return errors.WithStack(err)
	}
	if err := loadSchema(ctx, indexRuleDir, &databasev1.IndexRule{}, func(ctx context.Context, indexRule *databasev1.IndexRule) error {
		return e.CreateIndexRule(ctx, indexRule)
	}); err != nil {
		return errors.WithStack(err)
	}
	if err := loadSchema(ctx, indexRuleBindingDir, &databasev1.IndexRuleBinding{}, func(ctx context.Context, indexRuleBinding *databasev1.IndexRuleBinding) error {
		return e.CreateIndexRuleBinding(ctx, indexRuleBinding)
	}); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func (p *preloadService) SetRegistry(registry schema.Registry) {
	p.registry = registry
}

const (
	groupDir            = "testdata/groups"
	measureDir          = "testdata/measures"
	indexRuleDir        = "testdata/index-rules"
	indexRuleBindingDir = "testdata/index-rule-bindings"
)

func loadSchema[T proto.Message](ctx context.Context, dir string, resource T, loadFn func(ctx context.Context, resource T) error) error {
	entries, err := store.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		yamlData, err := store.ReadFile(path.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		var dataArray []map[interface{}]interface{}
		if err := yaml.Unmarshal(yamlData, &dataArray); err != nil {
			return err
		}
		for _, data := range dataArray {
			convertedData := convertMap(data)
			jsonData, err := json.Marshal(convertedData)
			if err != nil {
				return err
			}
			if err := protojson.Unmarshal(jsonData, resource); err != nil {
				return err
			}
			if err := loadFn(ctx, resource); err != nil {
				if status.Code(err) == codes.AlreadyExists {
					continue
				}
				return err
			}
		}
	}
	return nil
}

func convertMap(data map[interface{}]interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	for key, value := range data {
		strKey := fmt.Sprintf("%v", key)

		switch v := value.(type) {
		case map[interface{}]interface{}:
			result[strKey] = convertMap(v)
		case []interface{}:
			result[strKey] = convertSlice(v)
		default:
			result[strKey] = v
		}
	}

	return result
}

func convertSlice(data []interface{}) []interface{} {
	result := make([]interface{}, len(data))

	for i, value := range data {
		switch v := value.(type) {
		case map[interface{}]interface{}:
			result[i] = convertMap(v)
		case []interface{}:
			result[i] = convertSlice(v)
		default:
			result[i] = v
		}
	}

	return result
}
