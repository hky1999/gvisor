// Copyright 2019 The gVisor Authors.
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
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const resourceGenerationAnnotation = "akernel.scheduler/resource-generation"

type readerBirth struct {
	PID              int    `json:"pid"`
	StartTime        uint64 `json:"startTime"`
	PIDNamespace     string `json:"pidNamespace"`
	NamespaceInit    bool   `json:"namespaceInit"`
	PrivateNamespace bool   `json:"privateNamespace"`
}

type retirementIdentity struct {
	Version              int          `json:"version"`
	Generation           string       `json:"generation"`
	BootID               string       `json:"bootID"`
	ObserverPIDNamespace string       `json:"observerPIDNamespace"`
	ProcPIDNamespace     string       `json:"procPIDNamespace"`
	Sentry               readerBirth  `json:"sentry"`
	Gofer                *readerBirth `json:"gofer,omitempty"`
}

// procStartTime handles ')' and spaces in comm; field 22 follows the final ')'.
func procStartTime(raw string) (uint64, error) {
	end := strings.LastIndexByte(raw, ')')
	if end < 0 {
		return 0, fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(raw[end+1:])
	if len(fields) < 20 {
		return 0, fmt.Errorf("short process stat")
	}
	n, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("invalid process start time %q", fields[19])
	}
	return n, nil
}

func readReaderBirth(pid int) (readerBirth, error) {
	if pid <= 1 {
		return readerBirth{}, fmt.Errorf("invalid reader PID %d", pid)
	}
	base := filepath.Join("/proc", strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(base, "stat"))
	if err != nil {
		return readerBirth{}, err
	}
	start, err := procStartTime(string(stat))
	if err != nil {
		return readerBirth{}, err
	}
	ns, err := os.Readlink(filepath.Join(base, "ns/pid"))
	if err != nil {
		return readerBirth{}, err
	}
	ownNS, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return readerBirth{}, err
	}
	status, err := os.ReadFile(filepath.Join(base, "status"))
	if err != nil {
		return readerBirth{}, err
	}
	init := false
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "NSpid:") {
			ids := strings.Fields(strings.TrimPrefix(line, "NSpid:"))
			init = len(ids) != 0 && ids[len(ids)-1] == "1"
		}
	}
	// Reject a PID replaced while its namespace was being inspected.
	again, err := os.ReadFile(filepath.Join(base, "stat"))
	if err != nil {
		return readerBirth{}, err
	}
	second, err := procStartTime(string(again))
	if err != nil || second != start {
		return readerBirth{}, fmt.Errorf("reader PID %d changed during capture", pid)
	}
	return readerBirth{PID: pid, StartTime: start, PIDNamespace: ns, NamespaceInit: init, PrivateNamespace: ns != ownNS}, nil
}

func readerBootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if len(id) != 36 {
		return "", fmt.Errorf("invalid kernel boot identity")
	}
	return id, nil
}

// The PID returned by exec is relative to the caller's PID namespace. A procfs
// mounted for a different namespace must not be used to resolve that number.
func retirementObservation() (string, string, error) {
	observer, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return "", "", err
	}
	proc, err := os.Readlink("/proc/1/ns/pid")
	if err != nil {
		return "", "", err
	}
	if observer != proc {
		return "", "", fmt.Errorf("caller and procfs PID views do not agree")
	}
	return observer, proc, nil
}

func validReaderBirth(b readerBirth) error {
	if b.PID <= 1 || b.StartTime == 0 || b.PIDNamespace == "" {
		return fmt.Errorf("incomplete reader birth identity")
	}
	if !b.NamespaceInit || !b.PrivateNamespace {
		return fmt.Errorf("reader PID %d is not the init of a private PID namespace", b.PID)
	}
	return nil
}

func (c *Container) captureRetirementIdentity() error {
	if c.Spec == nil || c.Spec.Annotations[resourceGenerationAnnotation] == "" {
		return nil
	}
	generation := c.Spec.Annotations[resourceGenerationAnnotation]
	if c.Sandbox == nil || c.ID != c.Sandbox.ID || c.Saver.ID.ContainerID != c.Saver.ID.SandboxID {
		return fmt.Errorf("generation-bound retirement supports only a root container")
	}
	boot, err := readerBootID()
	if err != nil {
		return err
	}
	observer, proc, err := retirementObservation()
	if err != nil {
		return err
	}
	sentry, err := readReaderBirth(c.Sandbox.Pid.Load())
	if err != nil {
		return fmt.Errorf("capturing sentry: %w", err)
	}
	if err := validReaderBirth(sentry); err != nil {
		return err
	}
	identity := &retirementIdentity{Version: 2, Generation: generation, BootID: boot, ObserverPIDNamespace: observer, ProcPIDNamespace: proc, Sentry: sentry}
	if pid := c.GoferPid.Load(); pid != 0 {
		gofer, err := readReaderBirth(pid)
		if err != nil {
			return fmt.Errorf("capturing gofer: %w", err)
		}
		if err := validReaderBirth(gofer); err != nil {
			return err
		}
		if gofer.PID == sentry.PID || gofer.PIDNamespace == sentry.PIDNamespace {
			return fmt.Errorf("sentry and gofer must have separate process identities")
		}
		identity.Gofer = &gofer
	}
	c.RetirementIdentity = identity
	return nil
}

func loadRetirementLocked(s *StateFile, expected string) (*Container, error) {
	if !s.flock.Locked() {
		panic("retirement reload without creation lock")
	}
	raw, err := os.ReadFile(s.statePath())
	if err != nil {
		return nil, fmt.Errorf("loading retirement identity: %w", err)
	}
	c := &Container{}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("decoding retirement identity: %w", err)
	}
	if c.Spec == nil || c.Spec.Annotations[resourceGenerationAnnotation] != expected {
		return nil, fmt.Errorf("resource generation mismatch")
	}
	identity := c.RetirementIdentity
	if identity == nil || identity.Version != 2 || identity.Generation != expected || len(identity.BootID) != 36 || identity.ObserverPIDNamespace == "" || identity.ProcPIDNamespace == "" {
		return nil, fmt.Errorf("missing or invalid retirement identity")
	}
	if c.ID != s.ID.ContainerID || s.ID.ContainerID != s.ID.SandboxID || c.Saver.ID != s.ID || c.Saver.RootDir != s.RootDir {
		return nil, fmt.Errorf("retirement container identity mismatch")
	}
	if c.Sandbox != nil {
		if c.Sandbox.ID != c.ID {
			return nil, fmt.Errorf("retirement sandbox identity mismatch")
		}
		pid := c.Sandbox.Pid.Load()
		if pid != 0 && pid != identity.Sentry.PID {
			return nil, fmt.Errorf("sentry PID does not match saved birth identity")
		}
		c.Sandbox.SetRootDir(s.RootDir)
	}
	if identity.Sentry.PIDNamespace == identity.ObserverPIDNamespace {
		return nil, fmt.Errorf("sentry shares the observer PID namespace")
	}
	if err := validReaderBirth(identity.Sentry); err != nil {
		return nil, err
	}
	if identity.Gofer == nil {
		if c.GoferPid.Load() != 0 {
			return nil, fmt.Errorf("missing gofer birth identity")
		}
	} else {
		if identity.Gofer.PIDNamespace == identity.ObserverPIDNamespace {
			return nil, fmt.Errorf("gofer shares the observer PID namespace")
		}
		if err := validReaderBirth(*identity.Gofer); err != nil {
			return nil, err
		}
		if pid := c.GoferPid.Load(); pid != 0 && pid != identity.Gofer.PID {
			return nil, fmt.Errorf("gofer PID does not match saved birth identity")
		}
		if identity.Gofer.PID == identity.Sentry.PID || identity.Gofer.PIDNamespace == identity.Sentry.PIDNamespace {
			return nil, fmt.Errorf("overlapping reader birth identities")
		}
	}
	ids, err := listMatch(s.RootDir, FullID{SandboxID: s.ID.SandboxID})
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if id != s.ID {
			return nil, fmt.Errorf("strong retirement does not support a multi-container sandbox")
		}
	}
	return c, nil
}

func sameReaderBirth(a, b readerBirth) bool {
	return a.PID == b.PID && a.StartTime == b.StartTime && a.PIDNamespace == b.PIDNamespace
}

// pinReader returns -1 only if the original namespace init is already gone.
// It never signals an unrecognized/reused numeric PID.
func pinReader(saved readerBirth) (int, error) {
	if err := validReaderBirth(saved); err != nil {
		return -1, err
	}
	current, err := readReaderBirth(saved.PID)
	if os.IsNotExist(err) || errors.Is(err, unix.ESRCH) {
		return -1, nil
	}
	if err != nil {
		return -1, err
	}
	if !sameReaderBirth(current, saved) {
		return -1, nil
	}
	fd, err := unix.PidfdOpen(saved.PID, 0)
	if errors.Is(err, unix.ESRCH) {
		return -1, nil
	}
	if err != nil {
		return -1, fmt.Errorf("opening reader pidfd: %w", err)
	}
	current, err = readReaderBirth(saved.PID)
	if os.IsNotExist(err) || errors.Is(err, unix.ESRCH) || (err == nil && !sameReaderBirth(current, saved)) {
		unix.Close(fd)
		return -1, nil
	}
	if err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func waitReaderExit(fd int, deadline time.Time) error {
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("timed out waiting for kernel-confirmed reader exit")
		}
		ms := int((left + time.Millisecond - 1) / time.Millisecond)
		n, err := unix.Poll(poll, ms)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n > 0 && poll[0].Revents&unix.POLLIN != 0 {
			return nil
		}
		if n > 0 && poll[0].Revents&(unix.POLLERR|unix.POLLNVAL|unix.POLLHUP) != 0 {
			return fmt.Errorf("invalid reader pidfd poll result %#x", poll[0].Revents)
		}
	}
}

func (c *Container) retireReaders() error {
	identity := c.RetirementIdentity
	boot, err := readerBootID()
	if err != nil {
		return err
	}
	if boot != identity.BootID {
		// A different host or boot is not authority over the original readers.
		return fmt.Errorf("kernel boot identity changed; reader exit is unproven")
	}
	observer, proc, err := retirementObservation()
	if err != nil {
		return err
	}
	if observer != identity.ObserverPIDNamespace || proc != identity.ProcPIDNamespace {
		return fmt.Errorf("reader PID observation namespace changed")
	}
	births := []readerBirth{identity.Sentry}
	if identity.Gofer != nil {
		births = append(births, *identity.Gofer)
	}
	var fds []int
	defer func() {
		for _, fd := range fds {
			unix.Close(fd)
		}
	}()
	// Validate and pin every reader before signalling any of them.
	for _, birth := range births {
		fd, err := pinReader(birth)
		if err != nil {
			return err
		}
		if fd >= 0 {
			fds = append(fds, fd)
		}
	}
	for _, fd := range fds {
		if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("signalling reader pidfd: %w", err)
		}
	}
	deadline := time.Now().Add(2 * time.Minute)
	for _, fd := range fds {
		if err := waitReaderExit(fd, deadline); err != nil {
			return err
		}
	}
	return nil
}
