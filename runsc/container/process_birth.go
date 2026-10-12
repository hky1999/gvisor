// Copyright 2026 The gVisor Authors.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/processbirth"
)

const managedRuntimeStateLimit = 8 << 20

const (
	processNotDispatched  = "not_dispatched"
	processDispatching    = "dispatching"
	processBirthPublished = "birth_published"
)

func originalStateDirectory(path string) (*os.File, unix.Stat_t, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, unix.Stat_t{}, err
	}
	file := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0022 != 0 {
		file.Close()
		return nil, unix.Stat_t{}, errors.New("managed runtime state directory is not privately owned")
	}
	return file, stat, nil
}

func checkStateDirectory(path string, original unix.Stat_t) error {
	var current unix.Stat_t
	if err := unix.Lstat(path, &current); err != nil || current.Dev != original.Dev || current.Ino != original.Ino || current.Mode != original.Mode || current.Uid != original.Uid {
		return errors.New("original managed runtime state directory changed")
	}
	return nil
}

// SaveLockedDurable publishes native producer facts before another process can
// receive ownership. Failure keeps dispatch or completion unconfirmed.
func (s *StateFile) SaveLockedDurable(value any) error {
	if !s.flock.Locked() {
		return errors.New("original managed runtime state lock is not held")
	}
	data, err := json.Marshal(value)
	if err != nil || len(data) > managedRuntimeStateLimit {
		return errors.New("managed runtime state is invalid or exceeds its limit")
	}
	directory, original, err := originalStateDirectory(s.RootDir)
	if err != nil {
		return err
	}
	defer directory.Close()
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	name := ".scheduler-birth-" + hex.EncodeToString(random)
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0640)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	defer unix.Unlinkat(int(directory.Fd()), name, 0)
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := checkStateDirectory(s.RootDir, original); err != nil {
		return err
	}
	if err := unix.Renameat(int(directory.Fd()), name, int(directory.Fd()), filepath.Base(s.statePath())); err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		return err
	}
	return checkStateDirectory(s.RootDir, original)
}

func noDuplicateStateJSON(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("managed runtime state nesting exceeds its limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return errors.New("managed runtime state JSON is incomplete")
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delimiter == '{' {
		keys := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			text, ok := key.(string)
			if err != nil || !ok || keys[text] {
				return errors.New("managed runtime state has duplicate or invalid fields")
			}
			keys[text] = true
			if err := noDuplicateStateJSON(decoder, depth+1); err != nil {
				return err
			}
		}
	} else if delimiter == '[' {
		for decoder.More() {
			if err := noDuplicateStateJSON(decoder, depth+1); err != nil {
				return err
			}
		}
	} else {
		return errors.New("managed runtime state delimiter is invalid")
	}
	_, err = decoder.Token()
	return err
}

func readLockedManagedState(s *StateFile) (*Container, error) {
	directory, original, err := originalStateDirectory(s.RootDir)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	name := filepath.Base(s.statePath())
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var before, after, current unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Mode&0022 != 0 ||
		before.Uid != uint32(os.Geteuid()) || before.Nlink != 1 || before.Size <= 0 || before.Size > managedRuntimeStateLimit {
		return nil, errors.New("original managed state is not private bounded regular data")
	}
	data, err := io.ReadAll(io.LimitReader(file, managedRuntimeStateLimit+1))
	if err != nil || len(data) > managedRuntimeStateLimit {
		return nil, errors.New("original managed state is unreadable")
	}
	if err := unix.Fstat(fd, &after); err != nil {
		return nil, err
	}
	if err := unix.Fstatat(int(directory.Fd()), name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		before.Dev != after.Dev || before.Ino != after.Ino || before.Size != after.Size || before.Mtim != after.Mtim ||
		before.Ctim != after.Ctim || before.Mode != after.Mode || before.Uid != after.Uid || before.Nlink != after.Nlink ||
		after.Dev != current.Dev || after.Ino != current.Ino || after.Size != current.Size || after.Mtim != current.Mtim ||
		after.Ctim != current.Ctim || after.Mode != current.Mode || after.Uid != current.Uid || after.Nlink != current.Nlink {
		return nil, errors.New("original managed state changed while reading")
	}
	if err := noDuplicateStateJSON(json.NewDecoder(bytes.NewReader(data)), 0); err != nil {
		return nil, err
	}
	var value Container
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, errors.New("original managed state is malformed")
	}
	if err := checkStateDirectory(s.RootDir, original); err != nil {
		return nil, err
	}
	return &value, nil
}

// The caller holds the original native state lock. Comparing the new on-disk
// owner prevents FetchSpec's cached container from deleting a replacement.
func (c *Container) verifyLockedProcessOwner(expected string) error {
	if expected == "" {
		expected = c.ResourceGeneration
	}
	if !processbirth.ValidBinding(c.ID, expected) || c.ResourceGeneration != expected || c.ProcessBirthVersion != 1 ||
		c.Spec == nil || c.Spec.Annotations[processbirth.GenerationAnnotation] != expected ||
		c.Saver.ID.ContainerID != c.ID || c.Saver.ID.SandboxID != c.ID {
		return errors.New("original managed runtime owner or generation is incomplete")
	}
	current, err := readLockedManagedState(&c.Saver)
	if err != nil {
		return err
	}
	if current.ID != c.ID || current.ResourceGeneration != expected || current.ProcessBirthVersion != 1 ||
		current.RuntimeDispatched != c.RuntimeDispatched || current.GoferRequired != c.GoferRequired ||
		current.ProcessLaunchVersion != c.ProcessLaunchVersion || current.GoferLaunchState != c.GoferLaunchState || current.SentryLaunchState != c.SentryLaunchState ||
		!reflect.DeepEqual(current.LaunchCgroup, c.LaunchCgroup) ||
		current.GoferPid.Load() != c.GoferPid.Load() || current.BundleDir != c.BundleDir ||
		!reflect.DeepEqual(current.Spec, c.Spec) || !reflect.DeepEqual(current.GoferBirth, c.GoferBirth) {
		return errors.New("original managed runtime state was replaced after loading")
	}
	if (current.Sandbox == nil) != (c.Sandbox == nil) {
		return errors.New("original managed sandbox source differs")
	}
	if c.Sandbox != nil && (current.Sandbox.ID != c.ID || c.Sandbox.ID != c.ID ||
		current.Sandbox.ResourceGeneration != expected || c.Sandbox.ResourceGeneration != expected ||
		current.Sandbox.Pid.Load() != c.Sandbox.Pid.Load() ||
		!reflect.DeepEqual(current.Sandbox.ProcessBirth, c.Sandbox.ProcessBirth)) {
		return errors.New("original managed sentry source differs")
	}
	return nil
}

// Open every required original handle before the first signal. A changed
// gofer must not result in stopping the sentry before the mismatch is found.
func (c *Container) stopManagedRuntime(ctx context.Context) error {
	if c.ProcessBirthVersion != 1 {
		return errors.New("original runtime process birth protocol is missing")
	}
	needSentry, needGofer, err := c.managedProcessRequirements()
	if err != nil {
		return err
	}
	if !needSentry && !needGofer {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var sentry *processbirth.Handle
	if needSentry {
		sentry, err = processbirth.Open(ctx, c.Sandbox.ProcessBirth, c.ID, c.ResourceGeneration, "sentry")
		if err != nil {
			return err
		}
		defer sentry.Close()
	}
	var gofer *processbirth.Handle
	if needGofer {
		gofer, err = processbirth.Open(ctx, c.GoferBirth, c.ID, c.ResourceGeneration, "gofer")
		if err != nil {
			return err
		}
		defer gofer.Close()
	}
	if sentry != nil {
		if err := c.Sandbox.DestroyOriginal(ctx, sentry); err != nil {
			return err
		}
	}
	if gofer != nil {
		if err := gofer.KillAndWait(ctx); err != nil {
			return err
		}
	}
	c.GoferPid.Store(0)
	c.Sandbox = nil
	// The scheduler retains ownership of its cgroup and performs its own
	// generation-bound retirement. Do not uninstall it through a path-only
	// native runtime record after the original processes exit.
	return nil
}

// This classifies producer facts only. Any returned process still needs its
// original kernel handle and complete birth verification before a signal.
func (c *Container) managedProcessRequirements() (bool, bool, error) {
	if c.ProcessLaunchVersion == 0 {
		if c.GoferLaunchState != "" || c.SentryLaunchState != "" {
			return false, false, errors.New("legacy process owner contains unsupported launch-stage facts")
		}
		if !c.RuntimeDispatched {
			if c.Sandbox != nil || c.GoferPid.Load() != 0 || c.GoferBirth != nil {
				return false, false, errors.New("undispatched runtime has contradictory process facts")
			}
			return false, false, nil
		}
		if c.Sandbox == nil || c.Sandbox.ProcessBirth == nil || c.Sandbox.ResourceGeneration != c.ResourceGeneration {
			return false, false, errors.New("original sentry birth is incomplete")
		}
	} else if c.ProcessLaunchVersion == 1 {
		valid := func(stage string) bool {
			return stage == processNotDispatched || stage == processDispatching || stage == processBirthPublished
		}
		if !valid(c.GoferLaunchState) || !valid(c.SentryLaunchState) {
			return false, false, errors.New("original process launch stage is missing or invalid")
		}
		dispatched := c.GoferLaunchState != processNotDispatched || c.SentryLaunchState != processNotDispatched
		if c.RuntimeDispatched != dispatched || c.GoferRequired != (c.GoferLaunchState != processNotDispatched) {
			return false, false, errors.New("original process launch facts contradict dispatch ownership")
		}
		if c.GoferLaunchState == processDispatching || c.SentryLaunchState == processDispatching {
			return false, false, errors.New("original process dispatch lacks a published birth; outcome remains unknown")
		}
		if c.SentryLaunchState == processNotDispatched {
			if c.Sandbox != nil {
				return false, false, errors.New("undispatched sentry has contradictory sandbox facts")
			}
		} else if c.Sandbox == nil || c.Sandbox.ProcessBirth == nil || c.Sandbox.ResourceGeneration != c.ResourceGeneration {
			return false, false, errors.New("original sentry birth is incomplete")
		}
	} else {
		return false, false, errors.New("original process launch protocol is unsupported")
	}
	if c.GoferRequired {
		if c.GoferBirth == nil || (c.GoferPid.Load() != c.GoferBirth.PID && (c.ProcessLaunchVersion != 0 || c.GoferPid.Load() != 0)) {
			return false, false, errors.New("original gofer birth is incomplete")
		}
	} else if c.GoferBirth != nil || c.GoferPid.Load() != 0 {
		return false, false, errors.New("runtime no-gofer fact contradicts native ownership")
	}
	return c.Sandbox != nil, c.GoferRequired, nil
}

func (c *Container) lockManagedDeletion(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := c.Saver.lock(TryAcquire)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrStateFileLocked) {
			return err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
