// Copyright 2026 The gVisor Authors.
// SPDX-License-Identifier: Apache-2.0

// Package processbirth binds a runtime child to its kernel process birth.
// Persisted observations are reacquired through pidfds, never numeric signals.
package processbirth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// GenerationAnnotation is reserved for the scheduler's trusted OCI annotation.
const GenerationAnnotation = "akernel.scheduler/resource-generation"

// ValidBinding checks the bounded container and generation identifiers.
func ValidBinding(id, generation string) bool { return validKey(id) && validKey(generation) }

// Scope identifies the observer namespace in which a saved PID is meaningful.
type Scope struct {
	BootID             string `json:"boot_id"`
	PIDNamespaceDevice uint64 `json:"pid_namespace_device"`
	PIDNamespaceInode  uint64 `json:"pid_namespace_inode"`
}

// Birth is emitted by the initializer while its direct child remains unreaped.
// Executable and argument bytes are hashed and are never stored in the record.
type Birth struct {
	Version          int    `json:"version"`
	ContainerID      string `json:"container_id"`
	Generation       string `json:"resource_generation"`
	Role             string `json:"role"`
	Scope            Scope  `json:"scope"`
	PID              int    `json:"pid"`
	StartTicks       uint64 `json:"start_ticks"`
	InitializerPID   int    `json:"initializer_pid"`
	ExecutableSHA256 string `json:"executable_sha256"`
	ArgumentsSHA256  string `json:"arguments_sha256"`
	CgroupSHA256     string `json:"cgroup_sha256"`
}

type observation struct {
	pid, parent                   int
	start                         uint64
	exited                        bool
	executable, arguments, cgroup string
}

type operations struct {
	scope          func() (Scope, error)
	observe        func(context.Context, int) (*observation, error)
	open           func(int) (int, error)
	poll           func(int, int) (bool, error)
	signal         func(int, unix.Signal) error
	close          func(int) error
	initializerPID int
}

func kernelOperations() operations {
	return operations{
		scope: currentScope, observe: observeProcess,
		open: func(pid int) (int, error) { return unix.PidfdOpen(pid, 0) },
		poll: func(fd, milliseconds int) (bool, error) {
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			_, err := unix.Poll(fds, milliseconds)
			if errors.Is(err, unix.EINTR) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if fds[0].Revents&(unix.POLLNVAL|unix.POLLERR) != 0 {
				return false, errors.New("original process handle became invalid")
			}
			return fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0, nil
		},
		signal: func(fd int, signal unix.Signal) error { return unix.PidfdSendSignal(fd, signal, nil, 0) },
		close:  unix.Close, initializerPID: os.Getpid(),
	}
}

func validKey(value string) bool {
	return len(value) > 0 && len(value) <= 256 && value != "." && value != ".." &&
		strings.IndexFunc(value, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '+' || r == '.')
		}) == -1
}

func validDigest(value string) bool {
	data, err := hex.DecodeString(value)
	return err == nil && len(data) == sha256.Size && value == strings.ToLower(value)
}

// Validate rejects incomplete observations and another execution's binding.
func (b *Birth) Validate(containerID, generation, role string) error {
	if b == nil || b.Version != 1 || !validKey(containerID) || !validKey(generation) ||
		b.ContainerID != containerID || b.Generation != generation || b.Role != role ||
		(role != "sentry" && role != "gofer") || b.PID <= 0 || b.StartTicks == 0 ||
		b.InitializerPID <= 0 || b.Scope.BootID == "" || b.Scope.PIDNamespaceInode == 0 ||
		!validDigest(b.ExecutableSHA256) || !validDigest(b.ArgumentsSHA256) || !validDigest(b.CgroupSHA256) {
		return errors.New("original runtime process birth is missing or belongs to another execution")
	}
	return nil
}

// CaptureChild must run before the initializer permits the child to be reaped.
// A process discovered later by PID is not an acceptable birth source.
func CaptureChild(ctx context.Context, child *os.Process, containerID, generation, role string) (*Birth, error) {
	if child == nil || child.Pid <= 0 {
		return nil, errors.New("original child handle is missing")
	}
	return captureChild(ctx, child.Pid, containerID, generation, role, kernelOperations())
}

func captureChild(ctx context.Context, pid int, id, generation, role string, ops operations) (*Birth, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope, err := ops.scope()
	if err != nil {
		return nil, err
	}
	before, err := ops.observe(ctx, pid)
	if err != nil || before == nil || before.pid != pid || before.exited || before.parent != ops.initializerPID {
		return nil, errors.New("runtime birth source is not the initializer's live direct child")
	}
	fd, err := ops.open(pid)
	if err != nil {
		return nil, fmt.Errorf("open original child process handle: %w", err)
	}
	defer ops.close(fd)
	after, err := ops.observe(ctx, pid)
	if err != nil || after == nil || *after != *before {
		return nil, errors.New("original child changed while recording birth")
	}
	dead, err := ops.poll(fd, 0)
	if err != nil || dead {
		return nil, errors.New("original child exited before birth publication")
	}
	current, err := ops.scope()
	if err != nil || current != scope {
		return nil, errors.New("original child observer namespace changed")
	}
	birth := &Birth{Version: 1, ContainerID: id, Generation: generation, Role: role, Scope: scope,
		PID: pid, StartTicks: before.start, InitializerPID: ops.initializerPID,
		ExecutableSHA256: before.executable, ArgumentsSHA256: before.arguments, CgroupSHA256: before.cgroup}
	if err := birth.Validate(id, generation, role); err != nil {
		return nil, err
	}
	return birth, ctx.Err()
}

// Handle holds a verified original kernel task; it does not follow PID reuse.
type Handle struct {
	fd     int
	ops    operations
	closed bool
}

// Open reacquires a saved birth. Missing processes are accepted only with a
// complete original record and the same boot and observer PID namespace.
func Open(ctx context.Context, birth *Birth, id, generation, role string) (*Handle, error) {
	return openBirth(ctx, birth, id, generation, role, kernelOperations())
}

func openBirth(ctx context.Context, birth *Birth, id, generation, role string, ops operations) (*Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := birth.Validate(id, generation, role); err != nil {
		return nil, err
	}
	scope, err := ops.scope()
	if err != nil || scope != birth.Scope {
		return nil, errors.New("original process boot or PID namespace differs")
	}
	before, err := ops.observe(ctx, birth.PID)
	if err != nil {
		return nil, err
	}
	if before == nil {
		return &Handle{fd: -1, ops: ops}, nil
	}
	if before.pid != birth.PID || before.start != birth.StartTicks {
		return nil, errors.New("original runtime PID was reused")
	}
	if before.exited {
		return &Handle{fd: -1, ops: ops}, nil
	}
	if before.executable != birth.ExecutableSHA256 || before.arguments != birth.ArgumentsSHA256 || before.cgroup != birth.CgroupSHA256 {
		return nil, errors.New("original runtime process executable, arguments or cgroup differs")
	}
	fd, err := ops.open(birth.PID)
	if errors.Is(err, unix.ESRCH) {
		return &Handle{fd: -1, ops: ops}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open saved original process handle: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			ops.close(fd)
		}
	}()
	after, err := ops.observe(ctx, birth.PID)
	if err != nil || after == nil || *after != *before {
		return nil, errors.New("original runtime process changed while acquiring handle")
	}
	current, err := ops.scope()
	if err != nil || current != birth.Scope {
		return nil, errors.New("original process observer scope changed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ok = true
	return &Handle{fd: fd, ops: ops}, nil
}

// Close releases only the acquired kernel handle.
func (h *Handle) Close() error {
	if h == nil || h.closed {
		return nil
	}
	h.closed = true
	if h.fd < 0 {
		return nil
	}
	return h.ops.close(h.fd)
}

// KillAndWait signals the already-held original handle and confirms its exit.
// It never calls kill(PID), and has an independent twenty-second upper bound.
func (h *Handle) KillAndWait(ctx context.Context) error {
	if h == nil || h.closed {
		return errors.New("original process handle is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.fd < 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	dead, err := h.ops.poll(h.fd, 0)
	if err != nil {
		return err
	}
	if dead {
		return nil
	}
	if err := h.ops.signal(h.fd, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		dead, err := h.ops.poll(h.fd, 25)
		if err != nil {
			return err
		}
		if dead {
			return nil
		}
	}
}

func currentScope() (Scope, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	data, err := boundedFile(context.Background(), "/proc/sys/kernel/random/boot_id", 128)
	if err != nil {
		return Scope{}, err
	}
	self, err := boundedFile(context.Background(), "/proc/self/stat", 65536)
	if err != nil {
		return Scope{}, err
	}
	if _, err := processStat(self, os.Getpid()); err != nil {
		return Scope{}, errors.New("procfs PID numbering differs from the observer namespace")
	}
	var stat unix.Stat_t
	if err := unix.Stat("/proc/thread-self/ns/pid", &stat); err != nil {
		return Scope{}, err
	}
	return Scope{BootID: strings.TrimSpace(string(data)), PIDNamespaceDevice: uint64(stat.Dev), PIDNamespaceInode: stat.Ino}, nil
}

func boundedFile(ctx context.Context, path string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("process birth input is unreadable or exceeds its limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

func processStat(data []byte, pid int) (*observation, error) {
	text := string(data)
	first, last := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if first <= 0 || last <= first {
		return nil, errors.New("runtime process stat is malformed")
	}
	parsedPID, err := strconv.Atoi(strings.TrimSpace(text[:first]))
	fields := strings.Fields(text[last+1:])
	if err != nil || parsedPID != pid || len(fields) < 20 {
		return nil, errors.New("runtime process stat identity is incomplete")
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil || parent < 0 {
		return nil, errors.New("runtime process parent is unknown")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return nil, errors.New("runtime process start time is unknown")
	}
	return &observation{pid: pid, parent: parent, start: start, exited: fields[0] == "Z" || fields[0] == "X"}, nil
}

func executableDigest(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(f, (256<<20)+1))
	if err != nil || n <= 0 || n > 256<<20 {
		return "", errors.New("runtime executable is unreadable or exceeds its limit")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func observeProcess(ctx context.Context, pid int) (*observation, error) {
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	data, err := boundedFile(ctx, filepath.Join(dir, "stat"), 65536)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	before, err := processStat(data, pid)
	if err != nil || before.exited {
		return before, err
	}
	before.executable, err = executableDigest(ctx, filepath.Join(dir, "exe"))
	if err != nil {
		return nil, err
	}
	args, err := boundedFile(ctx, filepath.Join(dir, "cmdline"), 65536)
	if err != nil || len(args) == 0 {
		return nil, errors.New("original runtime arguments are unreadable")
	}
	cgroup, err := boundedFile(ctx, filepath.Join(dir, "cgroup"), 65536)
	if err != nil || len(cgroup) == 0 {
		return nil, errors.New("original runtime cgroup is unreadable")
	}
	argSHA, cgroupSHA := sha256.Sum256(args), sha256.Sum256(cgroup)
	before.arguments, before.cgroup = hex.EncodeToString(argSHA[:]), hex.EncodeToString(cgroupSHA[:])
	data, err = boundedFile(ctx, filepath.Join(dir, "stat"), 65536)
	if err != nil {
		return nil, err
	}
	after, err := processStat(data, pid)
	if err != nil || after.exited || before.pid != after.pid || before.start != after.start || before.parent != after.parent {
		return nil, errors.New("runtime process changed during birth observation")
	}
	return before, nil
}
