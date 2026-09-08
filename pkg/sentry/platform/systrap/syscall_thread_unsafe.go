// Copyright 2021 The gVisor Authors.
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

package systrap

import (
	"fmt"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/hostsyscall"
)

func (t *syscallThread) initRequestReplyAddresses(sentryStackAddr uintptr) {
	// These are safe as these addresses are mmapped and never moved/gced.
	sentryMessage := (*syscallSentryMessage)(unsafe.Pointer(sentryStackAddr))
	stubMessage := (*syscallStubMessage)(unsafe.Pointer(sentryStackAddr + syscallStubMessageOffset))
	atomic.StoreUint32(&sentryMessage.state, 0)

	t.sentryMessage = sentryMessage
	t.stubMessage = stubMessage
}

// maskAllSignals blocks all signals.
func (t *syscallThread) maskAllSignalsAttached() {
	p := t.thread

	mask := ^uint64(0)
	if errno := hostsyscall.RawSyscallErrno6(unix.SYS_PTRACE, linux.PTRACE_SETSIGMASK, uintptr(p.tid), 8, uintptr(unsafe.Pointer(&mask)), 0, 0); errno != 0 {
		panic(fmt.Sprintf("unable to setmask: %v", errno))
	}
}

// unmaskAllSignals unblocks all signals.
func (t *syscallThread) unmaskAllSignalsAttached() {
	p := t.thread
	mask := uint64(0)
	if errno := hostsyscall.RawSyscallErrno6(unix.SYS_PTRACE, linux.PTRACE_SETSIGMASK, uintptr(p.tid), 8, uintptr(unsafe.Pointer(&mask)), 0, 0); errno != 0 {
		panic(fmt.Sprintf("unable to setmask: %v", errno))
	}
}

func futexWakeUint32(addr *uint32) error {
	if e := hostsyscall.RawSyscallErrno(unix.SYS_FUTEX, uintptr(unsafe.Pointer(addr)), linux.FUTEX_WAKE, 1); e != 0 {
		return fmt.Errorf("failed to FUTEX_WAKE: %v", e)
	}
	return nil
}

func futexWaitForUint32(addr *uint32, targetValue uint32) error {
	for {
		val := atomic.LoadUint32(addr)
		if val == targetValue {
			break
		}

		_, _, e := unix.Syscall6(unix.SYS_FUTEX, uintptr(unsafe.Pointer(addr)), linux.FUTEX_WAIT, uintptr(val), 0, 0, 0)
		if e != 0 && e != unix.EAGAIN && e != unix.EINTR {
			return fmt.Errorf("failed to FUTEX_WAIT: %v", e)
		}
	}
	return nil
}

// futexWaitWake waits when other side will call FUTEX_WAKE. A value of the
// futex word has to be equal to futexValue and it must not be changed.
func futexWaitWake(futexAddr *uint32, futexValue uint32) error {
	for {
		_, _, e := unix.Syscall6(unix.SYS_FUTEX, uintptr(unsafe.Pointer(futexAddr)), linux.FUTEX_WAIT, uintptr(futexValue), 0, 0, 0)
		if e == 0 {
			break
		}
		if e != unix.EAGAIN && e != unix.EINTR {
			return fmt.Errorf("failed to FUTEX_WAIT: %v", e)
		}
	}

	return nil
}

func (t *syscallThread) kickSeccompNotify() unix.Errno {
	errno := hostsyscall.RawSyscallErrno(unix.SYS_IOCTL, uintptr(t.seccompNotify.Fd()),
		uintptr(linux.SECCOMP_IOCTL_NOTIF_SEND),
		uintptr(unsafe.Pointer(&t.seccompNotifyResp)))
	return errno
}

// maxSeccompNotifyENOENTRetries bounds how many times waitForSeccompNotify
// re-enters SECCOMP_IOCTL_NOTIF_RECV after the ioctl returned ENOENT while
// the stub still appeared alive. It only limits retries taken after an error
// return; it is not a timeout for the blocking ioctl itself, and it does not
// close the pre-existing window where the stub dies while RECV blocks
// without returning at all.
const maxSeccompNotifyENOENTRetries = 64

func (t *syscallThread) waitForSeccompNotify() error {
	enoentRetries := 0
	for {
		req := linux.SeccompNotif{}
		errno := hostsyscall.RawSyscallErrno(unix.SYS_IOCTL, uintptr(t.seccompNotify.Fd()),
			uintptr(linux.SECCOMP_IOCTL_NOTIF_RECV),
			uintptr(unsafe.Pointer(&req)))
		if errno == 0 {
			t.seccompNotifyResp.ID = req.ID
			break
		}
		if errno == unix.EINTR && t.subproc.alive() {
			continue
		}
		// ENOENT may indicate that the notification was interrupted and
		// revoked while the stub is still running. Re-enter the same RECV
		// only while the thread that installed this filter is observed
		// alive, and only a bounded number of times per call; beyond that
		// fall through to the original failure handling. EINTR retries are
		// not counted. The response ID is updated only on a successful
		// RECV, so a stale ID is never reused across retries.
		if errno == unix.ENOENT && t.subproc.alive() && enoentRetries < maxSeccompNotifyENOENTRetries {
			enoentRetries++
			if enoentRetries == 1 {
				t.thread.Warningf("seccomp notification receive interrupted (ENOENT); retrying while subprocess is alive")
			}
			continue
		}
		t.thread.kill()
		return fmt.Errorf("failed getting response from syscall thread : %w", errno)
	}
	return nil
}
