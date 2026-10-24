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
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sync"
)

type snapshotGateReader struct {
	data        *bytes.Reader
	failure     error
	entered     chan struct{}
	release     chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

func (r *snapshotGateReader) ReadAt(dst []byte, off int64) (int, error) {
	r.enterOnce.Do(func() { close(r.entered) })
	<-r.release
	if r.failure != nil {
		return 0, r.failure
	}
	return r.data.ReadAt(dst, off)
}

func (r *snapshotGateReader) unblock() {
	r.releaseOnce.Do(func() { close(r.release) })
}

type snapshotLoadFixture struct {
	mf     *MemoryFile
	fr     memmap.FileRange
	reader *snapshotGateReader
	done   chan struct{}
	err    error // Read only after done is closed.
}

// newSnapshotLoadFixture uses the real split-file codec and asynchronous page
// loader. The sole reader is stopped before it can write any restored bytes.
func newSnapshotLoadFixture(t *testing.T, failure error) *snapshotLoadFixture {
	t.Helper()
	dir := t.TempDir()
	orig := newTestMemoryFile(t, filepath.Join(dir, "original"), false)
	t.Cleanup(orig.Destroy)
	fr, err := orig.Allocate(2*hostarch.PageSize, AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		t.Fatal(err)
	}
	writeTestRange(t, orig, fr, 0xab)
	var metadata, pages bytes.Buffer
	saveDone := make(chan error, 1)
	aw := stateio.NewIOWriter(&pages, 2*hostarch.PageSize, 1, 1)
	apfs, err := StartAsyncPagesFileSave(aw, func(err error) { saveDone <- err })
	if err != nil {
		t.Fatal(err)
	}
	saveErr := orig.SaveTo(context.Background(), &metadata, &SaveOpts{PagesFile: apfs})
	apfs.MemoryFilesDone()
	select {
	case err := <-saveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("split-file save did not complete")
	}
	if saveErr != nil {
		t.Fatal(saveErr)
	}
	fx := &snapshotLoadFixture{
		mf: newTestMemoryFile(t, filepath.Join(dir, "restored"), false),
		fr: fr,
		reader: &snapshotGateReader{
			data:    bytes.NewReader(pages.Bytes()),
			failure: failure,
			entered: make(chan struct{}),
			release: make(chan struct{}),
		},
		done: make(chan struct{}),
	}
	t.Cleanup(fx.mf.Destroy)
	ar := stateio.NewIOReader(fx.reader, 2*hostarch.PageSize, 1, 1)
	apfl, err := StartAsyncPagesFileLoad(ar, func(err error) {
		fx.err = err
		close(fx.done)
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		fx.reader.unblock()
		select {
		case <-fx.done:
		case <-time.After(10 * time.Second):
			t.Error("background page loader was not released during cleanup")
		}
	})
	loadErr := fx.mf.LoadFrom(context.Background(), bytes.NewReader(metadata.Bytes()), &LoadOpts{PagesFile: apfl})
	apfl.MemoryFilesDone()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	select {
	case <-fx.reader.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("real asynchronous page reader did not reach the barrier")
	}
	return fx
}

func TestFilestoreSnapshotWaitsForBackgroundLoad(t *testing.T) {
	fx := newSnapshotLoadFixture(t, nil)
	dst, err := os.Create(filepath.Join(t.TempDir(), "snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		size        uint64
		fingerprint string
		err         error
	}
	returned := make(chan result, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		size, fingerprint, err := fx.mf.SnapshotToFd(dst)
		returned <- result{size, fingerprint, err}
	}()
	t.Cleanup(func() {
		fx.reader.unblock()
		select {
		case <-finished:
		case <-time.After(30 * time.Second):
			t.Error("snapshot goroutine was not released during cleanup")
		}
		dst.Close()
	})
	// Observe a waiter registered by SnapshotToFd on the actual blocked page
	// loader, rather than treating a fixed sleep as proof that it waited.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
waiting:
	for {
		select {
		case r := <-returned:
			t.Fatalf("snapshot finished before background input was released: size=%d fingerprint=%q err=%v", r.size, r.fingerprint, r.err)
		case <-ticker.C:
			amfl := fx.mf.asyncPageLoad.Load()
			if amfl == nil {
				t.Fatal("loader disappeared while its real input was blocked")
			}
			amfl.pf.mu.Lock()
			waiters := amfl.pf.numWaiters
			amfl.pf.mu.Unlock()
			if waiters > 0 {
				break waiting
			}
		case <-deadline.C:
			t.Fatal("snapshot never joined the blocked background page loader")
		}
	}
	fx.reader.unblock()
	select {
	case r := <-returned:
		if r.err != nil {
			t.Fatal(r.err)
		}
		st, err := dst.Stat()
		if err != nil || uint64(st.Size()) != r.size || r.fingerprint == "" {
			t.Fatalf("invalid completed snapshot credentials: %+v, stat=%v", r, err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("snapshot did not finish after the loader was released")
	}
	got := make([]byte, fx.fr.Length())
	if _, err := dst.ReadAt(got, int64(fx.fr.Start)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte{0xab}, len(got))) {
		t.Fatal("snapshot did not capture the restored private file bytes")
	}
}

func TestFilestoreSnapshotRejectsBackgroundLoadError(t *testing.T) {
	fx := newSnapshotLoadFixture(t, unix.EIO)
	fx.reader.unblock()
	select {
	case <-fx.done:
	case <-time.After(10 * time.Second):
		t.Fatal("injected background error did not reach the real loader")
	}
	if !errors.Is(fx.err, linuxerr.EIO) {
		t.Fatalf("background failure = %v, want EIO", fx.err)
	}
	dst, err := os.Create(filepath.Join(t.TempDir(), "snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if _, _, err := fx.mf.SnapshotToFd(dst); !errors.Is(err, linuxerr.EIO) {
		t.Fatalf("SnapshotToFd ignored failed async restore: got %v, want EIO", err)
	}
	if st, err := dst.Stat(); err != nil || st.Size() != 0 {
		t.Fatalf("failed source produced snapshot bytes: stat=%v err=%v", st, err)
	}
}
