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

package pgalloc

import (
	"bytes"
	"os"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

func TestLazyFilestoreDelta(t *testing.T) {
	base, err := os.CreateTemp(t.TempDir(), "base")
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	local, err := os.CreateTemp(t.TempDir(), "local")
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()

	size := int64(5 * hostarch.PageSize)
	baseData := bytes.Repeat([]byte{0x41}, int(size))
	if _, err := base.Write(baseData); err != nil {
		t.Fatal(err)
	}
	if err := local.Truncate(size); err != nil {
		t.Fatal(err)
	}
	l := &lazyFilestore{base: base, size: size, pages: make(map[uint64]lazyFilestorePage)}
	f := &MemoryFile{file: local, lazy: l}
	for _, page := range []uint64{0, 1, 4} {
		fr := memmap.FileRange{Start: page * hostarch.PageSize, End: (page + 1) * hostarch.PageSize}
		if fd, err := f.DataFD(fr); err != nil || fd != int(local.Fd()) {
			t.Fatalf("DataFD(%v) = %d, %v", fr, fd, err)
		}
	}
	// Clear a page whose baseline was never loaded. The next fault may
	// use this range, but must never fill it from the old image.
	l.markCleared(memmap.FileRange{Start: 2 * hostarch.PageSize, End: 3 * hostarch.PageSize})
	if _, err := f.DataFD(memmap.FileRange{Start: 2 * hostarch.PageSize, End: 3 * hostarch.PageSize}); err != nil {
		t.Fatal(err)
	}
	if _, err := local.WriteAt([]byte{0x7a}, hostarch.PageSize+19); err != nil {
		t.Fatal(err)
	}
	if _, err := local.WriteAt([]byte{0x88}, size-1); err != nil {
		t.Fatal(err)
	}

	delta, fp, err := l.snapshotDelta(local, size)
	if err != nil {
		t.Fatal(err)
	}
	extents := []checkpoint.FilestoreExtent{
		{Offset: hostarch.PageSize, Length: hostarch.PageSize},
		{Offset: 2 * hostarch.PageSize, Length: hostarch.PageSize, Zero: true},
		{Offset: 4 * hostarch.PageSize, Length: hostarch.PageSize},
	}
	if len(delta.Extents) != len(extents) {
		t.Fatalf("delta extents = %+v, want %+v", delta.Extents, extents)
	}
	for i, want := range extents {
		if delta.Extents[i] != want {
			t.Errorf("extent %d = %+v, want %+v", i, delta.Extents[i], want)
		}
	}
	if delta.BaseReadBytes != 3*hostarch.PageSize || delta.ScannedBytes != 4*hostarch.PageSize {
		t.Errorf("read %d bytes and scanned %d bytes", delta.BaseReadBytes, delta.ScannedBytes)
	}
	baseData[hostarch.PageSize+19] = 0x7a
	clear(baseData[2*hostarch.PageSize : 3*hostarch.PageSize])
	baseData[size-1] = 0x88
	wantFP, err := checkpoint.FingerprintReaderAt(bytes.NewReader(baseData), size)
	if err != nil || fp != wantFP {
		t.Errorf("complete fingerprint = %q, want %q (%v)", fp, wantFP, err)
	}
}

func TestDualFilestorePageSources(t *testing.T) {
	const size = 3 * hostarch.PageSize
	base, err := os.CreateTemp(t.TempDir(), "g0")
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	data := bytes.Repeat([]byte{0x47}, size)
	clear(data[:hostarch.PageSize])
	if _, err := base.Write(data); err != nil {
		t.Fatal(err)
	}
	local, err := os.CreateTemp(t.TempDir(), "upper")
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	if err := local.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f := &MemoryFile{file: local}
	l := &lazyFilestore{base: base, size: size, mapping: data, source: baselineFile{f}, pages: make(map[uint64]lazyFilestorePage)}
	f.lazy = l
	whole := memmap.FileRange{Start: 0, End: size}
	ro, run := f.ReadBacking(whole)
	if ro.FD() != int(base.Fd()) || run != whole {
		t.Fatalf("wrong initial source %v", run)
	}
	if _, err := ro.MapInternal(whole, hostarch.Write); err == nil {
		t.Fatal("writable baseline mapping")
	}
	buf := bytes.Repeat([]byte{0x95}, hostarch.PageSize)
	if err := f.OverwriteInheritedPage(hostarch.PageSize, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(buf))); err != nil {
		t.Fatal(err)
	}
	if l.baseReadBytes != 0 {
		t.Fatalf("overwrite read %d old bytes", l.baseReadBytes)
	}
	for i, file := range []memmap.File{ro, f, ro} {
		actual, run := f.ReadBacking(memmap.FileRange{Start: uint64(i) * hostarch.PageSize, End: size})
		if actual != file || run.Length() != hostarch.PageSize {
			t.Fatalf("page %d source %T range %v", i, actual, run)
		}
	}
	// Simulate a failed full-page write to an inherited zero page.
	if _, err := local.WriteAt([]byte("partial"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.DataFD(memmap.FileRange{Start: 0, End: hostarch.PageSize}); err != nil {
		t.Fatal(err)
	}
	if _, err := local.ReadAt(buf, 0); err != nil || !bytes.Equal(buf, data[:hostarch.PageSize]) {
		t.Fatal("stale upper bytes after retry", err)
	}
}
