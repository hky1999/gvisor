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

package boot

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRestoreEarlyGuardsReturnOriginalError drives the REAL
// containerManager.Restore early validation. Before restoreDone was created
// before the early-failure defer, every guard failure panicked inside
// onRestoreFailed (nil-cond SIGSEGV) and surfaced upstream as an urpc EOF
// instead of the actual error. These calls must return the original guard
// error, record it on the loader, and expose it to waitRestore.
func TestRestoreEarlyGuardsReturnOriginalError(t *testing.T) {
	const consistencyErr = "split filesystem restore flag and donated files must be consistent"
	cases := []struct {
		name    string
		donated bool
		flag    bool
	}{
		// pc-snapshot-term-2 shape: Create donated fs files, Restore flag
		// missing on the wire.
		{"donated-fs-without-flag", true, false},
		{"flag-without-donated-fs", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &Loader{state: created}
			if tc.donated {
				l.fsRestore = &fsRestore{}
			}
			cm := &containerManager{l: l}

			err := cm.Restore(&RestoreOpts{SplitFSRestore: tc.flag}, nil)
			if err == nil {
				t.Fatal("Restore succeeded, want consistency error")
			}
			if !strings.Contains(err.Error(), consistencyErr) {
				t.Fatalf("Restore error = %v, want %q", err, consistencyErr)
			}
			if l.state != restoreFailed {
				t.Fatalf("loader state = %v, want restoreFailed", l.state)
			}
			if l.restoreErr == nil || !strings.Contains(l.restoreErr.Error(), consistencyErr) {
				t.Fatalf("loader restoreErr = %v, want the original guard error", l.restoreErr)
			}
			// A waiter observes the ORIGINAL error through waitRestore; on
			// the pre-fix code this test never got here (panic).
			if werr := l.waitRestore(); werr == nil || !strings.Contains(werr.Error(), consistencyErr) {
				t.Fatalf("waitRestore = %v, want the original guard error", werr)
			}
		})
	}
}

// signaledLocker wraps the loader mutex for wait-registration proof: the
// first Cond.Wait entry Unlocks through this wrapper, which closes
// enteredWait exactly once. The main goroutine therefore knows the waiter
// is genuinely blocked inside Wait before the failure fires — no sleeps, no
// ordering guesses, and deleting the Broadcast in onRestoreFailed makes the
// test fail on the timeout instead of passing on a state short-circuit.
type signaledLocker struct {
	mu          *sync.Mutex
	enteredWait chan struct{}
	once        sync.Once
}

func (s *signaledLocker) Lock() { s.mu.Lock() }

func (s *signaledLocker) Unlock() {
	s.once.Do(func() { close(s.enteredWait) })
	s.mu.Unlock()
}

// TestRestoreEarlyFailureWakesRegisteredWaiter covers the Broadcast side of
// onRestoreFailed for a waiter that is already blocked in waitRestore: it
// must wake and receive the recorded error rather than deadlock. This is
// complementary to the real Restore guard test above, which exercises the
// failure path end to end.
func TestRestoreEarlyFailureWakesRegisteredWaiter(t *testing.T) {
	l := &Loader{state: restoringUnstarted}
	lock := &signaledLocker{mu: &l.mu, enteredWait: make(chan struct{})}
	l.restoreDone = sync.NewCond(lock)
	cm := &containerManager{l: l}

	woken := make(chan error, 1)
	go func() {
		woken <- l.waitRestore()
	}()
	select {
	case <-lock.enteredWait:
		// The waiter is now blocked inside Cond.Wait; only a Broadcast can
		// release it from here.
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never entered Cond.Wait")
	}
	cm.onRestoreFailed(&testRestoreError{})
	select {
	case err := <-woken:
		if err == nil || err.Error() != "early restore failure" {
			t.Fatalf("waiter error = %v, want the recorded early restore failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("registered waiter did not wake from onRestoreFailed")
	}
	if l.state != restoreFailed {
		t.Fatalf("loader state = %v, want restoreFailed", l.state)
	}
}

type testRestoreError struct{}

func (*testRestoreError) Error() string { return "early restore failure" }
