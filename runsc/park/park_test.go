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
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
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
	temporary := filepath.Join(store.Dir, ".park-write-abandoned")
	if err := os.WriteFile(temporary, []byte("partial status"), 0600); err != nil {
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
	if _, err := os.Stat(temporary); !os.IsNotExist(err) {
		t.Fatalf("abandoned status temporary remains: %v", err)
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

func TestFailureMarkerOverridesOldReadyRecord(t *testing.T) {
	store, status := newTestJob(t)
	now := time.Now()
	status.StoppedAt = &now
	status.DurableAt = &now
	status.State = Ready
	if err := store.Write(status, true); err != nil {
		t.Fatal(err)
	}
	ready, err := os.ReadFile(filepath.Join(store.Dir, statusName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Fail(status, errors.New("publishing ready failed")); err != nil {
		t.Fatal(err)
	}
	// The failure marker must take precedence even if crash recovery exposes
	// an older READY generation of the detailed status file.
	if err := os.WriteFile(filepath.Join(store.Dir, statusName), ready, 0600); err != nil {
		t.Fatal(err)
	}
	store.Close()
	got, err := Read(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Failed || got.DurableAt != nil || !got.CleanupComplete || got.StoppedAt == nil {
		t.Fatalf("old READY survived failure: %+v", got)
	}
}

func TestUnreadableFailureRecordDoesNotAdvertiseReady(t *testing.T) {
	store, status := newTestJob(t)
	now := time.Now()
	status.StoppedAt = &now
	status.DurableAt = &now
	status.State = Ready
	if err := store.Write(status, true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, failureName), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	store.Close()
	got, err := Read(store.Dir)
	if err == nil || got.State == Ready || got.DurableAt != nil {
		t.Fatalf("unverified publication advertised ready: %+v %v", got, err)
	}
}

func TestCleanupDoesNotRequireDetailedStatusPublication(t *testing.T) {
	store, status := newTestJob(t)
	for _, dir := range []string{status.ImagePath, status.FilestorePath} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "partial"), []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	reserved, err := os.Stat(filepath.Join(store.Dir, failureReserveName))
	if err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(store.Dir, statusName)
	oldStatus := filepath.Join(store.Dir, "previous-status")
	if err := os.Rename(statusPath, oldStatus); err != nil {
		t.Fatal(err)
	}
	// Keep JSON replacement unavailable throughout failure and cleanup.
	if err := os.Mkdir(statusPath, 0700); err != nil {
		t.Fatal(err)
	}
	failed, err := store.Fail(status, unix.ENOSPC)
	if err == nil || !failed.CleanupComplete {
		t.Fatalf("cleanup depended on JSON publication: %+v %v", failed, err)
	}
	published, err := os.Stat(filepath.Join(store.Dir, failureName))
	if err != nil || !os.SameFile(reserved, published) {
		t.Fatalf("failure allocated a new record: %v", err)
	}
	for _, dir := range []string{status.ImagePath, status.FilestorePath} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("partial artifacts remain: %s: %v", dir, err)
		}
	}
	if err := os.Remove(statusPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(oldStatus, statusPath); err != nil {
		t.Fatal(err)
	}
	store.Close()
	got, err := Read(store.Dir)
	if err != nil || got.State != Failed || !got.CleanupComplete {
		t.Fatalf("failure was forgotten after status repair: %+v %v", got, err)
	}
}

func TestConcurrentOrphanQueries(t *testing.T) {
	store, status := newTestJob(t)
	if err := os.Mkdir(status.ImagePath, 0700); err != nil {
		t.Fatal(err)
	}
	store.Close()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := Read(store.Dir)
			if err != nil || got.State == Ready {
				t.Errorf("concurrent query: %+v %v", got, err)
			}
		}()
	}
	wg.Wait()
	got, err := Read(store.Dir)
	if err != nil || got.State != Failed || !got.CleanupComplete {
		t.Fatalf("orphan cleanup did not finish: %+v %v", got, err)
	}
}

func TestLegacyFailureWithoutReserve(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprint(blocked), func(t *testing.T) {
			store, status := newTestJob(t)
			if err := os.Remove(filepath.Join(store.Dir, failureReserveName)); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(status.ImagePath, 0700); err != nil {
				t.Fatal(err)
			}
			if blocked {
				statusPath := filepath.Join(store.Dir, statusName)
				if err := os.Remove(statusPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(statusPath, 0700); err != nil {
					t.Fatal(err)
				}
			}
			got, err := store.Fail(status, unix.ENOSPC)
			if blocked {
				if err == nil || got.CleanupComplete || got.CleanupError == "" {
					t.Fatalf("legacy job cleaned without durable failure: %+v %v", got, err)
				}
				if _, err := os.Stat(status.ImagePath); err != nil {
					t.Fatalf("legacy outputs lost after publication failure: %v", err)
				}
			} else if err != nil || !got.CleanupComplete {
				t.Fatalf("legacy cleanup failed: %+v %v", got, err)
			}
		})
	}
}

func TestInheritedLockDoesNotSurviveExec(t *testing.T) {
	store, _ := newTestJob(t)
	raw, err := unix.Dup(int(store.LockFile().Fd()))
	if err != nil {
		t.Fatal(err)
	}
	inherited, _, err := Inherit(store.Dir, os.NewFile(uintptr(raw), "worker-lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer inherited.Close()
	flags, err := unix.FcntlInt(inherited.LockFile().Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("worker lock is inherited across exec: %d %v", flags, err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^TestParkLockExecHelper$")
	child.Env = append(os.Environ(), "GVISOR_PARK_LOCK_HELPER=1")
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		child.Process.Kill()
		child.Wait()
	}()
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("helper not ready: %q %v", line, err)
	}
	store.Close()
	inherited.Close()
	// The helper is still alive. It must not keep the worker's lease alive.
	got, err := Read(store.Dir)
	if err != nil || got.State != Failed {
		t.Fatalf("exec child retained ownership: %+v %v", got, err)
	}
}

func TestParkLockExecHelper(t *testing.T) {
	if os.Getenv("GVISOR_PARK_LOCK_HELPER") != "1" {
		return
	}
	fmt.Fprintln(os.Stdout, "ready")
	var buf [1]byte
	io.ReadFull(os.Stdin, buf[:])
}
