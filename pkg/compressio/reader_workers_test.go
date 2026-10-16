// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package compressio

import (
	"bytes"
	"compress/flate"
	"fmt"
	"io"
	"runtime"
	"testing"
)

type unreadableWorkerLimitInput struct{}

func (unreadableWorkerLimitInput) Read([]byte) (int, error) { panic("invalid limit consumed input") }
func (unreadableWorkerLimitInput) Close() error             { return nil }

func TestReaderWorkerLimitRejectsInvalid(t *testing.T) {
	for _, limit := range []int{-1, 0} {
		if reader, err := NewReaderWithMaxWorkers(unreadableWorkerLimitInput{}, nil, limit); err == nil || reader != nil {
			t.Fatalf("limit %d produced reader %v, error %v", limit, reader, err)
		}
	}
}

func TestReaderWorkerLimit(t *testing.T) {
	data := initTest(t, 2<<20)
	key := []byte("deterministic-worker-limit-test-key")
	var encoded bytes.Buffer
	writer, err := NewWriter(&encoded, key, 64<<10, flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{1, 2, 4} {
		t.Run(fmt.Sprintf("workers=%d", limit), func(t *testing.T) {
			reader, err := NewReaderWithMaxWorkers(io.NopCloser(bytes.NewReader(encoded.Bytes())), key, limit)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := len(reader.workers), min(limit, 2*runtime.GOMAXPROCS(0)); got != want {
				t.Fatalf("workers=%d, want %d", got, want)
			}
			actual, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(actual, data) {
				t.Fatal("bounded reader changed data")
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
		})
		for _, kind := range []string{"corrupt", "truncated"} {
			t.Run(fmt.Sprintf("workers=%d/%s", limit, kind), func(t *testing.T) {
				bad := append([]byte(nil), encoded.Bytes()...)
				if kind == "corrupt" {
					bad[len(bad)-1] ^= 1
				} else {
					bad = bad[:len(bad)-7]
				}
				reader, err := NewReaderWithMaxWorkers(io.NopCloser(bytes.NewReader(bad)), key, limit)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := io.ReadAll(reader); err == nil {
					t.Fatal("invalid stream accepted")
				}
				reader.Close()
			})
		}
	}
}
