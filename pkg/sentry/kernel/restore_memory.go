// Copyright 2018 The gVisor Authors.
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

// Package kernel provides an emulation of the Linux kernel.
//
// See README.md for a detailed overview.
//
// Lock order (outermost locks must be taken first):
//
//	Kernel.extMu
//	  TTY.mu
//	  timekeeperTcpipTimer.mu
//	  ThreadGroup.timerMu
//	    Locks acquired by ktime.Timer methods
//	      TaskSet.mu
//	        SignalHandlers.mu
//	          Task.mu
//	            FSContext.mu
//	      runningTasksMu
//	        Timekeeper.updateMu
//
// Locking SignalHandlers.mu in multiple SignalHandlers requires locking
// TaskSet.mu exclusively first. Locking Task.mu in multiple Tasks at the same
// time requires locking all of their signal mutexes first.
package kernel

import (
	"runtime"
	"runtime/debug"
)

// reclaimRestoreMemory releases temporary Go allocations from kernel decoding
// before synchronous MemoryFile loading faults application pages into memory.
// Those pages are outside the Go heap, so loading them need not trigger a GC
// or prompt the scavenger to return the decoder's unused heap to the host.
func reclaimRestoreMemory() (before, after runtime.MemStats) {
	runtime.ReadMemStats(&before)
	// One GC moves sync.Pool entries into its victim cache. A second cycle
	// is required to discard that cache before external page memory grows.
	runtime.GC()
	debug.FreeOSMemory()
	runtime.ReadMemStats(&after)
	return
}
