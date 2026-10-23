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

package park

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func newTestJob(t *testing.T) (*Store, Status) {
	t.Helper()
	store, status, err := Create(filepath.Join(t.TempDir(), "job"), "container")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, status
}

func TestReadyPublicationAndCompletionBarriers(t *testing.T) {
	store, status := newTestJob(t)
	if err := os.Mkdir(status.ImagePath, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	status.StoppedAt = &now
	status.DurableAt = &now
	status.State = Ready
	if err := store.Write(status, true); err != nil {
		t.Fatal(err)
	}
	live, err := Read(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if live.State != Committing || live.DurableAt != nil {
		t.Fatalf("live publication advertised ready: %+v", live)
	}
	if err := CheckArtifact(status.ImagePath, "image"); err == nil {
		t.Fatal("restore accepted a live publication")
	}
	if _, err := Wait(store.Dir, "stopped", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := Wait(store.Dir, "ready", 20*time.Millisecond); err == nil {
		t.Fatal("ready wait ignored publication lock")
	}
	store.Close()
	durable, err := Read(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if durable.State != Ready || durable.DurableAt == nil {
		t.Fatalf("durable status: %+v", durable)
	}
	if err := CheckArtifact(status.ImagePath, "image"); err != nil {
		t.Fatal(err)
	}
}

func TestAbandonedWorkerFailsAndCleansOnlyOwnedArtifacts(t *testing.T) {
	store, status := newTestJob(t)
	for _, dir := range []string{status.ImagePath, status.FilestorePath} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	victim := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(victim, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(victim), filepath.Join(status.ImagePath, "external")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(status.FilestorePath, "partial"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	status.StoppedAt = &now
	status.State = Committing
	if err := store.Write(status, false); err != nil {
		t.Fatal(err)
	}
	store.Close()
	failed, err := Read(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != Failed || failed.StoppedAt == nil || failed.Error == "" || failed.CleanupError != "" {
		t.Fatalf("bad failed status: %+v", failed)
	}
	for _, dir := range []string{status.ImagePath, status.FilestorePath} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("output survived cleanup: %s: %v", dir, err)
		}
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "original" {
		t.Fatalf("cleanup escaped job: %q %v", got, err)
	}
	if _, err := Wait(store.Dir, "ready", time.Second); err == nil {
		t.Fatal("failed job became ready")
	}
	if err := CheckArtifact(status.ImagePath, "image"); err == nil {
		t.Fatal("restore accepted failed job")
	}
}

func TestInheritedLockIdentityAndTimeoutOwnership(t *testing.T) {
	a, _ := newTestJob(t)
	b, _ := newTestJob(t)
	raw, err := unix.Dup(int(a.LockFile().Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Inherit(b.Dir, os.NewFile(uintptr(raw), "wrong-lock")); err == nil {
		t.Fatal("accepted another job's lock")
	}
	if status, err := Read(a.Dir); err != nil || status.State != Preparing {
		t.Fatalf("original ownership lost: %+v %v", status, err)
	}
	if _, err := Wait(a.Dir, "ready", 20*time.Millisecond); err == nil {
		t.Fatal("wait did not time out")
	}
	if status, err := Read(a.Dir); err != nil || status.State != Preparing {
		t.Fatalf("timeout changed task ownership: %+v %v", status, err)
	}
}

func TestArtifactSyncRejectsSymlinks(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "outside")); err != nil {
		t.Fatal(err)
	}
	if err := SyncArtifacts(dir); err == nil {
		t.Fatal("synced a symlink as a checkpoint artifact")
	}
}

func TestManagedOutputSymlinkCannotBypassPendingState(t *testing.T) {
	store, _ := newTestJob(t)
	outside := t.TempDir()
	image := filepath.Join(store.Dir, "image")
	if err := os.Symlink(outside, image); err != nil {
		t.Fatal(err)
	}
	if err := CheckArtifact(image, "image"); err == nil {
		t.Fatal("managed output symlink bypassed pending state")
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(image, alias); err != nil {
		t.Fatal(err)
	}
	// An alias to an existing managed image must also honor its source marker.
	os.Remove(image)
	if err := os.Mkdir(image, 0700); err != nil {
		t.Fatal(err)
	}
	if err := CheckArtifact(alias, "image"); err == nil {
		t.Fatal("alias bypassed managed publication")
	}
}

func TestJobParentAliasAndMalformedInheritance(t *testing.T) {
	parent := t.TempDir()
	alias := filepath.Join(t.TempDir(), "parent")
	if err := os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	store, _, err := Create(filepath.Join(alias, "job"), "container")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if status, err := Read(filepath.Join(alias, "job")); err != nil || status.State != Preparing {
		t.Fatalf("parent alias: %+v %v", status, err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, statusName), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := unix.Dup(int(store.LockFile().Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Inherit(store.Dir, os.NewFile(uintptr(raw), "inherited")); err == nil {
		t.Fatal("accepted malformed status")
	}
	if _, err := os.Stat(store.Dir); err != nil {
		t.Fatalf("malformed inheritance removed the task directory: %v", err)
	}
}

func TestReadyJobRemainsUsableAfterMove(t *testing.T) {
	store, status := newTestJob(t)
	now := time.Now()
	status.State = Ready
	status.StoppedAt = &now
	status.DurableAt = &now
	if err := store.Write(status, true); err != nil {
		t.Fatal(err)
	}
	store.Close()
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(store.Dir, moved); err != nil {
		t.Fatal(err)
	}
	got, err := Read(moved)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Ready || got.ImagePath != filepath.Join(moved, "image") || got.FilestorePath != filepath.Join(moved, "filestores") {
		t.Fatalf("moved task retained source paths: %+v", got)
	}
}
