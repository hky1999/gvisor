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

package checkpoint

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func cloneTestFiles(t *testing.T, contents []byte) (*os.File, *os.File) {
	t.Helper()
	dir := t.TempDir()
	src, err := os.Create(filepath.Join(dir, "source"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })
	if _, err := src.Write(contents); err != nil {
		t.Fatal(err)
	}
	dst, err := os.Create(filepath.Join(dir, "destination"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dst.Close() })
	return src, dst
}

func TestCloneFilestoreRangeShortAndInterrupted(t *testing.T) {
	contents := bytes.Repeat([]byte("filestore\x00"), 205)
	src, dst := cloneTestFiles(t, contents)
	if _, err := src.Seek(7, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Seek(3, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	calls := 0
	copyRange := func(in int, offIn *int64, out int, offOut *int64, count, flags int) (int, error) {
		calls++
		if calls == 1 {
			return -1, unix.EINTR
		}
		if in != int(src.Fd()) || out != int(dst.Fd()) || flags != 0 || *offIn != *offOut {
			t.Fatal("copy did not use the distinct held descriptors and explicit matching offsets")
		}
		if count > 257 {
			count = 257
		}
		buf := make([]byte, count)
		n, err := src.ReadAt(buf, *offIn)
		if err != nil {
			return n, err
		}
		n, err = dst.WriteAt(buf, *offOut)
		*offIn += int64(n)
		*offOut += int64(n)
		return n, err
	}
	if err := cloneFilestore(src, dst, func(int, int) error { return unix.EOPNOTSUPP }, copyRange); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(contents))
	if _, err := dst.ReadAt(got, 0); err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("short copy lost file contents: %v", err)
	}
	for _, p := range []struct {
		f    *os.File
		want int64
	}{{src, 7}, {dst, 3}} {
		if got, err := p.f.Seek(0, io.SeekCurrent); err != nil || got != p.want {
			t.Fatalf("file offset = %d, %v; want %d", got, err, p.want)
		}
	}
}

func TestCloneFilestoreErrorsRemainErrors(t *testing.T) {
	for _, errno := range []error{unix.EIO, unix.EPERM, unix.EBADF} {
		t.Run(errno.Error(), func(t *testing.T) {
			src, dst := cloneTestFiles(t, []byte("source"))
			copyCalled := false
			err := cloneFilestore(src, dst, func(int, int) error { return errno }, func(int, *int64, int, *int64, int, int) (int, error) {
				copyCalled = true
				return 0, nil
			})
			if !errors.Is(err, errno) || copyCalled {
				t.Fatalf("clone error = %v, fallback=%t; want original error without fallback", err, copyCalled)
			}
		})
	}
	for _, failure := range []error{unix.EIO, unix.EOPNOTSUPP, nil} {
		name := "unexpected-eof"
		want := error(io.ErrUnexpectedEOF)
		if failure != nil {
			name, want = failure.Error(), failure
		}
		t.Run("range-"+name, func(t *testing.T) {
			src, dst := cloneTestFiles(t, []byte("source"))
			calls := 0
			err := cloneFilestore(src, dst, func(int, int) error { return unix.EOPNOTSUPP }, func(_ int, in *int64, _ int, out *int64, _ int, _ int) (int, error) {
				calls++
				if calls == 1 {
					if _, err := dst.WriteAt([]byte("s"), 0); err != nil {
						t.Fatal(err)
					}
					*in, *out = 1, 1
					return 1, nil
				}
				return 0, failure
			})
			if !errors.Is(err, want) || calls != 2 {
				t.Fatalf("partial-copy error = %v, calls=%d; want %v", err, calls, want)
			}
		})
	}
}

func TestCloneFilestoreRejectsAliasAndNonempty(t *testing.T) {
	src, dst := cloneTestFiles(t, []byte("source"))
	for _, pair := range [][2]*os.File{{src, src}, {dst, src}, {dst, dst}} {
		if err := CloneFilestore(pair[0], pair[1]); err == nil {
			t.Fatal("accepted alias or nonempty destination")
		}
	}
}

func TestCloneFilestoreEmptyNativeRange(t *testing.T) {
	src, dst := cloneTestFiles(t, nil)
	err := cloneFilestore(src, dst, func(int, int) error { return unix.EOPNOTSUPP }, func(int, *int64, int, *int64, int, int) (int, error) {
		t.Fatal("empty filestore must not need a range syscall")
		return 0, unix.EIO
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCloneFilestoreAnonymousDirtyMappingIsPrivate(t *testing.T) {
	want := bytes.Repeat([]byte("mapped filestore\x00"), 65536)
	src, dst := cloneTestFiles(t, want)
	mapped, err := unix.Mmap(int(src.Fd()), 0, len(want), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(mapped)
	mapped[0], mapped[len(mapped)-1] = 'A', 'Z'
	want[0], want[len(want)-1] = 'A', 'Z'
	if err := os.Remove(src.Name()); err != nil {
		t.Fatal(err)
	}
	// No msync/fsync precedes the clone. The range-copy path must capture
	// the dirty mapping as FICLONE does for a paused private MemoryFile.
	if err := CloneFilestore(src, dst); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := dst.ReadAt(got, 0); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("clone omitted mapped writes: %v", err)
	}
	if _, err := dst.WriteAt([]byte("private"), 0); err != nil {
		t.Fatal(err)
	}
	if err := dst.Truncate(int64(len(want) / 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := src.ReadAt(got, 0); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("writing/truncating clone changed anonymous parent: %v", err)
	}
}
