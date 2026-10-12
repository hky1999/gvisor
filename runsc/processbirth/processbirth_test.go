// Copyright 2026 The gVisor Authors.
// SPDX-License-Identifier: Apache-2.0

package processbirth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func fixture() (Birth, observation, operations) {
	scope := Scope{BootID: "original-boot", PIDNamespaceDevice: 5, PIDNamespaceInode: 9}
	birth := Birth{Version: 1, ContainerID: "container-original", Generation: "generation-original", Role: "sentry",
		Scope: scope, PID: 123, StartTicks: 10, InitializerPID: 77,
		ExecutableSHA256: strings.Repeat("a", 64), ArgumentsSHA256: strings.Repeat("b", 64), CgroupSHA256: strings.Repeat("c", 64)}
	current := observation{pid: birth.PID, parent: birth.InitializerPID, start: birth.StartTicks,
		executable: birth.ExecutableSHA256, arguments: birth.ArgumentsSHA256, cgroup: birth.CgroupSHA256}
	ops := operations{
		scope:   func() (Scope, error) { return scope, nil },
		observe: func(context.Context, int) (*observation, error) { copy := current; return &copy, nil },
		open:    func(int) (int, error) { return 70, nil },
		poll:    func(int, int) (bool, error) { return false, nil },
		signal:  func(int, unix.Signal) error { return errors.New("unexpected signal") },
		close:   func(int) error { return nil }, initializerPID: 77,
	}
	return birth, current, ops
}

func TestCaptureRequiresTheActualLiveUnreapedChild(t *testing.T) {
	birth, current, ops := fixture()
	captured, err := captureChild(context.Background(), current.pid, birth.ContainerID, birth.Generation, birth.Role, ops)
	if err != nil || *captured != birth {
		t.Fatalf("capture: got %+v, %v", captured, err)
	}
	for _, kind := range []string{"foreign-parent", "already-exited", "missing", "replaced", "no-pidfd", "scope-change"} {
		t.Run(kind, func(t *testing.T) {
			_, current, ops := fixture()
			calls := 0
			switch kind {
			case "foreign-parent":
				current.parent++
			case "already-exited":
				current.exited = true
			case "missing":
				ops.observe = func(context.Context, int) (*observation, error) { return nil, nil }
			case "replaced":
				ops.observe = func(context.Context, int) (*observation, error) {
					calls++
					copy := current
					if calls > 1 {
						copy.start++
					}
					return &copy, nil
				}
			case "no-pidfd":
				ops.open = func(int) (int, error) { return -1, unix.ENOSYS }
			case "scope-change":
				original := birth.Scope
				ops.scope = func() (Scope, error) {
					calls++
					if calls > 1 {
						original.PIDNamespaceInode++
					}
					return original, nil
				}
			}
			if kind == "foreign-parent" || kind == "already-exited" {
				ops.observe = func(context.Context, int) (*observation, error) { return &current, nil }
			}
			if got, err := captureChild(context.Background(), birth.PID, birth.ContainerID, birth.Generation, birth.Role, ops); err == nil || got != nil {
				t.Fatalf("accepted invalid capture: %+v %v", got, err)
			}
		})
	}
}

func TestOpenRejectsReusedPIDAndChangedOriginalProcess(t *testing.T) {
	for _, kind := range []string{"start", "executable", "arguments", "cgroup", "boot", "namespace", "generation", "role", "id", "missing-proof", "unsupported-pidfd"} {
		t.Run(kind, func(t *testing.T) {
			birth, current, ops := fixture()
			switch kind {
			case "start":
				current.start++
			case "executable":
				current.executable = strings.Repeat("d", 64)
			case "arguments":
				current.arguments = strings.Repeat("d", 64)
			case "cgroup":
				current.cgroup = strings.Repeat("d", 64)
			case "boot":
				ops.scope = func() (Scope, error) { scope := birth.Scope; scope.BootID = "replacement"; return scope, nil }
			case "namespace":
				ops.scope = func() (Scope, error) { scope := birth.Scope; scope.PIDNamespaceInode++; return scope, nil }
			case "generation":
				birth.Generation = "replacement"
			case "role":
				birth.Role = "gofer"
			case "id":
				birth.ContainerID = "replacement"
			case "missing-proof":
				birth.Version = 0
			case "unsupported-pidfd":
				ops.open = func(int) (int, error) { return -1, unix.ENOSYS }
			}
			ops.observe = func(context.Context, int) (*observation, error) { return &current, nil }
			if got, err := openBirth(context.Background(), &birth, "container-original", "generation-original", "sentry", ops); err == nil || got != nil {
				t.Fatalf("accepted changed process: %+v %v", got, err)
			}
		})
	}
}

func TestOpenReplacementDuringPidfdAcquisitionClosesHandleWithoutSignal(t *testing.T) {
	birth, current, ops := fixture()
	closed, signals, observations := 0, 0, 0
	ops.observe = func(context.Context, int) (*observation, error) {
		observations++
		copy := current
		if observations > 1 {
			copy.start++
		}
		return &copy, nil
	}
	ops.close = func(fd int) error {
		if fd != 70 {
			t.Fatal("wrong handle")
		}
		closed++
		return nil
	}
	ops.signal = func(int, unix.Signal) error { signals++; return nil }
	got, err := openBirth(context.Background(), &birth, birth.ContainerID, birth.Generation, birth.Role, ops)
	if err == nil || got != nil || closed != 1 || signals != 0 {
		t.Fatalf("replacement was not fenced: %v %v close=%d signals=%d", got, err, closed, signals)
	}
}

func TestAcquiredHandleNeverLooksUpReusedNumericPIDDuringKill(t *testing.T) {
	birth, _, ops := fixture()
	observations, signals, polls, closed := 0, 0, 0, 0
	originalObserve := ops.observe
	ops.observe = func(ctx context.Context, pid int) (*observation, error) {
		observations++
		return originalObserve(ctx, pid)
	}
	ops.signal = func(fd int, signal unix.Signal) error {
		if fd != 70 || signal != unix.SIGKILL {
			t.Fatal("wrong process handle or signal")
		}
		signals++
		return nil
	}
	ops.poll = func(fd, milliseconds int) (bool, error) {
		if fd != 70 {
			t.Fatal("wrong poll handle")
		}
		polls++
		return polls > 1, nil
	}
	ops.close = func(fd int) error {
		if fd != 70 {
			t.Fatal("wrong close handle")
		}
		closed++
		return nil
	}
	handle, err := openBirth(context.Background(), &birth, birth.ContainerID, birth.Generation, birth.Role, ops)
	if err != nil {
		t.Fatal(err)
	}
	ops.observe = func(context.Context, int) (*observation, error) {
		t.Fatal("numeric PID lookup after acquiring original handle")
		return nil, nil
	}
	handle.ops.observe = ops.observe
	if err := handle.KillAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if observations != 2 || signals != 1 || closed != 1 {
		t.Fatalf("observations=%d signals=%d closed=%d", observations, signals, closed)
	}
}

func TestOriginalMissingOrZombieNeedsCompleteBirthAndSameScope(t *testing.T) {
	for _, zombie := range []bool{false, true} {
		birth, current, ops := fixture()
		current.exited = true
		ops.observe = func(context.Context, int) (*observation, error) {
			if zombie {
				return &current, nil
			}
			return nil, nil
		}
		ops.open = func(int) (int, error) { t.Fatal("dead original must not open replacement"); return -1, nil }
		handle, err := openBirth(context.Background(), &birth, birth.ContainerID, birth.Generation, birth.Role, ops)
		if err != nil {
			t.Fatal(err)
		}
		if err := handle.KillAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		handle.Close()
		birth.Version = 0
		if _, err := openBirth(context.Background(), &birth, birth.ContainerID, birth.Generation, birth.Role, ops); err == nil {
			t.Fatal("missing original proof became absence")
		}
	}
}

func TestKillCancellationPollAndSignalFailuresNeverBecomeCompletion(t *testing.T) {
	for _, kind := range []string{"cancelled", "poll-error", "signal-error", "cancel-while-waiting", "closed"} {
		t.Run(kind, func(t *testing.T) {
			birth, _, ops := fixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			handle, err := openBirth(ctx, &birth, birth.ContainerID, birth.Generation, birth.Role, ops)
			if err != nil {
				t.Fatal(err)
			}
			defer handle.Close()
			switch kind {
			case "cancelled":
				cancel()
			case "poll-error":
				handle.ops.poll = func(int, int) (bool, error) { return false, errors.New("poll unavailable") }
			case "signal-error":
				handle.ops.signal = func(int, unix.Signal) error { return unix.EPERM }
			case "cancel-while-waiting":
				handle.ops.signal = func(int, unix.Signal) error { cancel(); return nil }
			case "closed":
				handle.Close()
			}
			if err := handle.KillAndWait(ctx); err == nil {
				t.Fatal("unknown exit became completion")
			}
		})
	}
}

func TestProcessStatParsesParentAndStartWithParenthesesInName(t *testing.T) {
	data := "123 (runtime (worker)) S 77 " + strings.Repeat("0 ", 17) + "10 0"
	got, err := processStat([]byte(data), 123)
	if err != nil || got.pid != 123 || got.parent != 77 || got.start != 10 || got.exited {
		t.Fatalf("stat: %+v %v", got, err)
	}
	for _, data := range []string{"", "124 (runtime) S 77", "123 runtime", "123 (runtime) S 77", "123 (runtime) S -1 " + strings.Repeat("0 ", 17) + "10 0"} {
		if _, err := processStat([]byte(data), 123); err == nil {
			t.Fatal("invalid stat was accepted")
		}
	}
}
