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

package overlay_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/overlay"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/limits"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
)

// observedWriteSource counts copies into a staging buffer. BytesIO.CopyInTo
// supplies blocks directly to the file writer.
type observedWriteSource struct {
	usermem.BytesIO
	staged int
}

func (s *observedWriteSource) CopyIn(ctx context.Context, addr hostarch.Addr, dst []byte, opts usermem.IOOpts) (int, error) {
	n, err := s.BytesIO.CopyIn(ctx, addr, dst, opts)
	s.staged += n
	return n, err
}

func TestLazyFilestoreSharedPin(t *testing.T) {
	cpuid.Initialize()
	ctx := contexttest.Context(t)
	creds := auth.CredentialsFromContext(ctx)
	var vfsObj vfs.VirtualFilesystem
	if err := vfsObj.Init(ctx); err != nil {
		t.Fatal(err)
	}
	vfsObj.MustRegisterFilesystemType(tmpfs.Name, tmpfs.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{})
	vfsObj.MustRegisterFilesystemType(overlay.Name, overlay.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{})
	mount := func(name string, data any) vfs.VirtualDentry {
		t.Helper()
		ns, err := vfsObj.NewMountNamespace(ctx, creds, "", name, &vfs.MountOptions{
			GetFilesystemOptions: vfs.GetFilesystemOptions{InternalMount: true, InternalData: data},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		root := ns.Root(ctx)
		t.Cleanup(func() { root.DecRef(ctx); ns.DecRef(ctx) })
		return root
	}
	open := func(root vfs.VirtualDentry, flags uint32) *vfs.FileDescription {
		t.Helper()
		fd, err := vfsObj.OpenAt(ctx, creds, &vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse("file")}, &vfs.OpenOptions{Flags: flags, Mode: 0600})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { fd.DecRef(ctx) })
		return fd
	}
	dir := t.TempDir()
	newMemoryFile := func(name string, base *os.File) *pgalloc.MemoryFile {
		t.Helper()
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		mf, err := pgalloc.NewMemoryFile(f, pgalloc.MemoryFileOpts{
			DiskBackedFile: true, LazyBase: base, DisableMemoryAccounting: true, DisableIMAWorkAround: true,
		})
		if err != nil {
			f.Close()
			t.Fatal(err)
		}
		t.Cleanup(mf.Destroy)
		return mf
	}

	// Capture both file offsets and allocator metadata, then restore the
	// same file in a lazy tmpfs upper layer under an actual overlay mount.
	orig := newMemoryFile("base", nil)
	oldRoot := mount(tmpfs.Name, tmpfs.FilesystemOpts{MemoryFile: orig})
	original := open(oldRoot, linux.O_RDWR|linux.O_CREAT)
	const size = 8 * hostarch.PageSize
	want := bytes.Repeat([]byte{'A'}, size)
	if n, err := original.Write(ctx, usermem.BytesIOSequence(want), vfs.WriteOptions{}); n != size || err != nil {
		t.Fatalf("seed wrote %d: %v", n, err)
	}
	var tree, metadata bytes.Buffer
	if err := tmpfs.FSCheckpointWrite(ctx, oldRoot.Mount().Filesystem(), &tree); err != nil {
		t.Fatal(err)
	}
	if err := orig.SaveTo(ctx, &metadata, &pgalloc.SaveOpts{ExternalContent: true}); err != nil {
		t.Fatal(err)
	}
	base, err := os.Open(filepath.Join(dir, "base"))
	if err != nil {
		t.Fatal(err)
	}
	local := newMemoryFile("live", base)
	if err := local.LoadFrom(ctx, &metadata, &pgalloc.LoadOpts{}); err != nil {
		t.Fatal(err)
	}
	upper := mount(tmpfs.Name, tmpfs.FilesystemOpts{MemoryFile: local, SourceTar: io.NopCloser(&tree), SourceTarFSCheckpoint: true})
	root := mount(overlay.Name, overlay.FilesystemOptions{UpperRoot: upper, LowerRoots: []vfs.VirtualDentry{mount(tmpfs.Name, nil)}})
	reader := open(root, linux.O_RDONLY)
	writer := open(root, linux.O_RDWR)
	mapFile := func(fd *vfs.FileDescription, perms hostarch.AccessType) (*mm.MemoryManager, hostarch.Addr) {
		t.Helper()
		m, err := mm.NewMemoryManager(platform.FromContext(ctx), pgalloc.MemoryFileFromContext(ctx))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { m.DecUsers(ctx) })
		if _, err := m.SetMmapLayout(arch.New(arch.Host), limits.FromContext(ctx)); err != nil {
			t.Fatal(err)
		}
		opts := memmap.MMapOpts{Length: size, Perms: perms, MaxPerms: perms}
		if err := fd.ConfigureMMap(ctx, &opts); err != nil {
			t.Fatal(err)
		}
		defer opts.MappingIdentity.DecRef(ctx)
		addr, err := m.MMap(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		return m, addr
	}
	ro, addr := mapFile(reader, hostarch.Read)
	rw, other := mapFile(writer, hostarch.ReadWrite)

	// Populate a read-only inherited translation before pinning. Preparing
	// the pin must be able to invalidate this MM and reacquire its activeMu.
	var first [1]byte
	if _, err := ro.CopyIn(ctx, addr, first[:], usermem.IOOpts{}); err != nil || first[0] != 'A' {
		t.Fatalf("inherited read = %q, %v", first, err)
	}
	pins, err := ro.Pin(ctx, hostarch.AddrRange{Start: addr, End: addr + hostarch.PageSize}, hostarch.Read, false)
	defer mm.Unpin(pins)
	if err != nil || len(pins) != 1 {
		t.Fatalf("read-only pin returned %d ranges: %v", len(pins), err)
	}
	pin := pins[0]
	buf, err := pin.File.MapInternal(memmap.FileRange{Start: pin.Offset, End: pin.Offset + hostarch.PageSize}, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	pinned := buf.Head().ToSlice()
	if n, err := rw.CopyOut(ctx, other+1, []byte{'B'}, usermem.IOOpts{}); n != 1 || err != nil {
		t.Fatalf("shared write copied %d: %v", n, err)
	}
	if pinned[1] != 'B' {
		t.Fatalf("pin read %q after shared mmap write, want B", pinned[1])
	}
	want[1] = 'B'

	// Keep an inherited alias on the second page while writing the local
	// first page repeatedly. Only a write reaching the second page needs
	// to revoke that alias.
	if _, err := ro.CopyIn(ctx, addr+hostarch.PageSize, first[:], usermem.IOOpts{}); err != nil || first[0] != 'A' {
		t.Fatalf("second inherited page = %q, %v", first, err)
	}
	source := observedWriteSource{BytesIO: usermem.BytesIO{Bytes: []byte{'D'}}}
	src := usermem.BytesIOSequence(source.Bytes)
	src.IO = &source
	for i := 0; i < 3; i++ {
		if n, err := writer.PWrite(ctx, src, 2, vfs.WriteOptions{}); n != 1 || err != nil {
			t.Fatalf("local pwrite wrote %d: %v", n, err)
		}
	}
	if source.staged != 0 || pinned[2] != 'D' {
		t.Fatalf("local pwrite staged %d bytes, pin reads %q", source.staged, pinned[2])
	}
	want[2] = 'D'
	cross := []byte{'E', 'F'}
	if n, err := writer.PWrite(ctx, usermem.BytesIOSequence(cross), hostarch.PageSize-1, vfs.WriteOptions{}); n != 2 || err != nil {
		t.Fatalf("pwrite across backing transition wrote %d: %v", n, err)
	}
	if _, err := ro.CopyIn(ctx, addr+hostarch.PageSize, first[:], usermem.IOOpts{}); err != nil || first[0] != 'F' {
		t.Fatalf("retained alias after pwrite = %q, %v", first, err)
	}
	copy(want[hostarch.PageSize-1:], cross)

	// Release the reader's VMA: the pin survives it. Write across the
	// pinned local page, full inherited pages, and a final partial page.
	if err := ro.MUnmap(ctx, addr, size); err != nil {
		t.Fatal(err)
	}
	if err := rw.MUnmap(ctx, other, size); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{'C'}, size-5)
	if n, err := writer.PWrite(ctx, usermem.BytesIOSequence(payload), 3, vfs.WriteOptions{}); n != int64(len(payload)) || err != nil {
		t.Fatalf("pwrite wrote %d: %v", n, err)
	}
	copy(want[3:], payload)
	if !bytes.Equal(pinned, want[:hostarch.PageSize]) {
		t.Fatal("pin diverged after pwrite")
	}
	got := make([]byte, size)
	if n, err := reader.PRead(ctx, usermem.BytesIOSequence(got), 0, vfs.ReadOptions{}); n != size || err != nil || !bytes.Equal(got, want) {
		t.Fatalf("pwrite readback mismatch: read %d, err %v", n, err)
	}
}
