// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package specutils

import (
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
)

// Duplicate-destination mounts whose Sources differ are an override, not a
// conflict: source paths embed per-sandbox container directories and vary
// across checkpoint restore. The comparison must ignore Source exactly like
// the single-mount branch does.
func TestValidateMountsAllowsDuplicateDestinationWithDifferentSources(t *testing.T) {
	oldMnts := []specs.Mount{
		{Destination: "/etc/resolv.conf", Type: "bind", Source: "/etc/resolv_akernel.conf", Options: []string{"bind", "ro"}},
		{Destination: "/etc/resolv.conf", Type: "bind", Source: "/containers/sbox-a/sandbox-files/resolv.conf", Options: []string{"bind", "ro"}},
		{Destination: "/etc/hosts", Type: "bind", Source: "/etc/base-hosts"},
	}
	newMnts := []specs.Mount{
		{Destination: "/etc/resolv.conf", Type: "bind", Source: "/etc/resolv_akernel.conf", Options: []string{"bind", "ro"}},
		{Destination: "/etc/resolv.conf", Type: "bind", Source: "/containers/sbox-b/sandbox-files/resolv.conf", Options: []string{"bind", "ro"}},
		{Destination: "/etc/hosts", Type: "bind", Source: "/etc/base-hosts"},
	}
	if err := validateMounts("Mounts", "__no_name_0", oldMnts, newMnts); err != nil {
		t.Fatalf("duplicate destination with varying sources rejected: %v", err)
	}
}

// Duplicates that differ beyond Source (type or options) are still a conflict.
func TestValidateMountsRejectsConflictingDuplicates(t *testing.T) {
	oldMnts := []specs.Mount{
		{Destination: "/etc/resolv.conf", Type: "bind", Source: "/a", Options: []string{"bind", "ro"}},
		{Destination: "/etc/resolv.conf", Type: "bind", Source: "/b", Options: []string{"bind", "rw"}},
	}
	newMnts := []specs.Mount{
		{Destination: "/etc/resolv.conf", Type: "bind", Source: "/a", Options: []string{"bind", "ro"}},
		{Destination: "/etc/resolv.conf", Type: "bind", Source: "/b", Options: []string{"bind", "rw"}},
	}
	if err := validateMounts("Mounts", "__no_name_0", oldMnts, newMnts); err == nil {
		t.Fatal("duplicate destination with conflicting options accepted")
	}
}
