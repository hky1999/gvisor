// Copyright 2019 The gVisor Authors.
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

package container

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/sandbox"
)

func retirementFixture(t *testing.T) *Container {
	t.Helper()
	id := "reader-retirement"
	root := t.TempDir()
	return &Container{
		ID:      id,
		Spec:    &specs.Spec{Root: &specs.Root{Path: root}, Annotations: map[string]string{resourceGenerationAnnotation: "generation-a"}},
		Sandbox: &sandbox.Sandbox{ID: id, MountHints: &boot.PodMountHints{}},
		Status:  Running,
		Saver:   StateFile{RootDir: root, ID: FullID{SandboxID: id, ContainerID: id}},
	}
}

func saveRetirementFixture(t *testing.T, c *Container) {
	t.Helper()
	if err := c.Saver.lock(BlockAcquire); err != nil {
		t.Fatal(err)
	}
	defer c.Saver.UnlockOrDie()
	if err := c.Saver.SaveLocked(c); err != nil {
		t.Fatal(err)
	}
}

func namespaceReader(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "sleep 60 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWPID}
	if err := cmd.Start(); err != nil {
		if os.IsPermission(err) {
			t.Skipf("private PID namespace unavailable: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func nativeReader(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func assertReaderAlive(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := unix.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("owned reader was unexpectedly signalled: %v", err)
	}
	fd, err := unix.PidfdOpen(cmd.Process.Pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	p := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if n, err := unix.Poll(p, 0); err != nil || n != 0 {
		t.Fatalf("reader exited: poll=%d err=%v events=%x", n, err, p[0].Revents)
	}
}

func bindReaders(t *testing.T, c *Container, sentry, gofer *exec.Cmd) {
	t.Helper()
	c.Sandbox.Pid.Store(sentry.Process.Pid)
	if gofer != nil {
		c.GoferPid.Store(gofer.Process.Pid)
	}
	if err := c.captureRetirementIdentity(); err != nil {
		t.Fatal(err)
	}
	saveRetirementFixture(t, c)
}

func TestRetirementProcStatParsing(t *testing.T) {
	fields := append([]string{"S"}, make([]string, 19)...)
	for i := 1; i < len(fields); i++ {
		fields[i] = "0"
	}
	fields[19] = "123456"
	got, err := procStartTime("42 (reader ) unusual name) " + strings.Join(fields, " "))
	if err != nil || got != 123456 {
		t.Fatalf("start time=%d err=%v", got, err)
	}
	for _, raw := range []string{"", "42 (short) S", "42 (bad) " + strings.Repeat("0 ", 20)} {
		if _, err := procStartTime(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestRetirementRequiresGeneration(t *testing.T) {
	c := retirementFixture(t)
	if err := c.DestroyIfGeneration(""); err == nil {
		t.Fatal("empty generation accepted")
	}
}

func TestRetirementMissingStateIsNotProof(t *testing.T) {
	c := retirementFixture(t)
	if err := c.DestroyIfGeneration("generation-a"); err == nil {
		t.Fatal("missing state accepted as exit proof")
	}
}

func TestRetirementMissingBirthRetainsState(t *testing.T) {
	c := retirementFixture(t)
	saveRetirementFixture(t, c)
	if err := c.DestroyIfGeneration("generation-a"); err == nil {
		t.Fatal("legacy state accepted as exit proof")
	}
	if _, err := os.Stat(c.Saver.statePath()); err != nil {
		t.Fatalf("state removed: %v", err)
	}
}

func TestRetirementRejectsNonPrivateReader(t *testing.T) {
	c := retirementFixture(t)
	p := nativeReader(t)
	c.Sandbox.Pid.Store(p.Process.Pid)
	if err := c.captureRetirementIdentity(); err == nil {
		t.Fatal("host PID namespace accepted")
	}
	assertReaderAlive(t, p)
}

func TestRetirementGenerationMismatchDoesNotSignal(t *testing.T) {
	c := retirementFixture(t)
	p := namespaceReader(t)
	bindReaders(t, c, p, nil)
	if err := c.DestroyIfGeneration("other-generation"); err == nil {
		t.Fatal("generation mismatch accepted")
	}
	assertReaderAlive(t, p)
	if _, err := os.Stat(c.Saver.statePath()); err != nil {
		t.Fatalf("state removed: %v", err)
	}
}

func TestRetirementReloadsGenerationUnderLock(t *testing.T) {
	c := retirementFixture(t)
	p := namespaceReader(t)
	bindReaders(t, c, p, nil)
	stale := &Container{ID: c.ID, Saver: StateFile{RootDir: c.Saver.RootDir, ID: c.Saver.ID}, Spec: c.Spec}
	c.Spec.Annotations[resourceGenerationAnnotation] = "generation-b"
	c.RetirementIdentity.Generation = "generation-b"
	saveRetirementFixture(t, c)
	if err := stale.DestroyIfGeneration("generation-a"); err == nil {
		t.Fatal("stale object retired a new generation")
	}
	assertReaderAlive(t, p)
}

func TestRetirementInvalidGoferDoesNotSignalSentry(t *testing.T) {
	c := retirementFixture(t)
	p := namespaceReader(t)
	bindReaders(t, c, p, nil)
	bad := c.RetirementIdentity.Sentry
	bad.PID = 1
	c.RetirementIdentity.Gofer = &bad
	saveRetirementFixture(t, c)
	if err := c.DestroyIfGeneration("generation-a"); err == nil {
		t.Fatal("invalid second reader accepted")
	}
	assertReaderAlive(t, p)
}

func TestRetirementReusedPIDNotSignalled(t *testing.T) {
	p := namespaceReader(t)
	birth, err := readReaderBirth(p.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	birth.StartTime++
	fd, err := pinReader(birth)
	if err != nil || fd != -1 {
		t.Fatalf("reused PID pinned: fd=%d err=%v", fd, err)
	}
	assertReaderAlive(t, p)
}

func TestRetirementKernelWaitIsNotKillSubmission(t *testing.T) {
	p := nativeReader(t)
	fd, err := unix.PidfdOpen(p.Process.Pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := waitReaderExit(fd, time.Now().Add(10*time.Millisecond)); err == nil {
		t.Fatal("live process accepted as exited")
	}
	assertReaderAlive(t, p)
	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := waitReaderExit(fd, time.Now().Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func descendantPIDFD(t *testing.T, pid int) int {
	t.Helper()
	file := fmt.Sprintf("/proc/%d/task/%d/children", pid, pid)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(string(raw))
		if len(fields) != 0 {
			child, err := strconv.Atoi(fields[0])
			if err != nil {
				t.Fatal(err)
			}
			fd, err := unix.PidfdOpen(child, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { unix.Close(fd) })
			return fd
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("owned PID namespace did not create a descendant")
	return -1
}

func TestRetirementWaitsForBothNamespacesAndDescendants(t *testing.T) {
	c := retirementFixture(t)
	sentry, gofer := namespaceReader(t), namespaceReader(t)
	bindReaders(t, c, sentry, gofer)
	sfd, gfd := descendantPIDFD(t, sentry.Process.Pid), descendantPIDFD(t, gofer.Process.Pid)
	lockBefore, err := os.Stat(c.Saver.lockPath())
	if err != nil {
		t.Fatal(err)
	}
	// A newly loaded object models a different runsc process, not an in-memory
	// pointer held by the original creator.
	reopened := &Container{ID: c.ID, Saver: StateFile{RootDir: c.Saver.RootDir, ID: c.Saver.ID}}
	if err := reopened.DestroyIfGeneration("generation-a"); err != nil {
		t.Fatal(err)
	}
	for _, fd := range []int{sfd, gfd} {
		if err := waitReaderExit(fd, time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(c.Saver.statePath()); !os.IsNotExist(err) {
		t.Fatalf("state remains: %v", err)
	}
	lockAfter, err := os.Stat(c.Saver.lockPath())
	if err != nil || !os.SameFile(lockBefore, lockAfter) {
		t.Fatalf("creation lock inode changed: %v", err)
	}
	if err := reopened.DestroyIfGeneration("generation-a"); err == nil {
		t.Fatal("missing state was converted into a fresh proof")
	}
}

func TestRetirementSaveUsesCompletePrivateState(t *testing.T) {
	c := retirementFixture(t)
	saveRetirementFixture(t, c)
	info, err := os.Stat(c.Saver.statePath())
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("state permissions: %v %v", info, err)
	}
	files, err := filepath.Glob(filepath.Join(c.Saver.RootDir, ".state-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary state leaked: %v %v", files, err)
	}
}

func TestRetirementRejectsChangedObservation(t *testing.T) {
	cases := []struct{ name, key, value string }{
		{"boot", "bootID", "00000000-0000-0000-0000-000000000000"},
		{"observer", "observerPIDNamespace", "pid:[different-observer]"},
		{"proc-view", "procPIDNamespace", "pid:[different-proc-view]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := retirementFixture(t)
			p := namespaceReader(t)
			bindReaders(t, c, p, nil)
			raw, err := os.ReadFile(c.Saver.statePath())
			if err != nil {
				t.Fatal(err)
			}
			var state map[string]any
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			identity, ok := state["akernelRetirementIdentity"].(map[string]any)
			if !ok {
				t.Fatal("missing identity fixture")
			}
			identity[tc.key] = tc.value
			if err := c.Saver.lock(BlockAcquire); err != nil {
				t.Fatal(err)
			}
			err = c.Saver.SaveLocked(state)
			c.Saver.UnlockOrDie()
			if err != nil {
				t.Fatal(err)
			}
			if err := c.DestroyIfGeneration("generation-a"); err == nil {
				t.Fatal("changed process observation was accepted as retirement proof")
			}
			assertReaderAlive(t, p)
			if _, err := os.Stat(c.Saver.statePath()); err != nil {
				t.Fatalf("state removed: %v", err)
			}
		})
	}
}
