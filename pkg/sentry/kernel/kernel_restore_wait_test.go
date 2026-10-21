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

package kernel

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

type checkpointNotification struct {
	gen CheckpointGeneration
	err error
}

// checkpointRegistrationIsBlocked waits for Register to park on the generation
// lock held by the caller. Observing its blocked stack, rather than sleeping,
// makes the subsequent waiter-lock probe independent of scheduler timing. Both
// versions can finish after the caller releases the generation lock, so the RED
// regression does not deliberately leave a deadlocked goroutine behind.
func checkpointRegistrationIsBlocked() bool {
	buf := make([]byte, 1<<20)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		stacks := string(buf[:runtime.Stack(buf, true)])
		for _, stack := range strings.Split(stacks, "\n\n") {
			if !strings.Contains(stack, "(*CheckpointWaitable).Register(") {
				continue
			}
			header := strings.SplitN(stack, "\n", 2)[0]
			if strings.Contains(header, "[semacquire") || strings.Contains(header, "[sync.Mutex.Lock") {
				return true
			}
		}
		runtime.Gosched()
	}
	return false
}

func TestCheckpointRegistrationLockOrder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		notify  func(*Kernel)
		restore bool
	}{
		{"restore", (*Kernel).IncCheckpointGenOnRestore, true},
		{"checkpoint", func(k *Kernel) { k.OnCheckpointAttempt(nil) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := &Kernel{}
			k.CheckpointWait.k = k
			observed := make(chan checkpointNotification, 2)
			registered := make(chan any, 1)

			k.checkpointMu.Lock()
			go func() {
				registered <- k.CheckpointWait.Register(func(gen CheckpointGeneration, err error) {
					observed <- checkpointNotification{gen, err}
				}, 1)
			}()
			parked := checkpointRegistrationIsBlocked()
			waiterAvailable := k.CheckpointWait.mu.TryLock()
			if waiterAvailable {
				k.CheckpointWait.mu.Unlock()
			}
			// Always let Register finish, even when testing the unfixed version.
			k.checkpointMu.Unlock()

			var key any
			select {
			case key = <-registered:
			case <-time.After(10 * time.Second):
				t.Fatal("Register did not finish after the generation lock was released")
			}
			defer k.CheckpointWait.Unregister(key)
			if !parked {
				t.Fatal("did not observe Register blocked on the held generation lock")
			}
			if !waiterAvailable {
				t.Error("Register holds the waiter lock while waiting for the generation lock; checkpoint notification uses the reverse order")
			}

			tc.notify(k)
			select {
			case got := <-observed:
				if got.gen.Count != 1 || got.gen.Restore != tc.restore || got.err != nil {
					t.Errorf("notification = %+v, want generation 1, restore=%t and no error", got, tc.restore)
				}
			default:
				t.Fatal("registered waiter missed checkpoint notification")
			}
			tc.notify(k)
			select {
			case got := <-observed:
				t.Errorf("waiter was called more than once: %+v", got)
			default:
			}
		})
	}
}

func TestCheckpointRegistrationAfterNotification(t *testing.T) {
	k := &Kernel{}
	k.CheckpointWait.k = k
	k.IncCheckpointGenOnRestore()
	observed := make(chan checkpointNotification, 2)
	key := k.CheckpointWait.Register(func(gen CheckpointGeneration, err error) {
		observed <- checkpointNotification{gen, err}
	}, 1)
	defer k.CheckpointWait.Unregister(key)
	select {
	case got := <-observed:
		if got.gen.Count != 1 || !got.gen.Restore || got.err != nil {
			t.Fatalf("immediate notification = %+v, want restored generation 1", got)
		}
	default:
		t.Fatal("registration missed the already completed checkpoint")
	}
	k.OnCheckpointAttempt(nil)
	select {
	case got := <-observed:
		t.Fatalf("already notified waiter was called again: %+v", got)
	default:
	}
}

func TestCheckpointWaiterThresholdUnregisterAndError(t *testing.T) {
	k := &Kernel{}
	k.CheckpointWait.k = k
	observed := make(chan checkpointNotification, 2)
	callback := func(gen CheckpointGeneration, err error) {
		observed <- checkpointNotification{gen, err}
	}
	key := k.CheckpointWait.Register(callback, 2)
	k.OnCheckpointAttempt(nil)
	select {
	case got := <-observed:
		t.Fatalf("waiter was called below its generation threshold: %+v", got)
	default:
	}
	k.CheckpointWait.Unregister(key)
	k.OnCheckpointAttempt(nil)
	select {
	case got := <-observed:
		t.Fatalf("unregistered waiter was called: %+v", got)
	default:
	}

	key = k.CheckpointWait.Register(callback, 3)
	defer k.CheckpointWait.Unregister(key)
	wantErr := errors.New("checkpoint failed for waiter regression")
	k.OnCheckpointAttempt(wantErr)
	select {
	case got := <-observed:
		if got.gen.Count != 3 || got.gen.Restore || got.err != wantErr {
			t.Fatalf("failure notification = %+v, want generation 3 with original error", got)
		}
	default:
		t.Fatal("waiter missed checkpoint failure")
	}
}
