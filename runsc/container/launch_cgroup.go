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

// OriginalCgroupIdentity is supplied from the original generation lease when
// the original directory descriptor was lost. It cannot come from new state.
type OriginalCgroupIdentity struct {
	Path   string
	Device uint64
	Inode  uint64
}

func (original OriginalCgroupIdentity) valid() bool {
	return filepath.IsAbs(original.Path) && filepath.Clean(original.Path) == original.Path && original.Path != "/" &&
		len(original.Path) < 4096 && !strings.ContainsAny(original.Path, "\x00\r\n") && original.Device != 0 && original.Inode != 0
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

// verifyParentOffline is a restart proof for an original group whose directory
// descriptor was lost. Only the producer's original parent, current original
// mount and scope, and absence of the exact original entry can authorize it.
// cgroup v2 does not allow rename; absence under that original parent cannot
// mean that this same kernel group was renamed elsewhere.
func (b *ManagedLaunchCgroup) verifyParentOffline(ctx context.Context, fd int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.validate(); err != nil {
		return err
	}
	if fd < 3 {
		return errors.New("original cgroup parent descriptor is invalid")
	}
	if err := b.verifyScope(); err != nil {
		return err
	}
	parent, err := managedCgroupDirectory(fd)
	if err != nil || parent.Dev != b.ParentDevice || parent.Ino != b.ParentInode {
		return errors.New("original cgroup parent descriptor differs from the producer identity")
	}
	root, err := unix.Open("/sys/fs/cgroup", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	rootStat, err := managedCgroupDirectory(root)
	if err != nil || rootStat.Dev != b.RootDevice || rootStat.Ino != b.RootInode {
		return errors.New("original cgroup mount root changed before parent absence verification")
	}
	path := strings.TrimPrefix(filepath.Dir(b.Path), "/")
	if path == "" {
		path = "."
	}
	current, err := unix.Openat2(root, path, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV})
	if err != nil {
		return err
	}
	defer unix.Close(current)
	currentStat, err := managedCgroupDirectory(current)
	if err != nil || currentStat.Dev != b.ParentDevice || currentStat.Ino != b.ParentInode {
		return errors.New("original cgroup parent entry changed before absence verification")
	}
	for _, originalParent := range []int{fd, current} {
		var entry unix.Stat_t
		err := unix.Fstatat(originalParent, filepath.Base(b.Path), &entry, unix.AT_SYMLINK_NOFOLLOW)
		if !errors.Is(err, unix.ENOENT) {
			return errors.New("original cgroup name is present or its absence is unknown")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.verifyScope()
}

// This validator accepts a well-formed unknown dispatch only for an independently
// proven offline original group. It never grants process or control authority.
func (c *Container) verifyCgroupCleanupSource() error {
	if c.Spec == nil || c.Spec.Linux == nil || c.LaunchCgroup == nil || c.LaunchCgroup.Path != c.Spec.Linux.CgroupsPath || c.ProcessLaunchVersion != 1 {
		return errors.New("original offline cgroup source differs from its managed OCI path or launch protocol")
	}
	valid := func(stage string) bool {
		return stage == processNotDispatched || stage == processDispatching || stage == processBirthPublished
	}
	if !valid(c.GoferLaunchState) || !valid(c.SentryLaunchState) ||
		c.RuntimeDispatched != (c.GoferLaunchState != processNotDispatched || c.SentryLaunchState != processNotDispatched) ||
		c.GoferRequired != (c.GoferLaunchState != processNotDispatched) {
		return errors.New("original cgroup cleanup launch facts are missing or contradictory")
	}
	if _, override := c.Spec.Annotations[cgroupParentAnnotation]; override {
		return errors.New("original cgroup cleanup cannot adopt an independent sandbox parent")
	}
	if h := c.Spec.Hooks; h != nil && len(h.Prestart)+len(h.CreateRuntime)+len(h.CreateContainer)+len(h.StartContainer)+len(h.Poststart)+len(h.Poststop) != 0 {
		return errors.New("original cgroup cleanup does not support external lifecycle hooks")
	}
	if c.GoferLaunchState == processBirthPublished {
		if err := c.GoferBirth.Validate(c.ID, c.ResourceGeneration, "gofer"); err != nil || c.GoferBirth.PID != c.GoferPid.Load() {
			return errors.New("published gofer source differs from its original birth")
		}
	} else if c.GoferBirth != nil || c.GoferPid.Load() != 0 {
		return errors.New("unpublished gofer contains contradictory process facts")
	}
	if c.GoferRequired && c.GoferLaunchState != processBirthPublished && c.SentryLaunchState != processNotDispatched {
		return errors.New("sentry dispatch preceded required gofer publication")
	}
	if c.SentryLaunchState == processBirthPublished {
		if c.Sandbox == nil || c.Sandbox.ResourceGeneration != c.ResourceGeneration || c.Sandbox.ProcessBirth == nil {
			return errors.New("published sentry source is incomplete")
		}
		if err := c.Sandbox.ProcessBirth.Validate(c.ID, c.ResourceGeneration, "sentry"); err != nil || c.Sandbox.ProcessBirth.PID != c.Sandbox.Pid.Load() {
			return errors.New("published sentry source differs from its original birth")
		}
	} else if c.Sandbox != nil {
		return errors.New("unpublished sentry contains contradictory sandbox facts")
	}
	return c.LaunchCgroup.validate()
}
