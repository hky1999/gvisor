// Copyright 2026 The gVisor Authors.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// managedControlSocket is captured under the original native lifecycle lock.
// It grants file cleanup only after the caller proves the original cgroup
// offline; it never grants process, control-message or budget authority.
type managedControlSocket struct {
	root      string
	directory *os.File
	rootStat  unix.Stat_t
	socket    *os.File
	name      string
	original  unix.Stat_t
	absent    bool
}

func (c *Container) prepareManagedControlSocket(ctx context.Context) (*managedControlSocket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.Sandbox == nil {
		return nil, nil
	}
	if c.ResourceGeneration == "" || c.Sandbox.ResourceGeneration != c.ResourceGeneration ||
		c.Sandbox.ID != c.ID || !filepath.IsAbs(c.Saver.RootDir) || filepath.Clean(c.Saver.RootDir) != c.Saver.RootDir {
		return nil, errors.New("original managed control socket owner is incomplete")
	}
	name := "runsc-" + c.ID + ".sock"
	if c.Sandbox.ControlSocketPath != filepath.Join(c.Saver.RootDir, name) {
		return nil, errors.New("original managed control socket is outside its native state directory")
	}
	directory, original, err := originalStateDirectory(c.Saver.RootDir)
	if err != nil {
		return nil, err
	}
	lease := &managedControlSocket{root: c.Saver.RootDir, directory: directory, rootStat: original, name: name}
	ok := false
	defer func() {
		if !ok {
			lease.close()
		}
	}()
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		lease.absent = true
	} else if err != nil {
		return nil, err
	} else {
		lease.socket = os.NewFile(uintptr(fd), "original managed control socket")
		if err := unix.Fstat(fd, &lease.original); err != nil {
			return nil, err
		}
		if lease.original.Mode&unix.S_IFMT != unix.S_IFSOCK || lease.original.Uid != uint32(os.Geteuid()) ||
			lease.original.Nlink != 1 || lease.original.Mode&0022 != 0 {
			return nil, errors.New("original managed control endpoint is not a private single-link socket")
		}
	}
	if err := checkStateDirectory(lease.root, lease.rootStat); err != nil {
		return nil, err
	}
	ok = true
	return lease, nil
}

func (s *managedControlSocket) close() {
	if s == nil {
		return
	}
	if s.socket != nil {
		_ = s.socket.Close()
	}
	_ = s.directory.Close()
}

func sameManagedControlSocket(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode && a.Uid == b.Uid && a.Gid == b.Gid && a.Nlink == b.Nlink
}

// remove never unlinks a replacement at the canonical pathname. Move the
// candidate to a unique private name first, then compare it with the held
// original inode. A mismatching file is restored without overwriting anything.
func (s *managedControlSocket) remove(ctx context.Context) (result error) {
	if s == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkStateDirectory(s.root, s.rootStat); err != nil {
		return err
	}
	fd := int(s.directory.Fd())
	var current unix.Stat_t
	err := unix.Fstatat(fd, s.name, &current, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		if err := s.directory.Sync(); err != nil {
			return err
		}
		return checkStateDirectory(s.root, s.rootStat)
	}
	if err != nil {
		return err
	}
	if s.absent || !sameManagedControlSocket(s.original, current) {
		return errors.New("original managed control socket was replaced before retirement")
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	retired := ".scheduler-control-retire-" + hex.EncodeToString(random)
	if err := unix.Renameat2(fd, s.name, fd, retired, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	removed := false
	defer func() {
		if !removed {
			err := unix.Renameat2(fd, retired, fd, s.name, unix.RENAME_NOREPLACE)
			if err != nil {
				result = errors.Join(result, fmt.Errorf("control socket retained at private retirement name after failed restore: %w", err))
			}
			result = errors.Join(result, s.directory.Sync())
		}
	}()
	if err := unix.Fstatat(fd, retired, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		!sameManagedControlSocket(s.original, current) {
		return errors.New("original managed control socket changed during retirement")
	}
	if err := checkStateDirectory(s.root, s.rootStat); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := unix.Unlinkat(fd, retired, 0); err != nil {
		return err
	}
	removed = true
	if err := s.directory.Sync(); err != nil {
		return err
	}
	return checkStateDirectory(s.root, s.rootStat)
}
