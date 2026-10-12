// Copyright 2026 The gVisor Authors.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/config"
)

// ManagedLaunchCgroup 是派发任何子进程前保存的实际分组身份。
// 此记录单独不证明分组退出，也不授权预算归还。
type ManagedLaunchCgroup struct {
	Version              int    `json:"version"`
	Path                 string `json:"path"`
	Device               uint64 `json:"device"`
	Inode                uint64 `json:"inode"`
	RootDevice           uint64 `json:"root_device"`
	RootInode            uint64 `json:"root_inode"`
	ParentDevice         uint64 `json:"parent_device"`
	ParentInode          uint64 `json:"parent_inode"`
	BootID               string `json:"boot_id"`
	CgroupNamespaceDev   uint64 `json:"cgroup_namespace_device"`
	CgroupNamespaceInode uint64 `json:"cgroup_namespace_inode"`
	MountNamespaceDev    uint64 `json:"mount_namespace_device"`
	MountNamespaceInode  uint64 `json:"mount_namespace_inode"`
	PIDNamespaceDev      uint64 `json:"pid_namespace_device"`
	PIDNamespaceInode    uint64 `json:"pid_namespace_inode"`
}

func (b *ManagedLaunchCgroup) validate() error {
	if b == nil || b.Version != 1 || !filepath.IsAbs(b.Path) || filepath.Clean(b.Path) != b.Path || b.Path == "/" || len(b.Path) >= 4096 || strings.ContainsAny(b.Path, "\x00\r\n") || b.Device == 0 || b.Inode == 0 || b.RootDevice != b.Device || b.ParentDevice != b.Device || b.RootInode == 0 || b.ParentInode == 0 || b.Inode == b.RootInode || b.Inode == b.ParentInode || b.BootID == "" || len(b.BootID) > 128 || b.CgroupNamespaceInode == 0 || b.MountNamespaceInode == 0 || b.PIDNamespaceInode == 0 {
		return errors.New("original managed launch cgroup identity is incomplete")
	}
	return nil
}

func managedCgroupScope() (*ManagedLaunchCgroup, error) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || len(boot) > 128 || strings.TrimSpace(string(boot)) == "" {
		return nil, errors.New("original managed cgroup boot identity is unavailable")
	}
	b := &ManagedLaunchCgroup{Version: 1, BootID: strings.TrimSpace(string(boot))}
	for _, ns := range []struct {
		name       string
		dev, inode *uint64
	}{
		{"cgroup", &b.CgroupNamespaceDev, &b.CgroupNamespaceInode},
		{"mnt", &b.MountNamespaceDev, &b.MountNamespaceInode},
		{"pid", &b.PIDNamespaceDev, &b.PIDNamespaceInode},
	} {
		var stat unix.Stat_t
		if err := unix.Stat("/proc/thread-self/ns/"+ns.name, &stat); err != nil {
			return nil, err
		}
		*ns.dev, *ns.inode = stat.Dev, stat.Ino
	}
	return b, nil
}

func (b *ManagedLaunchCgroup) verifyScope() error {
	current, err := managedCgroupScope()
	if err != nil {
		return err
	}
	if current.BootID != b.BootID || current.CgroupNamespaceDev != b.CgroupNamespaceDev || current.CgroupNamespaceInode != b.CgroupNamespaceInode || current.MountNamespaceDev != b.MountNamespaceDev || current.MountNamespaceInode != b.MountNamespaceInode || current.PIDNamespaceDev != b.PIDNamespaceDev || current.PIDNamespaceInode != b.PIDNamespaceInode {
		return errors.New("original managed cgroup observer scope changed")
	}
	return nil
}

func managedCgroupDirectory(fd int) (unix.Stat_t, error) {
	var stat unix.Stat_t
	var fs unix.Statfs_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return stat, err
	}
	if err := unix.Fstatfs(fd, &fs); err != nil || fs.Type != unix.CGROUP2_SUPER_MAGIC || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0022 != 0 {
		return stat, errors.New("original managed group is not an owned cgroup v2 directory")
	}
	return stat, nil
}

func captureManagedLaunchCgroup(conf *config.Config, spec *specs.Spec) (*ManagedLaunchCgroup, error) {
	if spec == nil || spec.Linux == nil || spec.Linux.CgroupsPath == "" {
		return nil, nil
	}
	if !conf.IgnoreCgroups || conf.SystemdCgroup {
		return nil, errors.New("managed launch cgroup requires inherited scheduler ownership without runtime cgroup management")
	}
	if _, override := spec.Annotations[cgroupParentAnnotation]; override {
		return nil, errors.New("managed launch cgroup cannot use an independent sandbox parent")
	}
	if spec.Hooks != nil && (len(spec.Hooks.Prestart)+len(spec.Hooks.CreateRuntime)+len(spec.Hooks.CreateContainer)+len(spec.Hooks.StartContainer)+len(spec.Hooks.Poststart)+len(spec.Hooks.Poststop) != 0) {
		return nil, errors.New("managed launch cgroup does not support external OCI lifecycle hooks")
	}
	b, err := managedCgroupScope()
	if err != nil {
		return nil, err
	}
	b.Path = spec.Linux.CgroupsPath
	if !filepath.IsAbs(b.Path) || filepath.Clean(b.Path) != b.Path || b.Path == "/" || len(b.Path) >= 4096 || strings.ContainsAny(b.Path, "\x00\r\n") {
		return nil, errors.New("managed launch cgroup path is not an exact non-root absolute path")
	}
	root, err := unix.Open("/sys/fs/cgroup", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(root)
	open := func(path string) (int, error) {
		return unix.Openat2(root, path, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV})
	}
	group, err := open(strings.TrimPrefix(b.Path, "/"))
	if err != nil {
		return nil, err
	}
	defer unix.Close(group)
	parentPath := strings.TrimPrefix(filepath.Dir(b.Path), "/")
	if parentPath == "" {
		parentPath = "."
	}
	parent, err := open(parentPath)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parent)
	for _, entry := range []struct {
		fd         int
		dev, inode *uint64
	}{{root, &b.RootDevice, &b.RootInode}, {parent, &b.ParentDevice, &b.ParentInode}, {group, &b.Device, &b.Inode}} {
		stat, err := managedCgroupDirectory(entry.fd)
		if err != nil {
			return nil, err
		}
		*entry.dev, *entry.inode = stat.Dev, stat.Ino
	}
	actual, err := os.ReadFile("/proc/thread-self/cgroup")
	if err != nil || len(actual) > 65536 || string(actual) != "0::"+b.Path+"\n" {
		return nil, errors.New("actual creator does not belong to the original prepared cgroup")
	}
	for _, name := range []string{"cgroup.procs", "cgroup.events"} {
		fd, err := unix.Openat(group, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("original managed cgroup control %s is unavailable: %w", name, err)
		}
		unix.Close(fd)
	}
	var current unix.Stat_t
	if err := unix.Fstatat(parent, filepath.Base(b.Path), &current, unix.AT_SYMLINK_NOFOLLOW); err != nil || current.Dev != b.Device || current.Ino != b.Inode {
		return nil, errors.New("original launch cgroup directory entry changed")
	}
	if err := b.validate(); err != nil {
		return nil, err
	}
	return b, b.verifyScope()
}

// verifyOffline 只接受原内核目录句柄，不能借同名路径或空库存授权。
// cgroup v2 原目录被删除后，其必需控制入口均不可再打开。
func (b *ManagedLaunchCgroup) verifyOffline(ctx context.Context, fd int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.validate(); err != nil {
		return err
	}
	if fd < 3 {
		return errors.New("original offline cgroup descriptor is invalid")
	}
	if err := b.verifyScope(); err != nil {
		return err
	}
	stat, err := managedCgroupDirectory(fd)
	if err != nil || stat.Dev != b.Device || stat.Ino != b.Inode {
		return errors.New("offline cgroup descriptor differs from the original producer identity")
	}
	for _, name := range []string{"cgroup.procs", "cgroup.events"} {
		control, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err == nil {
			unix.Close(control)
			return errors.New("original cgroup is still online; empty membership is insufficient")
		}
		if !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("original cgroup offline state is unknown: %w", err)
		}
	}
	return b.verifyScope()
}
