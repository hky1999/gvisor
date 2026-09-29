// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package tmpfs

import (
	"archive/tar"
	"bytes"
	"context"
	"testing"
)

// mergeTarForTest drives mergeFromTar with the tar-reading callbacks wired
// to in-memory regular file contents.
func mergeTarForTest(t *testing.T, fs *filesystem, entries []tar.Header, contents map[string]string) error {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i := range entries {
		hdr := entries[i]
		if hdr.Typeflag == tar.TypeReg {
			data := contents[hdr.Name]
			hdr.Size = int64(len(data))
			if err := tw.WriteHeader(&hdr); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(data)); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := tw.WriteHeader(&hdr); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	cb := &memReaderCallbacks{contents: contents}
	return fs.mergeFromTar(context.Background(), tar.NewReader(bytes.NewReader(buf.Bytes())), cb)
}

// A conflicting-type entry (Fifo) whose name already exists as a regular
// file in the merged tree must be accepted (metadata refresh), not fail
// the whole restore.
func TestMergeToleratesConflictingTypeOnRegularFile(t *testing.T) {
	fs, _, err := newFSForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	reg := tar.Header{Name: "./a/pyc.tmp", Typeflag: tar.TypeReg, Mode: 0o644}
	if err := mergeTarForTest(t, fs, []tar.Header{reg}, map[string]string{"./a/pyc.tmp": "data"}); err != nil {
		t.Fatal(err)
	}
	conflict := tar.Header{Name: "./a/pyc.tmp", Typeflag: tar.TypeFifo, Mode: 0o644}
	if err := mergeTarForTest(t, fs, []tar.Header{conflict}, nil); err != nil {
		t.Fatalf("conflicting-type merge should be tolerated: %v", err)
	}
}
