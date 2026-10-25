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

package tmpfs

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"

	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// TestSourceTarLongSymlinkRelease is a regression test for a bug where
// symlinkFromTar did not call fs.accountPages(1) for symlinks whose target
// length is >= shortSymlinkLen, while (*inode).decRef unconditionally calls
// fs.unaccountPages(1) on teardown for such symlinks. The asymmetry
// underflowed fs.pagesUsed and panicked filesystem.unaccountPages on
// mount-namespace teardown.
func TestSourceTarLongSymlinkRelease(t *testing.T) {
	ctx := contexttest.Context(t)
	creds := auth.CredentialsFromContext(ctx)

	// Symlink target of length shortSymlinkLen (128) triggers the long-symlink
	// accounting path.
	longTarget := strings.Repeat("a", shortSymlinkLen)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     "./",
		Typeflag: tar.TypeDir,
		Mode:     0755,
	}); err != nil {
		t.Fatalf("tar.WriteHeader(dir): %v", err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:     "./longlink",
		Typeflag: tar.TypeSymlink,
		Linkname: longTarget,
		Mode:     0777,
	}); err != nil {
		t.Fatalf("tar.WriteHeader(symlink): %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar.Writer.Close: %v", err)
	}

	vfsObj := &vfs.VirtualFilesystem{}
	if err := vfsObj.Init(ctx); err != nil {
		t.Fatalf("VFS init: %v", err)
	}
	vfsObj.MustRegisterFilesystemType("tmpfs", FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{
		AllowUserMount: true,
	})

	mntns, err := vfsObj.NewMountNamespace(ctx, creds, "", "tmpfs", &vfs.MountOptions{
		GetFilesystemOptions: vfs.GetFilesystemOptions{
			InternalData: FilesystemOpts{
				SourceTar: io.NopCloser(&buf),
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("NewMountNamespace: %v", err)
	}

	// Drop the only reference to trigger filesystem teardown. Without the fix,
	// releaseChildrenLocked -> inode.decRef -> fs.unaccountPages(1) underflows
	// fs.pagesUsed and panics here.
	mntns.DecRef(ctx)
}

// TestMergeTarPreservesOverlayWhiteout reproduces restoring the deleted Git
// index.lock entry from the writable-layer archive into its saved tmpfs inode.
func TestMergeTarPreservesOverlayWhiteout(t *testing.T) {
	ctx := contexttest.Context(t)
	_, root, cleanup, err := newTmpfsRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	fs := root.Mount().Filesystem().Impl().(*filesystem)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	paths := map[string]*inode{"./": fs.root.inode}
	cb := &tarDefaultReaderCallbacks{headerToContent: make(map[*tar.Header]*bytes.Buffer)}
	whiteout := &tar.Header{Name: "./index.lock", Typeflag: tar.TypeChar, Mode: 0}
	if err := fs.mknodFromTar(ctx, whiteout, paths, cb); err != nil {
		t.Fatal(err)
	}
	alias := &tar.Header{Name: "./another.lock", Typeflag: tar.TypeLink, Linkname: whiteout.Name}
	if err := fs.linkFromTar(alias, paths); err != nil {
		t.Fatal(err)
	}
	original := paths[whiteout.Name]
	links := original.nlink.Load()
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, hdr := range []*tar.Header{{Name: "./", Typeflag: tar.TypeDir, Mode: 0755}, whiteout, alias} {
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := fs.mergeFromTar(ctx, tar.NewReader(bytes.NewReader(archive.Bytes())), cb); err != nil {
			t.Fatalf("merge %d: %v", attempt, err)
		}
		dir := fs.root.inode.impl.(*directory)
		if dir.childMap["index.lock"].inode != original || dir.childMap["another.lock"].inode != original || original.nlink.Load() != links {
			t.Fatal("whiteout inode, alias or link count changed during restore")
		}
		if dev, ok := original.impl.(*deviceFile); !ok || dev != fs.ovlWhiteout {
			t.Fatal("restored deletion no longer uses the original overlay whiteout")
		}
	}
}

func TestMergeTarWhiteoutRefusesTypeOrDeviceChange(t *testing.T) {
	for _, test := range []struct {
		name               string
		existing, incoming tar.Header
	}{
		{"whiteout-to-device", tar.Header{Typeflag: tar.TypeChar}, tar.Header{Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}},
		{"device-to-whiteout", tar.Header{Typeflag: tar.TypeChar, Mode: 0600, Devmajor: 1, Devminor: 3}, tar.Header{Typeflag: tar.TypeChar}},
		{"regular-to-whiteout", tar.Header{Typeflag: tar.TypeReg, Mode: 0600}, tar.Header{Typeflag: tar.TypeChar}},
		{"whiteout-to-block", tar.Header{Typeflag: tar.TypeChar}, tar.Header{Typeflag: tar.TypeBlock}},
		{"whiteout-to-fifo", tar.Header{Typeflag: tar.TypeChar}, tar.Header{Typeflag: tar.TypeFifo}},
		{"whiteout-mode-change", tar.Header{Typeflag: tar.TypeChar}, tar.Header{Typeflag: tar.TypeChar, Mode: 0600}},
		{"whiteout-major-wrap", tar.Header{Typeflag: tar.TypeChar}, tar.Header{Typeflag: tar.TypeChar, Devmajor: 1 << 16}},
		{"whiteout-minor-wrap", tar.Header{Typeflag: tar.TypeChar}, tar.Header{Typeflag: tar.TypeChar, Devminor: 1 << 32}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := contexttest.Context(t)
			_, root, cleanup, err := newTmpfsRoot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			fs := root.Mount().Filesystem().Impl().(*filesystem)
			fs.mu.Lock()
			defer fs.mu.Unlock()
			paths := map[string]*inode{"./": fs.root.inode}
			cb := &tarDefaultReaderCallbacks{headerToContent: make(map[*tar.Header]*bytes.Buffer)}
			test.existing.Name = "./index.lock"
			test.incoming.Name = test.existing.Name
			if err := fs.mknodFromTar(ctx, &test.existing, paths, cb); err != nil {
				t.Fatal(err)
			}
			original := paths[test.existing.Name]
			links, mode := original.nlink.Load(), original.mode.Load()
			if err := fs.mknodMergeFromTar(ctx, &test.incoming, paths, cb); err == nil {
				t.Fatal("conflicting type or device was accepted")
			}
			if paths[test.existing.Name] != original || original.nlink.Load() != links || original.mode.Load() != mode {
				t.Fatal("refused merge changed the original inode")
			}
		})
	}
}
