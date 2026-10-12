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

package cmd

import (
	"os"
	"strings"
	"testing"

	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/flag"
)

func TestNotFound(t *testing.T) {
	f := flag.NewFlagSet("test", flag.ContinueOnError)
	f.Parse([]string{"123"})
	dir, err := os.MkdirTemp("", "metadata")
	if err != nil {
		t.Fatalf("error creating dir: %v", err)
	}
	conf := &config.Config{RootDir: dir}

	d := Delete{}
	if err := d.execute(f, conf); err == nil {
		t.Error("Deleting non-existent container should have failed")
	}

	d = Delete{force: true}
	if err := d.execute(f, conf); err != nil {
		t.Errorf("Deleting non-existent container with --force should NOT have failed: %v", err)
	}
}

func TestOfflineCgroupProofDescriptorsRequireOriginalGenerationAndExclusiveRole(t *testing.T) {
	binding := []string{"--scheduler-original-cgroup-path=/owned/original", "--scheduler-original-cgroup-device=3", "--scheduler-original-cgroup-inode=9"}
	cases := []struct {
		args        []string
		message     string
		withBinding bool
	}{
		{[]string{"--scheduler-offline-cgroup-parent-fd=3", "--expected-resource-generation=generation", "original"}, "requires the original lease path", false},
		{[]string{"--expected-resource-generation=generation", "original"}, "requires a parent proof", true},
		{[]string{"--scheduler-offline-cgroup-parent-fd=3", "original"}, "requires the original generation", true},
		{[]string{"--scheduler-offline-cgroup-parent-fd=2", "--expected-resource-generation=generation", "original"}, "requires the original generation", true},
		{[]string{"--scheduler-offline-cgroup-fd=3", "--scheduler-offline-cgroup-parent-fd=4", "--expected-resource-generation=generation", "original"}, "refuses combined", true},
		{[]string{"--scheduler-offline-cgroup-parent-fd=3", "--expected-resource-generation=generation", "original", "neighbor"}, "exactly one full", true},
	}
	for _, test := range cases {
		f := flag.NewFlagSet("test", flag.ContinueOnError)
		d := Delete{}
		d.SetFlags(f)
		args := test.args
		if test.withBinding {
			args = append(append([]string{}, binding...), args...)
		}
		if err := f.Parse(args); err != nil {
			t.Fatal(err)
		}
		err := d.execute(f, &config.Config{RootDir: t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), test.message) {
			t.Fatalf("invalid proof reached container loading instead of the required refusal: %v", err)
		}
	}
	f := flag.NewFlagSet("test", flag.ContinueOnError)
	d := Delete{}
	d.SetFlags(f)
	if err := f.Parse([]string{"original"}); err != nil {
		t.Fatal(err)
	}
	if d.offlineParentFD != 0 || d.offlineCgroupFD != 0 {
		t.Fatal("legacy deletion silently selected cgroup proof")
	}
}
