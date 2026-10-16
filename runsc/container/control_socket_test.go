// Copyright 2026 The gVisor Authors.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/sandbox"
)

// These tests exercise exact file retirement only. They do not fabricate
// process births or claim an offline cgroup or a completed physical deletion.
func controlSocketOwner(t *testing.T) (*Container, string) {
	t.Helper()
	c := birthOwnerFixture(t)
	path := filepath.Join(c.Saver.RootDir, "runsc-"+c.ID+".sock")
	c.Sandbox = &sandbox.Sandbox{ID: c.ID, ResourceGeneration: c.ResourceGeneration, ControlSocketPath: path}
	return c, path
}

func bindControlSocketFile(t *testing.T, path string) {
	t.Helper()
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	address := filepath.Join("/proc/self/fd", strconv.Itoa(int(directory.Fd())), filepath.Base(path))
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: address}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
}

func TestManagedControlSocketRetirementAllowsSameCanonicalNameAgain(t *testing.T) {
	c, path := controlSocketOwner(t)
	bindControlSocketFile(t, path)
	original, err := c.prepareManagedControlSocket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer original.close()
	if err := original.remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("retired original socket remains: %v", err)
	}
	bindControlSocketFile(t, path)
	if err := original.remove(context.Background()); err == nil {
		t.Fatal("original retirement removed a replacement socket")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("replacement socket changed: %v", err)
	}
}

func TestManagedControlSocketRetirementRejectsForeignPathsAndObjects(t *testing.T) {
	for _, kind := range []string{"foreign-root", "foreign-name", "regular", "symlink", "hardlink", "writable"} {
		t.Run(kind, func(t *testing.T) {
			c, path := controlSocketOwner(t)
			switch kind {
			case "foreign-root":
				c.Sandbox.ControlSocketPath = filepath.Join(t.TempDir(), filepath.Base(path))
				bindControlSocketFile(t, c.Sandbox.ControlSocketPath)
			case "foreign-name":
				c.Sandbox.ControlSocketPath = filepath.Join(c.Saver.RootDir, "runsc-neighbor.sock")
				bindControlSocketFile(t, c.Sandbox.ControlSocketPath)
			case "regular":
				if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				foreign := filepath.Join(t.TempDir(), "foreign")
				if err := os.WriteFile(foreign, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(foreign, path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				bindControlSocketFile(t, path)
				if err := os.Link(path, path+".neighbor"); err != nil {
					t.Fatal(err)
				}
			case "writable":
				bindControlSocketFile(t, path)
				if err := os.Chmod(path, 0777); err != nil {
					t.Fatal(err)
				}
			}
			if original, err := c.prepareManagedControlSocket(context.Background()); err == nil {
				original.close()
				t.Fatal("foreign or unsafe socket accepted")
			}
			if _, err := os.Lstat(c.Sandbox.ControlSocketPath); err != nil {
				t.Fatalf("refused object changed: %v", err)
			}
		})
	}
}

func TestManagedControlSocketRetirementKeepsChangedOriginalAndNeighbor(t *testing.T) {
	for _, kind := range []string{"replacement", "new-after-absence", "directory", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			c, path := controlSocketOwner(t)
			if kind != "new-after-absence" {
				bindControlSocketFile(t, path)
			}
			original, err := c.prepareManagedControlSocket(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer original.close()
			ctx := context.Background()
			switch kind {
			case "replacement":
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				bindControlSocketFile(t, path)
			case "new-after-absence":
				bindControlSocketFile(t, path)
			case "directory":
				if err := os.Rename(c.Saver.RootDir, c.Saver.RootDir+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(c.Saver.RootDir, 0700); err != nil {
					t.Fatal(err)
				}
				bindControlSocketFile(t, path)
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := original.remove(ctx); err == nil {
				t.Fatal("changed or cancelled retirement accepted")
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("replacement or original changed: %v", err)
			}
		})
	}
}
