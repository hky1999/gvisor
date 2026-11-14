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

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gvisor.dev/gvisor/runsc/cgroup"
	"gvisor.dev/gvisor/runsc/config"
)

func TestSandboxProcessEnvStartupCPUHint(t *testing.T) {
	t.Setenv("GOMAXPROCS", "99")
	t.Setenv("RUNSC_HOST_SENTINEL", "must-not-reach-sentry")
	for _, unsafe := range []bool{false, true} {
		for _, cpus := range []int{0, 2, 112} {
			t.Run(fmt.Sprintf("unsafe=%t/cpus=%d", unsafe, cpus), func(t *testing.T) {
				conf := &config.Config{TestOnlyAllowRunAsCurrentUserWithoutChroot: unsafe}
				env := sandboxProcessEnv(conf, sandboxProcessEnvOptions{startupCPUs: cpus, sentryUsesCgo: true})
				values := make(map[string]string)
				for _, entry := range env {
					key, value, ok := strings.Cut(entry, "=")
					if ok {
						if _, exists := values[key]; exists {
							t.Fatalf("duplicate environment key %q", key)
						}
						values[key] = value
					}
				}
				if unsafe {
					if values["GOMAXPROCS"] != "99" || values["RUNSC_HOST_SENTINEL"] != "must-not-reach-sentry" {
						t.Fatal("unsafe test environment changed its scheduler setting or sentinel")
					}
				} else {
					if _, leaked := values["RUNSC_HOST_SENTINEL"]; leaked {
						t.Fatal("host environment leaked into Sentry")
					}
					want := ""
					if cpus > 0 {
						want = fmt.Sprint(cpus)
					}
					if values["GOMAXPROCS"] != want {
						t.Fatalf("startup GOMAXPROCS = %q, want %q", values["GOMAXPROCS"], want)
					}
				}
				if values["GLIBC_TUNABLES"] != "glibc.pthread.rseq=0" {
					t.Fatal("lost cgo rseq protection")
				}
			})
		}
	}
}

func TestGetGCSURIFromImagePath(t *testing.T) {
	tmpDir := t.TempDir()

	testCases := []struct {
		name      string
		content   string
		writeOpts bool
		want      string
	}{
		{
			name:      "missing file",
			writeOpts: false,
			want:      "",
		},
		{
			name:      "invalid json",
			content:   "not valid json",
			writeOpts: true,
			want:      "",
		},
		{
			name:      "empty bucket",
			content:   `{"bucket": ""}`,
			writeOpts: true,
			want:      "",
		},
		{
			name:      "bucket only",
			content:   `{"bucket": "my-test-bucket"}`,
			writeOpts: true,
			want:      "gs://my-test-bucket",
		},
		{
			name:      "bucket with object prefix",
			content:   `{"bucket": "my-test-bucket", "object_prefix": "snapshots/test/"}`,
			writeOpts: true,
			want:      "gs://my-test-bucket/snapshots/test/",
		},
		{
			name:      "bucket with leading slash in object prefix",
			content:   `{"bucket": "my-test-bucket", "object_prefix": "/snapshots/test/"}`,
			writeOpts: true,
			want:      "gs://my-test-bucket/snapshots/test/",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			subDir := filepath.Join(tmpDir, tc.name)
			if err := os.MkdirAll(subDir, 0755); err != nil {
				t.Fatalf("failed to create directory: %v", err)
			}
			if tc.writeOpts {
				optsPath := filepath.Join(subDir, checkpointGCSOptsFileName)
				if err := os.WriteFile(optsPath, []byte(tc.content), 0644); err != nil {
					t.Fatalf("failed to write %s: %v", optsPath, err)
				}
			}
			got := getGCSURIFromImagePath(subDir)
			if got != tc.want {
				t.Errorf("getGCSURIFromImagePath(%q) = %q, want %q", subDir, got, tc.want)
			}
		})
	}
}

type fakeCgroup struct {
	cgroup.Cgroup
	numCPU    int
	numCPUErr error
	cpuQuota  int64
	cpuPeriod int64
}

func (f *fakeCgroup) NumCPU() (int, error) {
	return f.numCPU, f.numCPUErr
}

func (f *fakeCgroup) CPUQuota() (int64, error) {
	return f.cpuQuota, nil
}

func (f *fakeCgroup) CPUPeriod() (int64, error) {
	return f.cpuPeriod, nil
}

func TestCalculateCPUNum(t *testing.T) {
	for _, tc := range []struct {
		name            string
		numCPU          int
		numCPUErr       error
		cpuQuota        int64
		cpuPeriod       int64
		cpuNumFromQuota bool
		want            int
	}{
		{
			name:      "cgroup NumCPU error fallback to runtime.NumCPU",
			numCPUErr: errors.New("cgroup cpuset read error"),
			want:      runtime.NumCPU(),
		},
		{
			name:   "cgroup NumCPU success",
			numCPU: 8,
			want:   8,
		},
		{
			name:            "cgroup NumCPU error fallback with quota limit",
			numCPUErr:       errors.New("cgroup cpuset read error"),
			cpuQuota:        400000,
			cpuPeriod:       100000,
			cpuNumFromQuota: true,
			want:            min(runtime.NumCPU(), 4),
		},
		{
			name:            "cgroup NumCPU error fallback with low quota minCPUs floor",
			numCPUErr:       errors.New("cgroup cpuset read error"),
			cpuQuota:        100000,
			cpuPeriod:       100000,
			cpuNumFromQuota: true,
			want:            min(runtime.NumCPU(), 2),
		},
		{
			name:            "cgroup NumCPU success with quota limit",
			numCPU:          16,
			cpuQuota:        400000,
			cpuPeriod:       100000,
			cpuNumFromQuota: true,
			want:            4,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cg := &fakeCgroup{
				numCPU:    tc.numCPU,
				numCPUErr: tc.numCPUErr,
				cpuQuota:  tc.cpuQuota,
				cpuPeriod: tc.cpuPeriod,
			}
			gotNum, gotQuota, gotPeriod, err := calculateCPUNum(cg, tc.cpuNumFromQuota)
			if err != nil {
				t.Fatalf("calculateCPUNum failed: %v", err)
			}
			if gotNum != tc.want {
				t.Errorf("calculateCPUNum() got cpuNum = %d, want %d", gotNum, tc.want)
			}
			if gotQuota != tc.cpuQuota {
				t.Errorf("calculateCPUNum() got cpuQuota = %d, want %d", gotQuota, tc.cpuQuota)
			}
			if gotPeriod != tc.cpuPeriod {
				t.Errorf("calculateCPUNum() got cpuPeriod = %d, want %d", gotPeriod, tc.cpuPeriod)
			}
		})
	}
}
