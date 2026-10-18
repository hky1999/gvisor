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
	"strings"

	"golang.org/x/sys/unix"
)

const controlRetirementPrefix = ".scheduler-control-retire-"

// A retirement intent is file authority for one original generation only.
// The caller must independently prove its original cgroup offline before remove.
type controlSocketRetirement struct {
	Version            int                   `json:"version"`
	ContainerID        string                `json:"containerID"`
	ResourceGeneration string                `json:"resourceGeneration"`
	RootPath           string                `json:"rootPath"`
	RootDevice         uint64                `json:"rootDevice"`
	RootInode          uint64                `json:"rootInode"`
	RootMode           uint32                `json:"rootMode"`
	RootUID            uint32                `json:"rootUID"`
	RootGID            uint32                `json:"rootGID"`
	CanonicalName      string                `json:"canonicalName"`
	PrivateName        string                `json:"privateName"`
	Phase              string                `json:"phase"`
	Socket             controlSocketIdentity `json:"socket"`
}

type controlSocketIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Mode   uint32 `json:"mode"`
	UID    uint32 `json:"uid"`
	GID    uint32 `json:"gid"`
	Links  uint64 `json:"links"`
}

func socketIdentity(s unix.Stat_t) controlSocketIdentity {
	return controlSocketIdentity{Device: s.Dev, Inode: s.Ino, Mode: s.Mode, UID: s.Uid, GID: s.Gid, Links: uint64(s.Nlink)}
}

func (s controlSocketIdentity) private() bool {
	return s.Device != 0 && s.Inode != 0 && s.Mode&unix.S_IFMT == unix.S_IFSOCK &&
		s.UID == uint32(os.Geteuid()) && s.Links == 1 && s.Mode&0022 == 0
}

func (i *controlSocketRetirement) validate(c *Container, root unix.Stat_t, canonical string) error {
	suffix := strings.TrimPrefix(i.PrivateName, controlRetirementPrefix)
	random, err := hex.DecodeString(suffix)
	if i.Version != 1 || i.ContainerID != c.ID || i.ResourceGeneration != c.ResourceGeneration ||
		i.RootPath != c.Saver.RootDir || i.RootDevice != root.Dev || i.RootInode != root.Ino ||
		i.RootMode != root.Mode || i.RootUID != root.Uid || i.RootGID != root.Gid ||
		i.CanonicalName != canonical || !strings.HasPrefix(i.PrivateName, controlRetirementPrefix) ||
		err != nil || len(random) != 16 || hex.EncodeToString(random) != suffix ||
		(i.Phase != "prepared" && i.Phase != "renamed") || !i.Socket.private() || i.Socket.Device != root.Dev {
		return errors.New("original managed control socket retirement intent is invalid")
	}
	return nil
}

// managedControlSocket grants exact file cleanup only. It grants no process,
// control-message, cgroup or budget authority.
type managedControlSocket struct {
	owner     *Container
	root      string
	directory *os.File
	rootStat  unix.Stat_t
	socket    *os.File
	name      string
	original  unix.Stat_t
	absent    bool
	retired   bool
	intent    *controlSocketRetirement
}

func (c *Container) prepareManagedControlSocket(ctx context.Context) (*managedControlSocket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.Saver.flock == nil || !c.Saver.flock.Locked() {
		return nil, errors.New("original managed control socket state lock is not held")
	}
	if c.Sandbox == nil {
		if c.ControlSocketRetirement != nil {
			return nil, errors.New("pending control socket retirement has no original sandbox")
		}
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
	lease := &managedControlSocket{
		owner: c, root: c.Saver.RootDir, directory: directory, rootStat: original,
		name: name, intent: c.ControlSocketRetirement, retired: c.ControlSocketRetired,
	}
	ok := false
	defer func() {
		if !ok {
			lease.close()
		}
	}()
	if lease.retired {
		if lease.intent != nil {
			return nil, errors.New("completed control socket retirement still has a pending intent")
		}
		// Do not capture a later canonical socket after durable completion.
		ok = true
		return lease, nil
	}
	target := name
	if lease.intent != nil {
		if err := lease.intent.validate(c, original, name); err != nil {
			return nil, err
		}
		target = lease.intent.PrivateName
	}
	fd, err := unix.Openat(int(directory.Fd()), target, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) && lease.intent != nil {
		fd, err = unix.Openat(int(directory.Fd()), name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	if errors.Is(err, unix.ENOENT) {
		lease.absent = true
	} else if err != nil {
		return nil, err
	} else {
		lease.socket = os.NewFile(uintptr(fd), "original managed control socket")
		if err := unix.Fstat(fd, &lease.original); err != nil {
			return nil, err
		}
		if lease.intent != nil && socketIdentity(lease.original) != lease.intent.Socket {
			// A missing private entry may mean unlink completed before the
			// final state save. Never borrow the replacement canonical inode.
			if target != name {
				var private unix.Stat_t
				if err := unix.Fstatat(int(directory.Fd()), target, &private, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
					return nil, errors.New("recorded private control socket was replaced")
				}
				lease.absent = true
			}
		} else if !socketIdentity(lease.original).private() {
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
	return socketIdentity(a) == socketIdentity(b)
}

func (s *managedControlSocket) finish(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkStateDirectory(s.root, s.rootStat); err != nil {
		return err
	}
	// Persist a completion witness with the original owner. Merely clearing
	// the intent could let a later retry capture a replacement canonical socket.
	s.owner.ControlSocketRetirement = nil
	s.owner.ControlSocketRetired = true
	if err := s.owner.Saver.saveLockedDurableAt(s.owner, s.directory, s.rootStat); err != nil {
		return err
	}
	s.retired = true
	return nil
}

// remove replays only the recorded private name and original socket identity.
// Prepared state is durable before rename; deletion and its parent directory
// are durable before the pending intent is replaced by a completion witness.
func (s *managedControlSocket) remove(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkStateDirectory(s.root, s.rootStat); err != nil {
		return err
	}
	if s.retired {
		if s.socket != nil {
			var current unix.Stat_t
			err := unix.Fstatat(int(s.directory.Fd()), s.name, &current, unix.AT_SYMLINK_NOFOLLOW)
			if err == nil && !sameManagedControlSocket(s.original, current) {
				return errors.New("original managed control socket was replaced after retirement")
			}
			if err != nil && !errors.Is(err, unix.ENOENT) {
				return err
			}
		}
		return s.finish(ctx)
	}
	fd := int(s.directory.Fd())
	if s.intent == nil {
		var current unix.Stat_t
		err := unix.Fstatat(fd, s.name, &current, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) {
			if err := s.directory.Sync(); err != nil {
				return err
			}
			return s.finish(ctx)
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
		s.intent = &controlSocketRetirement{
			Version: 1, ContainerID: s.owner.ID, ResourceGeneration: s.owner.ResourceGeneration,
			RootPath: s.root, RootDevice: s.rootStat.Dev, RootInode: s.rootStat.Ino,
			RootMode: s.rootStat.Mode, RootUID: s.rootStat.Uid, RootGID: s.rootStat.Gid,
			CanonicalName: s.name, PrivateName: controlRetirementPrefix + hex.EncodeToString(random),
			Phase: "prepared", Socket: socketIdentity(s.original),
		}
		s.owner.ControlSocketRetirement = s.intent
		if err := s.owner.Saver.saveLockedDurableAt(s.owner, s.directory, s.rootStat); err != nil {
			return err
		}
	}
	if err := s.intent.validate(s.owner, s.rootStat, s.name); err != nil {
		return err
	}
	var current unix.Stat_t
	err := unix.Fstatat(fd, s.intent.PrivateName, &current, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		err = unix.Fstatat(fd, s.name, &current, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) || (err == nil && socketIdentity(current) != s.intent.Socket) {
			if err := s.directory.Sync(); err != nil {
				return err
			}
			return s.finish(ctx)
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := unix.Renameat2(fd, s.name, fd, s.intent.PrivateName, unix.RENAME_NOREPLACE); err != nil {
			return err
		}
		if err := unix.Fstatat(fd, s.intent.PrivateName, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
			socketIdentity(current) != s.intent.Socket {
			restore := unix.Renameat2(fd, s.intent.PrivateName, fd, s.name, unix.RENAME_NOREPLACE)
			return errors.Join(errors.New("original control socket changed during retirement"), restore, s.directory.Sync())
		}
	} else if err != nil {
		return err
	} else if socketIdentity(current) != s.intent.Socket {
		return errors.New("recorded private control socket was replaced before retirement")
	}
	if err := s.directory.Sync(); err != nil {
		return err
	}
	s.intent.Phase = "renamed"
	s.owner.ControlSocketRetirement = s.intent
	if err := s.owner.Saver.saveLockedDurableAt(s.owner, s.directory, s.rootStat); err != nil {
		return err
	}
	if err := checkStateDirectory(s.root, s.rootStat); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := unix.Fstatat(fd, s.intent.PrivateName, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		socketIdentity(current) != s.intent.Socket {
		return errors.New("original private control socket changed before unlink")
	}
	if err := unix.Unlinkat(fd, s.intent.PrivateName, 0); err != nil {
		return fmt.Errorf("unlinking original private control socket: %w", err)
	}
	if err := s.directory.Sync(); err != nil {
		return err
	}
	return s.finish(ctx)
}
