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
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"sort"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/hostfd"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sync"
)

var zeroPageHash = sha256.Sum256(make([]byte, hostarch.PageSize))

type lazyFilestorePage struct {
	// inherited is set after filling the page from the base. Pages that
	// were allocated or decommitted locally do not inherit any old bytes.
	inherited bool
	hash      [sha256.Size]byte
}

type lazyFilestore struct {
	base    *os.File
	mapping []byte
	source  baselineFile
	size    int64

	// mu serializes filling and clearing. It is never held when acquiring
	// MemoryFile.mu or waiting on Sentry mappings.
	mu            sync.Mutex
	pages         map[uint64]lazyFilestorePage // key is a page-aligned byte offset
	baseReadBytes uint64
	baseHash      string
}

// UsesLazyFilestore reports whether the restored backing has a remote baseline.
func (f *MemoryFile) UsesLazyFilestore() bool {
	return f.lazy != nil
}

func (f *MemoryFile) ensureLazyRange(fr memmap.FileRange) error {
	l := f.lazy
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Calls with unaligned ranges are possible from internal file access.
	var buf [hostarch.PageSize]byte
	for offset := hostarch.PageRoundDown(fr.Start); offset < fr.End && offset < uint64(l.size); offset += hostarch.PageSize {
		if _, ready := l.pages[offset]; ready {
			continue
		}
		if _, err := l.base.ReadAt(buf[:], int64(offset)); err != nil {
			return fmt.Errorf("read filestore baseline at %d: %w", offset, err)
		}
		l.baseReadBytes += uint64(len(buf))
		hash := sha256.Sum256(buf[:])
		// Even an inherited zero page may cover a failed partial host write.
		// Publish the local location only after replacing every byte.
		if _, err := f.file.WriteAt(buf[:], int64(offset)); err != nil {
			return fmt.Errorf("fill local filestore at %d: %w", offset, err)
		}
		l.pages[offset] = lazyFilestorePage{inherited: true, hash: hash}
	}
	return nil
}

// baselineFile shares allocation references with its owning MemoryFile. Its
// immutable host mapping can outlive any individual guest translation.
type baselineFile struct{ *MemoryFile }

func (b *baselineFile) FD() int                                 { return int(b.lazy.base.Fd()) }
func (b *baselineFile) DataFD(fr memmap.FileRange) (int, error) { return b.FD(), nil }
func (b *baselineFile) MapInternal(fr memmap.FileRange, at hostarch.AccessType) (safemem.BlockSeq, error) {
	if at.Write || fr.End > uint64(len(b.lazy.mapping)) {
		return safemem.BlockSeq{}, unix.EACCES
	}
	return safemem.BlockSeqOf(safemem.BlockFromUnsafeSlice(b.lazy.mapping[fr.Start:fr.End])), nil
}

// ReadBacking returns the longest initial run sharing a backing file.
// The tmpfs data lock serializes selection with transitions for this inode.
func (f *MemoryFile) ReadBacking(fr memmap.FileRange) (memmap.File, memmap.FileRange) {
	l := f.lazy
	if l == nil {
		return f, fr
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	inherited := func(off uint64) bool {
		_, local := l.pages[off]
		return off < uint64(l.size) && !local
	}
	start := hostarch.PageRoundDown(fr.Start)
	base := inherited(start)
	for off := start + hostarch.PageSize; off < fr.End; off += hostarch.PageSize {
		if inherited(off) != base {
			fr.End = off
			break
		}
	}
	if base {
		return &l.source, fr
	}
	return f, fr
}

// OverwriteInheritedPage commits a completely supplied page without fetching
// its predecessor. A short write leaves the baseline authoritative.
func (f *MemoryFile) OverwriteInheritedPage(offset uint64, src safemem.BlockSeq) error {
	if offset%hostarch.PageSize != 0 || src.NumBytes() != hostarch.PageSize {
		panic("unaligned inherited page overwrite")
	}
	l := f.lazy
	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := hostfd.Pwritev2(int32(f.file.Fd()), src, int64(offset), 0)
	if err != nil {
		return err
	}
	if n != hostarch.PageSize {
		return io.ErrShortWrite
	}
	l.pages[offset] = lazyFilestorePage{}
	return nil
}

// markCleared requires l.mu. The caller makes or has made the local contents
// zero. Future accesses use only the local backing, even after offset reuse.
func (l *lazyFilestore) markCleared(fr memmap.FileRange) {
	for offset := fr.Start; offset < fr.End; offset += hostarch.PageSize {
		l.pages[offset] = lazyFilestorePage{}
	}
}

// snapshotDelta examines the local pages of the reflink snapshot while the
// sandbox is quiesced. CPU stores to shared mappings are visible through dst.
func (l *lazyFilestore) snapshotDelta(dst *os.File, size int64) (*checkpoint.FilestoreDelta, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	offsets := make([]uint64, 0, len(l.pages))
	for offset := range l.pages {
		if offset < uint64(size) {
			offsets = append(offsets, offset)
		}
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })

	delta := &checkpoint.FilestoreDelta{
		BaseFingerprint: l.baseHash,
		BaseSize:        l.size,
		BaseReadBytes:   l.baseReadBytes,
	}
	var buf [hostarch.PageSize]byte
	for _, offset := range offsets {
		if _, err := dst.ReadAt(buf[:], int64(offset)); err != nil {
			return nil, "", fmt.Errorf("scan local filestore at %d: %w", offset, err)
		}
		delta.ScannedBytes += uint64(len(buf))
		hash := sha256.Sum256(buf[:])
		page := l.pages[offset]
		if page.inherited && page.hash == hash {
			continue
		}
		zero := hash == zeroPageHash
		if zero && offset >= uint64(l.size) {
			continue // Extending the complete artifact already gives zeros.
		}
		if len(delta.Extents) > 0 {
			last := &delta.Extents[len(delta.Extents)-1]
			if last.Zero == zero && last.Offset+last.Length == int64(offset) {
				last.Length += hostarch.PageSize
				continue
			}
		}
		delta.Extents = append(delta.Extents, checkpoint.FilestoreExtent{
			Offset: int64(offset), Length: hostarch.PageSize, Zero: zero,
		})
	}
	fp, err := checkpoint.FingerprintReaderAt(lazyFilestoreView{layer: l, local: dst}, size)
	if err != nil {
		return nil, "", fmt.Errorf("fingerprint complete filestore view: %w", err)
	}
	return delta, fp, nil
}

// lazyFilestoreView resolves the sampled ranges of an immutable save view.
type lazyFilestoreView struct {
	layer *lazyFilestore
	local *os.File
}

// The caller must hold view.layer.mu.
func (view lazyFilestoreView) ReadAt(data []byte, offset int64) (int, error) {
	read := 0
	for len(data) > 0 {
		pageOffset := hostarch.PageRoundDown(uint64(offset))
		length := min(len(data), hostarch.PageSize-int(uint64(offset)-pageOffset))
		file := view.local
		if _, present := view.layer.pages[pageOffset]; !present && offset < view.layer.size {
			file = view.layer.base
		}
		n, err := file.ReadAt(data[:length], offset)
		read += n
		if err != nil {
			return read, err
		}
		if n != length {
			return read, io.ErrUnexpectedEOF
		}
		data = data[length:]
		offset += int64(length)
	}
	return read, nil
}
