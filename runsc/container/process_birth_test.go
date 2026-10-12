// Copyright 2026 The gVisor Authors.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/processbirth"
)

func birthOwnerFixture(t *testing.T) *Container {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	c := &Container{
		ID: "container-original", ResourceGeneration: "generation-original", ProcessBirthVersion: 1,
		Status: Creating, BundleDir: "/original/bundle",
		Spec:  &specs.Spec{Annotations: map[string]string{processbirth.GenerationAnnotation: "generation-original"}},
		Saver: StateFile{RootDir: root, ID: FullID{ContainerID: "container-original", SandboxID: "container-original"}},
	}
	if err := c.Saver.LockForNew(); err != nil {
		t.Fatal(err)
	}
	if err := c.saveLocked(); err != nil {
		t.Fatal(err)
	}
	c.Saver.UnlockOrDie()
	return c
}

func readBirthOwnerBytes(t *testing.T, c *Container) []byte {
	t.Helper()
	data, err := os.ReadFile(c.Saver.statePath())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestManagedUndispatchedOwnerRetiresOnlyItsMatchingGeneration(t *testing.T) {
	c := birthOwnerFixture(t)
	before := readBirthOwnerBytes(t, c)
	if err := c.DestroyIfGeneration(context.Background(), "generation-replacement"); err == nil {
		t.Fatal("wrong generation retired")
	}
	if after := readBirthOwnerBytes(t, c); string(before) != string(after) {
		t.Fatal("wrong generation changed original state")
	}
	// No process was dispatched: this case exercises only persisted native
	// preparation. It does not signal a real task or prove partial startup.
	if err := c.DestroyIfGeneration(context.Background(), c.ResourceGeneration); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.Saver.statePath()); !os.IsNotExist(err) {
		t.Fatalf("matching preparation remains: %v", err)
	}
}

func TestManagedCachedOwnerCannotRetireReplacementOnDisk(t *testing.T) {
	c := birthOwnerFixture(t)
	original, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var replacement Container
	if err := json.Unmarshal(original, &replacement); err != nil {
		t.Fatal(err)
	}
	replacement.ResourceGeneration = "generation-replacement"
	replacement.Spec = &specs.Spec{Annotations: map[string]string{processbirth.GenerationAnnotation: replacement.ResourceGeneration}}
	data, err := json.Marshal(&replacement)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Saver.statePath(), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.DestroyIfGeneration(context.Background(), c.ResourceGeneration); err == nil {
		t.Fatal("cached owner deleted replacement")
	}
	if string(readBirthOwnerBytes(t, c)) != string(data) {
		t.Fatal("replacement changed")
	}
}

func TestManagedMissingBirthAfterDispatchCannotRemoveOwnerState(t *testing.T) {
	c := birthOwnerFixture(t)
	c.RuntimeDispatched = true
	if err := c.Saver.lock(BlockAcquire); err != nil {
		t.Fatal(err)
	}
	if err := c.saveLocked(); err != nil {
		t.Fatal(err)
	}
	c.Saver.UnlockOrDie()
	before := readBirthOwnerBytes(t, c)
	if err := c.DestroyIfGeneration(context.Background(), c.ResourceGeneration); err == nil || !strings.Contains(err.Error(), "birth is incomplete") {
		t.Fatalf("missing birth became completed cleanup: %v", err)
	}
	if string(readBirthOwnerBytes(t, c)) != string(before) {
		t.Fatal("incomplete owner was removed or modified")
	}
}

func TestManagedOwnerRejectsMalformedOrUnsafeNativeState(t *testing.T) {
	for _, kind := range []string{"duplicate", "trailing", "symlink", "fifo", "hardlink", "oversized", "unsafe-mode"} {
		t.Run(kind, func(t *testing.T) {
			c := birthOwnerFixture(t)
			path := c.Saver.statePath()
			data := readBirthOwnerBytes(t, c)
			switch kind {
			case "duplicate":
				data = []byte(strings.Replace(string(data), "\"resourceGeneration\":", "\"resourceGeneration\":\"replacement\",\"resourceGeneration\":", 1))
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "trailing":
				if err := os.WriteFile(path, append(data, []byte(" {}")...), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "replacement")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, filepath.Join(t.TempDir(), "second-link")); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.Truncate(path, managedRuntimeStateLimit+1); err != nil {
					t.Fatal(err)
				}
			case "unsafe-mode":
				if err := os.Chmod(path, 0666); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.DestroyIfGeneration(context.Background(), c.ResourceGeneration); err == nil {
				t.Fatal("unsafe owner became completion")
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("unsafe original source was removed: %v", err)
			}
		})
	}
}
