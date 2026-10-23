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

package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/unet"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/park"
	"gvisor.dev/gvisor/runsc/sandbox"
)

type parkRequest struct {
	Version int
	Config  *config.Config
	ID      FullID
	Opts    sandbox.CheckpointOpts
}

func (c *Container) validatePark(conf *config.Config, opts sandbox.CheckpointOpts) ([]checkpoint.ResourceID, error) {
	if opts.Resume || opts.FilestoreSnapshotDir != "" || opts.ParkFilestores != nil {
		return nil, fmt.Errorf("park requires a non-resuming save without a snapshot directory")
	}
	if opts.CudaCheckpointPath != "" {
		return nil, fmt.Errorf("asynchronous park does not support GPU checkpoints")
	}
	if c.Sandbox == nil || c.ID != c.Sandbox.ID || c.Sandbox.NoRootContainer {
		return nil, fmt.Errorf("park requires a single-container sandbox")
	}
	if err := c.requireStatus("park", Running); err != nil {
		return nil, err
	}
	ids, err := List(conf.RootDir)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if id.SandboxID == c.Sandbox.ID && id.ContainerID != c.ID {
			return nil, fmt.Errorf("park requires a single-container sandbox")
		}
	}
	targets, err := c.filestoreTargets()
	if err != nil {
		return nil, err
	}
	if len(targets) > 1024 {
		return nil, fmt.Errorf("park supports at most 1024 filestores")
	}
	return targets, nil
}

// StartPark starts a detached worker. The returned task owns its lifetime;
// callers wait separately for source exit or durable completion.
func (c *Container) StartPark(conf *config.Config, dir string, opts sandbox.CheckpointOpts) (park.Status, error) {
	if _, err := c.validatePark(conf, opts); err != nil {
		return park.Status{}, err
	}
	store, status, err := park.Create(dir, c.ID)
	if err != nil {
		return status, err
	}
	defer store.Close()
	fail := func(err error) (park.Status, error) {
		status.State = park.Failed
		status.Error = err.Error()
		if cleanupErr := store.RemoveArtifacts(); cleanupErr != nil {
			status.CleanupError = cleanupErr.Error()
		} else {
			status.CleanupComplete = true
		}
		return status, errors.Join(err, store.Write(status, true))
	}
	opts.SkipFilestorePages = true
	request := parkRequest{Version: 1, Config: conf, ID: c.Saver.ID, Opts: opts}
	if err := park.WriteJSON(store.Dir, "request.json", request, false); err != nil {
		return fail(err)
	}
	log, err := os.OpenFile(filepath.Join(store.Dir, "worker.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fail(err)
	}
	defer log.Close()
	executable, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	args := append(conf.ToFlags(), "park-worker", "--job-dir="+store.Dir, "--lock-fd=3")
	command := exec.Command(executable, args...)
	command.ExtraFiles = []*os.File{store.LockFile()}
	command.Stdout = log
	command.Stderr = log
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return fail(err)
	}
	go func() { command.Wait() }()
	return status, nil
}

func cloneParkFile(dest, source *os.File) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, dest.Fd(), unix.FICLONE, source.Fd())
	if errno != 0 {
		return errno
	}
	return nil
}

func probeParkReflink(dir string) error {
	source, err := os.CreateTemp(dir, ".probe-source-")
	if err != nil {
		return err
	}
	defer source.Close()
	defer os.Remove(source.Name())
	dest, err := os.CreateTemp(dir, ".probe-dest-")
	if err != nil {
		return err
	}
	defer dest.Close()
	defer os.Remove(dest.Name())
	if err := cloneParkFile(dest, source); err != nil {
		return fmt.Errorf("park requires a reflink-capable filesystem: %w", err)
	}
	return nil
}

// RunParkWorker owns the inherited task lock until completion or failure.
// It receives backing FDs through the save RPC's private channel, not /proc.
func RunParkWorker(dir string, lock *os.File) (retErr error) {
	store, status, err := park.Inherit(dir, lock)
	if err != nil {
		return err
	}
	defer store.Close()
	if status.State != park.Preparing {
		return fmt.Errorf("park job has already started")
	}
	var files []*os.File
	closeFiles := func() {
		for _, file := range files {
			file.Close()
		}
		files = nil
	}
	defer func() {
		closeFiles()
		if retErr != nil {
			status.State = park.Failed
			status.DurableAt = nil
			status.Error = retErr.Error()
			// A durable failure record must replace any visible READY before deleting
			// outputs. If publication fails, leave artifacts for later reconciliation.
			if err := store.Write(status, true); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("publishing failure; outputs retained: %w", err))
				return
			}
			if err := store.RemoveArtifacts(); err != nil {
				status.CleanupError = err.Error()
			} else {
				status.CleanupComplete = true
			}
			retErr = errors.Join(retErr, store.Write(status, true))
		}
	}()
	status.WorkerPID = os.Getpid()
	if err := store.Write(status, false); err != nil {
		return err
	}
	requestFile, err := os.OpenFile(filepath.Join(dir, "request.json"), os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	var request parkRequest
	err = json.NewDecoder(requestFile).Decode(&request)
	requestFile.Close()
	if err != nil {
		return err
	}
	if request.Version != 1 || request.Config == nil || request.ID.ContainerID != status.ContainerID {
		return fmt.Errorf("invalid park request")
	}
	if err := request.Config.Validate(); err != nil {
		return err
	}
	c, err := Load(request.Config.RootDir, request.ID, LoadOpts{Exact: true})
	if err != nil {
		return err
	}
	if err := c.Saver.lock(BlockAcquire); err != nil {
		return err
	}
	locked := true
	defer func() {
		if locked {
			c.Saver.UnlockOrDie()
		}
	}()
	if err := c.Saver.loadLocked(c); err != nil {
		return err
	}
	c.Sandbox.SetRootDir(request.Config.RootDir)
	targets, err := c.validatePark(request.Config, request.Opts)
	if err != nil {
		return err
	}
	if err := os.Mkdir(status.ImagePath, 0700); err != nil {
		return err
	}
	if err := os.Mkdir(status.FilestorePath, 0700); err != nil {
		return err
	}
	if len(targets) > 0 {
		if err := probeParkReflink(status.FilestorePath); err != nil {
			return err
		}
	}
	var targetStat unix.Stat_t
	if err := unix.Stat(status.FilestorePath, &targetStat); err != nil {
		return err
	}
	expected := make(map[checkpoint.ResourceID]bool, len(targets))
	for _, id := range targets {
		expected[id] = true
	}
	pidfd, err := unix.PidfdOpen(c.Sandbox.Pid.Load(), 0)
	if err != nil {
		return err
	}
	defer unix.Close(pidfd)
	receiver, sender, err := unet.SocketPair(true)
	if err != nil {
		return err
	}
	defer receiver.Close()
	raw, err := sender.Release()
	if err != nil {
		sender.Close()
		return err
	}
	channel := os.NewFile(uintptr(raw), "park-transfer")
	defer channel.Close()
	type transferResult struct {
		entries []checkpoint.ParkFile
		files   []*os.File
		err     error
	}
	transferred := make(chan transferResult, 1)
	go func() {
		entries, owned, err := checkpoint.ReceiveParkFiles(receiver, func(entry checkpoint.ParkFile, file *os.File) error {
			if !expected[entry.ID] {
				return fmt.Errorf("unexpected park resource %+v", entry.ID)
			}
			info, err := file.Stat()
			if err != nil {
				return err
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok || !info.Mode().IsRegular() || st.Nlink != 0 || uint64(st.Dev) != targetStat.Dev {
				return fmt.Errorf("park filestore must be anonymous and on the checkpoint filesystem")
			}
			delete(expected, entry.ID)
			return nil
		})
		if err == nil && len(expected) != 0 {
			err = fmt.Errorf("park transfer omitted %d resources", len(expected))
		}
		if err == nil {
			var n int
			n, err = receiver.Write([]byte{1})
			if err == nil && n != 1 {
				err = fmt.Errorf("short park acknowledgement")
			}
		}
		if err != nil {
			for _, file := range owned {
				file.Close()
			}
			owned = nil
			receiver.Close()
		}
		transferred <- transferResult{entries, owned, err}
	}()
	status.State = park.Capturing
	if err := store.Write(status, false); err != nil {
		receiver.Close()
		channel.Close()
		result := <-transferred
		files = result.files
		return err
	}
	request.Opts.SkipFilestorePages = true
	request.Opts.ParkFilestores = channel
	captureErr := c.Checkpoint(request.Config, status.ImagePath, request.Opts)
	channel.Close()
	receiver.Close()
	result := <-transferred
	files = result.files
	if captureErr != nil || result.err != nil {
		if exited, _ := pollParkExit(pidfd, 0); exited {
			now := time.Now()
			status.StoppedAt = &now
		}
		return errors.Join(captureErr, result.err)
	}
	if exited, err := pollParkExit(pidfd, -1); err != nil || !exited {
		return fmt.Errorf("confirming park source exit: %w", err)
	}
	stoppedAt := time.Now()
	status.StoppedAt = &stoppedAt
	c.Status = Stopped
	if err := c.saveLocked(); err != nil {
		return err
	}
	c.Saver.UnlockOrDie()
	locked = false
	status.State = park.Stopped
	if err := store.Write(status, false); err != nil {
		return err
	}
	status.State = park.Committing
	if err := store.Write(status, false); err != nil {
		return err
	}
	entries := make([]checkpoint.FilestoreSidecarEntry, 0, len(files))
	for i, source := range files {
		name := fmt.Sprintf("filestore-%d", i)
		dest, err := os.OpenFile(filepath.Join(status.FilestorePath, name), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		before := time.Now()
		cloneErr := cloneParkFile(dest, source)
		status.CloneNS += time.Since(before).Nanoseconds()
		if cloneErr != nil {
			dest.Close()
			return fmt.Errorf("cloning %s: %w", name, cloneErr)
		}
		before = time.Now()
		syncErr := dest.Sync()
		status.SnapshotSyncNS += time.Since(before).Nanoseconds()
		if syncErr != nil {
			dest.Close()
			return syncErr
		}
		info, statErr := dest.Stat()
		fp, fpErr := checkpoint.FingerprintFile(dest)
		closeErr := dest.Close()
		if err := errors.Join(statErr, fpErr, closeErr); err != nil {
			return err
		}
		id := result.entries[i].ID
		entries = append(entries, checkpoint.FilestoreSidecarEntry{File: name, ResourceContainer: id.ContainerName, ResourcePath: id.Path, SizeBytes: uint64(info.Size()), ChunkCount: result.entries[i].ChunkCount, Fingerprint: fp})
	}
	closeFiles()
	sidecar, err := os.OpenFile(filepath.Join(status.FilestorePath, filestoresSidecarName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = checkpoint.WriteFilestoreSidecar(sidecar, entries)
	err = errors.Join(err, sidecar.Close())
	if err != nil {
		return err
	}
	if err := park.SyncArtifacts(status.ImagePath); err != nil {
		return err
	}
	if err := park.SyncArtifacts(status.FilestorePath); err != nil {
		return err
	}
	requestFile, err = os.Open(filepath.Join(dir, "request.json"))
	if err != nil {
		return err
	}
	err = requestFile.Sync()
	err = errors.Join(err, requestFile.Close())
	if err != nil {
		return err
	}
	if err := store.Sync(); err != nil {
		return err
	}
	durableAt := time.Now()
	status.DurableAt = &durableAt
	status.State = park.Ready
	return store.Write(status, true)
}

func pollParkExit(pidfd, timeoutMS int) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
	for {
		_, err := unix.Poll(fds, timeoutMS)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, err
		}
		return fds[0].Revents&unix.POLLIN != 0, nil
	}
}
