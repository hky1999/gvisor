// Copyright 2026 The gVisor Authors.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/runsc/config"
)

// 仅使用结构化身份和普通文件；不操作真实分组或发送信号。
func managedCgroupFixture() *ManagedLaunchCgroup {
	return &ManagedLaunchCgroup{Version: 1, Path: "/owned/original", Device: 3, Inode: 9, RootDevice: 3, RootInode: 1, ParentDevice: 3, ParentInode: 5, BootID: "controlled-boot", CgroupNamespaceDev: 4, CgroupNamespaceInode: 6, MountNamespaceDev: 4, MountNamespaceInode: 7, PIDNamespaceDev: 4, PIDNamespaceInode: 8}
}

func TestManagedLaunchCgroupRequiresCompleteOriginalDirectoryAndScope(t *testing.T) {
	if err := managedCgroupFixture().validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"version", "root", "relative", "jump", "device", "inode", "parent-device", "parent-inode", "root-inode", "boot", "cgroup-namespace", "mount-namespace", "pid-namespace"} {
		t.Run(change, func(t *testing.T) {
			b := managedCgroupFixture()
			switch change {
			case "version":
				b.Version = 2
			case "root":
				b.Path = "/"
			case "relative":
				b.Path = "owned/original"
			case "jump":
				b.Path = "/owned/../original"
			case "device":
				b.Device = 0
			case "inode":
				b.Inode = 0
			case "parent-device":
				b.ParentDevice++
			case "parent-inode":
				b.ParentInode = b.Inode
			case "root-inode":
				b.RootInode = b.Inode
			case "boot":
				b.BootID = ""
			case "cgroup-namespace":
				b.CgroupNamespaceInode = 0
			case "mount-namespace":
				b.MountNamespaceInode = 0
			case "pid-namespace":
				b.PIDNamespaceInode = 0
			}
			if err := b.validate(); err == nil {
				t.Fatal("incomplete or different scope was accepted")
			}
		})
	}
}

func TestManagedLaunchCgroupRefusesIndependentManagementAndHooksBeforeObservation(t *testing.T) {
	for _, kind := range []string{"runtime-management", "systemd", "parent", "hooks"} {
		t.Run(kind, func(t *testing.T) {
			conf := &config.Config{IgnoreCgroups: true}
			spec := &specs.Spec{Linux: &specs.Linux{CgroupsPath: "/owned/original"}, Annotations: map[string]string{}}
			switch kind {
			case "runtime-management":
				conf.IgnoreCgroups = false
			case "systemd":
				conf.SystemdCgroup = true
			case "parent":
				spec.Annotations[cgroupParentAnnotation] = ""
			case "hooks":
				spec.Hooks = &specs.Hooks{Poststop: []specs.Hook{{Path: "/must-not-run"}}}
			}
			if _, err := captureManagedLaunchCgroup(conf, spec); err == nil {
				t.Fatal("independent runtime effects were accepted")
			}
		})
	}
	if b, err := captureManagedLaunchCgroup(&config.Config{}, &specs.Spec{}); err != nil || b != nil {
		t.Fatalf("non-cgroup input changed: %v", err)
	}
}

func TestManagedLaunchCgroupReplacementCannotUseCachedRuntimeOwner(t *testing.T) {
	c := launchStageFixture(t)
	c.Spec.Linux = &specs.Linux{CgroupsPath: "/owned/original"}
	c.LaunchCgroup = managedCgroupFixture()
	publishLaunchFixture(t, c)
	var replacement Container
	if err := json.Unmarshal(readBirthOwnerBytes(t, c), &replacement); err != nil {
		t.Fatal(err)
	}
	replacement.LaunchCgroup.Inode++
	data, err := json.Marshal(&replacement)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Saver.statePath(), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.DestroyIfGeneration(context.Background(), c.ResourceGeneration); err == nil {
		t.Fatal("cached source accepted a different original group")
	}
	if string(readBirthOwnerBytes(t, c)) != string(data) {
		t.Fatal("replacement state was changed")
	}
}

func TestManagedOfflineCgroupCannotUseOrdinaryDirectoryOrWrongGeneration(t *testing.T) {
	c := launchStageFixture(t)
	c.Spec.Linux = &specs.Linux{CgroupsPath: "/owned/original"}
	b, err := managedCgroupScope()
	if err != nil {
		t.Fatal(err)
	}
	b.Path = "/owned/original"
	b.Device = 3
	b.Inode = 9
	b.RootDevice = 3
	b.RootInode = 1
	b.ParentDevice = 3
	b.ParentInode = 5
	c.LaunchCgroup = b
	c.RuntimeDispatched = true
	c.GoferRequired = true
	c.GoferLaunchState = processDispatching
	publishLaunchFixture(t, c)
	before := readBirthOwnerBytes(t, c)
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	for _, gen := range []string{c.ResourceGeneration, "generation-other"} {
		if err := c.DestroyIfGenerationAfterCgroupOffline(context.Background(), gen, int(directory.Fd())); err == nil {
			t.Fatal("ordinary empty directory authorized unknown launch cleanup")
		}
		if string(readBirthOwnerBytes(t, c)) != string(before) {
			t.Fatal("unknown original source was changed")
		}
	}
}

func TestManagedOfflineCgroupMissingSourceAndCancellationRetainUnknownLaunch(t *testing.T) {
	c := launchStageFixture(t)
	c.RuntimeDispatched = true
	c.SentryLaunchState = processDispatching
	publishLaunchFixture(t, c)
	before := readBirthOwnerBytes(t, c)
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := c.DestroyIfGenerationAfterCgroupOffline(context.Background(), c.ResourceGeneration, int(directory.Fd())); err == nil {
		t.Fatal("missing original group became offline proof")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.DestroyIfGenerationAfterCgroupOffline(ctx, c.ResourceGeneration, int(directory.Fd())); err == nil {
		t.Fatal("cancellation became cleanup completion")
	}
	if string(readBirthOwnerBytes(t, c)) != string(before) {
		t.Fatal("unknown original launch changed")
	}
}
