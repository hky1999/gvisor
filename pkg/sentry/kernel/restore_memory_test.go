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
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"sync"
	"testing"
)

//go:noinline
func allocateRestoreGarbage() {
	b := make([]byte, 64<<20)
	for i := 0; i < len(b); i += 4096 {
		b[i] = 0x7d
	}
	runtime.KeepAlive(b)
}

func TestReclaimRestoreMemory(t *testing.T) {
	const child = "GVISOR_TEST_RESTORE_RECLAIM_CHILD"
	if os.Getenv(child) != "1" {
		// Isolate the collector policy and heap from other tests.
		cmd := exec.Command(os.Args[0], "-test.run=^TestReclaimRestoreMemory$", "-test.v")
		cmd.Env = append(os.Environ(), child+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("restore heap child failed: %v\n%s", err, out)
		} else {
			t.Logf("%s", out)
		}
		return
	}
	debug.SetGCPercent(-1)
	debug.FreeOSMemory()
	live := make([]byte, 8<<20)
	for i := 0; i < len(live); i += 4096 {
		live[i] = byte(i / 4096)
	}
	allocateRestoreGarbage()
	before, after := reclaimRestoreMemory()
	const reclaimed = 32 << 20
	if before.HeapAlloc < after.HeapAlloc+reclaimed {
		t.Fatalf("temporary decoder heap not collected: before=%d after=%d", before.HeapAlloc, after.HeapAlloc)
	}
	residentBefore := before.HeapSys - before.HeapReleased
	residentAfter := after.HeapSys - after.HeapReleased
	if residentBefore < residentAfter+reclaimed {
		t.Fatalf("unused heap not returned to host: before=%d after=%d", residentBefore, residentAfter)
	}
	if after.NumGC <= before.NumGC {
		t.Fatal("no collection occurred before external page loading")
	}
	for i := 0; i < len(live); i += 4096 {
		if live[i] != byte(i/4096) {
			t.Fatalf("live restored state changed at offset %d", i)
		}
	}
	runtime.KeepAlive(live)
	t.Logf("heap_alloc_before=%d heap_alloc_after=%d resident_before=%d resident_after=%d", before.HeapAlloc, after.HeapAlloc, residentBefore, residentAfter)
}

var decoderScratchPool sync.Pool

//go:noinline
func leaveDecoderScratchInPool() {
	b := make([]byte, 64<<20)
	for i := 0; i < len(b); i += 4096 {
		b[i] = 0x3d
	}
	decoderScratchPool.Put(&b)
}

func TestReclaimPooledDecoderMemory(t *testing.T) {
	const child = "GVISOR_TEST_POOLED_DECODER_RECLAIM_CHILD"
	if os.Getenv(child) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestReclaimPooledDecoderMemory$", "-test.v")
		cmd.Env = append(os.Environ(), child+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("pooled restore heap child failed: %v\n%s", err, out)
		} else {
			t.Logf("%s", out)
		}
		return
	}

	debug.SetGCPercent(-1)
	debug.FreeOSMemory()
	live := make([]byte, 8<<20)
	for i := 0; i < len(live); i += 4096 {
		live[i] = byte(i / 4096)
	}
	leaveDecoderScratchInPool()
	before, after := reclaimRestoreMemory()
	const expectedRelease = 32 << 20
	t.Logf("before_alloc=%d after_alloc=%d before_retained=%d after_retained=%d gc=%d->%d", before.HeapAlloc, after.HeapAlloc, before.HeapSys-before.HeapReleased, after.HeapSys-after.HeapReleased, before.NumGC, after.NumGC)
	if before.HeapAlloc < after.HeapAlloc+expectedRelease {
		t.Fatalf("pooled decoder scratch remains retained across page loading: before=%d after=%d", before.HeapAlloc, after.HeapAlloc)
	}
	if before.HeapSys-before.HeapReleased < after.HeapSys-after.HeapReleased+expectedRelease {
		t.Fatal("pooled decoder backing pages were not returned")
	}
	for i := 0; i < len(live); i += 4096 {
		if live[i] != byte(i/4096) {
			t.Fatal("live data changed")
		}
	}
	runtime.KeepAlive(live)
}
