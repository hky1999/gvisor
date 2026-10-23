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

// Package park stores asynchronous checkpoint progress and durable completion.
package park

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// State is a park task phase. Only Ready advertises usable checkpoint artifacts.
type State string

// Park task states.
const (
	Preparing  State = "preparing"
	Capturing  State = "capturing"
	Stopped    State = "stopped"
	Committing State = "committing"
	Ready      State = "ready"
	Failed     State = "failed"
)

// Status is the versioned progress record. StoppedAt is set only after pidfd
// confirmation. Before Ready, progress is observable but is not crash durable.
type Status struct {
	Kind            string     `json:"kind"`
	Version         int        `json:"version"`
	ContainerID     string     `json:"container_id"`
	WorkerPID       int        `json:"worker_pid"`
	State           State      `json:"state"`
	StartedAt       time.Time  `json:"started_at"`
	StoppedAt       *time.Time `json:"stopped_at,omitempty"`
	DurableAt       *time.Time `json:"durable_at,omitempty"`
	ImagePath       string     `json:"image_path"`
	FilestorePath   string     `json:"filestore_path"`
	Error           string     `json:"error,omitempty"`
	CleanupError    string     `json:"cleanup_error,omitempty"`
	CleanupComplete bool       `json:"cleanup_complete"`
	CloneNS         int64      `json:"ficlone_ns,omitempty"`
	SnapshotSyncNS  int64      `json:"snapshot_fsync_ns,omitempty"`
}

const statusName = ".runsc-park.json"
const lockName = ".runsc-park.lock"
const failureName = ".runsc-park.failed"
const failureReserveName = ".runsc-park.failure-reserve"

const failureRecord = "runsc-park-failure-v1\n"

// Store owns the exclusive task lock. Closing the last inherited lock descriptor
// proves that the worker can no longer retain anonymous filestore references.
type Store struct {
	Dir  string
	lock *os.File
}

// Create reserves a new private job directory. Existing directories are rejected.
func Create(dir, containerID string) (*Store, Status, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, Status{}, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(dir))
	if err != nil {
		return nil, Status{}, err
	}
	dir = filepath.Join(parent, filepath.Base(dir))
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, Status{}, err
	}
	file, err := openLock(dir, os.O_CREATE|os.O_EXCL)
	if err != nil {
		os.RemoveAll(dir)
		return nil, Status{}, err
	}
	store := &Store{Dir: dir, lock: file}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		store.Close()
		os.RemoveAll(dir)
		return nil, Status{}, err
	}
	status := Status{Kind: "runsc-park", Version: 1, ContainerID: containerID, State: Preparing, StartedAt: time.Now(), ImagePath: filepath.Join(dir, "image"), FilestorePath: filepath.Join(dir, "filestores")}
	// Reserve the failure record before capturing the source. Renaming this
	// file can publish failure even when no space remains for a new JSON file.
	reserve, err := os.OpenFile(filepath.Join(dir, failureReserveName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		_, err = reserve.WriteString(failureRecord)
		if err == nil {
			err = reserve.Sync()
		}
		err = errors.Join(err, reserve.Close())
	}
	if err == nil {
		err = store.Write(status, true)
	}
	if err != nil {
		store.Close()
		os.RemoveAll(dir)
		return nil, Status{}, err
	}
	return store, status, nil
}

// LockFile is borrowed for inheritance by the worker.
func (s *Store) LockFile() *os.File { return s.lock }

// Close releases the task lock.
func (s *Store) Close() error { return s.lock.Close() }

func privateDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0077 != 0 || st.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("park directory must be a private directory owned by the current user")
	}
	return nil
}

func openLock(dir string, flags int) (*os.File, error) {
	if err := privateDirectory(dir); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, lockName), flags|os.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
}

// Inherit takes ownership of an inherited lock FD after checking its identity.
func Inherit(dir string, file *os.File) (*Store, Status, error) {
	if err := privateDirectory(dir); err != nil {
		file.Close()
		return nil, Status{}, err
	}
	a, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, Status{}, err
	}
	b, err := os.Lstat(filepath.Join(dir, lockName))
	if err != nil || !a.Mode().IsRegular() || !os.SameFile(a, b) {
		file.Close()
		return nil, Status{}, fmt.Errorf("invalid inherited park lock")
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, Status{}, err
	}
	unix.CloseOnExec(int(file.Fd()))
	store := &Store{Dir: dir, lock: file}
	status, err := read(dir)
	if err != nil {
		store.Close()
		return nil, Status{}, err
	}
	return store, status, nil
}

// Write publishes a whole status record. Nonterminal progress deliberately does
// not fsync: acknowledging stopped must not wait for unrelated disk writeback.
func (s *Store) Write(status Status, durable bool) error {
	stored := status
	stored.ImagePath = "image"
	stored.FilestorePath = "filestores"
	if err := WriteJSON(s.Dir, statusName, stored, durable); err != nil {
		return err
	}
	if durable {
		return syncDirectory(filepath.Dir(s.Dir))
	}
	return nil
}

// Sync persists the task directory and its entry in the existing parent.
func (s *Store) Sync() error {
	if err := syncDirectory(s.Dir); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(s.Dir))
}

// WriteJSON atomically replaces a private task file, optionally making it durable.
func WriteJSON(dir, name string, value any, durable bool) error {
	if filepath.Base(name) != name {
		return fmt.Errorf("invalid park file name")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".park-write-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return err
	}
	if durable {
		if err := file.Sync(); err != nil {
			file.Close()
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	if durable {
		return syncDirectory(dir)
	}
	return nil
}

func read(dir string) (Status, error) {
	var status Status
	file, err := os.OpenFile(filepath.Join(dir, statusName), os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return status, err
	}
	defer file.Close()
	if err := json.NewDecoder(file).Decode(&status); err != nil {
		return status, err
	}
	if status.Kind != "runsc-park" || status.Version != 1 || status.ImagePath != "image" || status.FilestorePath != "filestores" {
		return status, fmt.Errorf("invalid park status")
	}
	status.ImagePath = filepath.Join(dir, "image")
	status.FilestorePath = filepath.Join(dir, "filestores")
	if status.State == Ready && (status.StoppedAt == nil || status.DurableAt == nil) {
		return status, fmt.Errorf("ready park status lacks completion timestamps")
	}
	if (status.State == Stopped || status.State == Committing) && status.StoppedAt == nil {
		return status, fmt.Errorf("park stop has not been confirmed")
	}
	failed, err := failurePublished(dir)
	if err != nil {
		return status, err
	}
	if failed && status.State != Failed {
		status.State = Failed
		status.DurableAt = nil
		status.CleanupComplete = false
		status.Error = "park failed before durable completion; detailed status could not be published"
	}
	return status, nil
}

func failurePublished(dir string) (bool, error) {
	file, err := os.OpenFile(filepath.Join(dir, failureName), os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != int64(len(failureRecord)) {
		return false, fmt.Errorf("invalid park failure record")
	}
	data := make([]byte, len(failureRecord))
	if _, err := file.ReadAt(data, 0); err != nil {
		return false, err
	}
	if string(data) != failureRecord {
		return false, fmt.Errorf("invalid park failure record")
	}
	return true, nil
}

// Fail records failure before removing incomplete artifacts. A reserved record
// keeps cleanup independent of allocating space for the detailed JSON status.
// If failure cannot be made durable, artifacts remain for reconciliation.
func (s *Store) Fail(status Status, cause error) (Status, error) {
	status.State = Failed
	status.DurableAt = nil
	status.Error = cause.Error()
	status.CleanupComplete = false
	retain := func(err error) (Status, error) {
		status.CleanupError = fmt.Sprintf("publishing failure; outputs retained: %v", err)
		return status, err
	}
	failed, err := failurePublished(s.Dir)
	if err != nil {
		return retain(err)
	}
	if !failed {
		if err := os.Rename(filepath.Join(s.Dir, failureReserveName), filepath.Join(s.Dir, failureName)); err != nil {
			if !os.IsNotExist(err) {
				return retain(err)
			}
			// Older jobs have no reserve. They must persist the full failure
			// status before cleanup and may need free space to do so.
			if err := s.Write(status, true); err != nil {
				return retain(err)
			}
		}
	}
	if err := s.Sync(); err != nil {
		return retain(err)
	}
	// A failure marker is already durable. JSON publication may be retried
	// after deleting the outputs has freed space.
	_ = s.Write(status, true)
	cleanupErr := s.RemoveArtifacts()
	cleanupErr = errors.Join(cleanupErr, s.Sync())
	if cleanupErr != nil {
		status.CleanupError = cleanupErr.Error()
	} else {
		status.CleanupError = ""
		status.CleanupComplete = true
	}
	return status, errors.Join(cleanupErr, s.Write(status, true))
}

// RemoveArtifacts removes fixed output paths and abandoned status temporaries.
// Metadata and logs remain available; symlinks are removed rather than followed.
func (s *Store) RemoveArtifacts() error {
	err := errors.Join(os.RemoveAll(filepath.Join(s.Dir, "image")), os.RemoveAll(filepath.Join(s.Dir, "filestores")))
	entries, readErr := os.ReadDir(s.Dir)
	if readErr != nil {
		return errors.Join(err, readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".park-write-") {
			err = errors.Join(err, os.Remove(filepath.Join(s.Dir, entry.Name())))
		}
	}
	return err
}

// Read returns progress and reconciles tasks whose exclusive owner disappeared.
// It never declares an abandoned in-progress task ready.
func Read(dir string) (Status, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return Status{}, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return Status{}, err
	}
	file, err := openLock(dir, 0)
	if err != nil {
		return Status{}, err
	}
	defer file.Close()
	lockErr := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if lockErr != nil && lockErr != unix.EWOULDBLOCK {
		return Status{}, lockErr
	}
	status, err := read(dir)
	if err != nil {
		if status.State == Ready {
			status.State = Committing
			status.DurableAt = nil
		}
		return status, err
	}
	if lockErr != nil {
		if status.State == Ready {
			status.State = Committing
			status.DurableAt = nil
		}
		return status, nil
	}
	if status.State == Ready {
		// Finish publication if a worker died between status rename and fsync.
		if err := (&Store{Dir: dir}).Sync(); err != nil {
			status.State = Committing
			status.DurableAt = nil
			return status, err
		}
		return status, nil
	}
	store := &Store{Dir: dir, lock: file}
	if status.State == Failed && status.CleanupComplete {
		return status, nil
	}
	cause := status.Error
	if status.State != Failed {
		cause = "park worker exited before durable completion; task lost its retained filestore descriptors"
	}
	return store.Fail(status, errors.New(cause))
}

// Wait waits for confirmed source exit or durable completion. A timeout leaves
// the worker running. Failure is reported even when the source already stopped.
func Wait(dir, completion string, timeout time.Duration) (Status, error) {
	if completion != "none" && completion != "stopped" && completion != "ready" {
		return Status{}, fmt.Errorf("invalid park completion %q", completion)
	}
	deadline := time.Now().Add(timeout)
	for {
		status, err := Read(dir)
		if err != nil {
			return status, err
		}
		if completion == "none" {
			return status, nil
		}
		if status.State == Failed {
			return status, fmt.Errorf("park failed: %s", status.Error)
		}
		if (completion == "stopped" && status.StoppedAt != nil) || status.State == Ready {
			return status, nil
		}
		if timeout > 0 && time.Now().After(deadline) {
			return status, fmt.Errorf("timed out waiting for park %s; worker continues", completion)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// CheckArtifact rejects a managed artifact before its task is durable.
// Ordinary checkpoints without the reserved park status file are unaffected.
func CheckArtifact(path, base string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	candidates := []string{path}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != path {
		candidates = append(candidates, resolved)
	}
	for _, candidate := range candidates {
		if filepath.Base(candidate) != base {
			continue
		}
		dir := filepath.Dir(candidate)
		if _, err := os.Lstat(filepath.Join(dir, statusName)); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		status, err := Read(dir)
		if err != nil {
			return err
		}
		if status.State != Ready {
			return fmt.Errorf("park checkpoint is not durable (state %s); wait for ready", status.State)
		}
	}
	return nil
}

func syncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

// SyncArtifacts makes files and directory entries durable without following
// symlinks or synchronizing unrelated filesystem data.
func SyncArtifacts(dir string) error {
	var directories []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in park artifacts: %s", path)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular park artifact: %s", path)
		}
		file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		err = file.Sync()
		return errors.Join(err, file.Close())
	})
	if err != nil {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := syncDirectory(directories[i]); err != nil {
			return err
		}
	}
	return nil
}
