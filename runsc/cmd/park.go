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

package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/google/subcommands"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/sentry/control/controlapi"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/state/statefile"
	"gvisor.dev/gvisor/runsc/cmd/util"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/park"
	"gvisor.dev/gvisor/runsc/sandbox"
)

// Park creates an asynchronous non-resuming checkpoint task.
type Park struct {
	Checkpoint
	completion string
	timeout    time.Duration
}

// Name implements subcommands.Command.Name.
func (*Park) Name() string { return "park" }

// Synopsis implements subcommands.Command.Synopsis.
func (*Park) Synopsis() string { return "stop a sandbox and persist its checkpoint asynchronously" }

// Usage implements subcommands.Command.Usage.
func (*Park) Usage() string {
	return "park --job-dir=<new directory> [--completion=stopped|ready|none] <container id>\n"
}

// SetFlags implements subcommands.Command.SetFlags.
func (p *Park) SetFlags(f *flag.FlagSet) {
	f.StringVar(&p.imagePath, "job-dir", "", "new private task directory on the filestore filesystem")
	f.Var(newCheckpointCompressionValue(statefile.CompressionLevelDefault, &p.compression), "compression", "checkpoint compression: none|flate-best-speed")
	f.BoolVar(&p.direct, "direct", false, "use O_DIRECT for checkpoint pages")
	f.BoolVar(&p.excludeCommittedZeroPages, "exclude-committed-zero-pages", false, "exclude committed zero-filled pages")
	f.StringVar(&p.saveRestoreExecArgv, "save-restore-exec-argv", "", "argv of the save/restore hook")
	f.DurationVar(&p.saveRestoreExecTimeout, "save-restore-exec-timeout", controlapi.DefaultSaveRestoreExecTimeout, "hook timeout")
	p.splitFSCheckpointPaths.defaultPrefix = checkpointfiles.FSCheckpointDir
	f.Var(&p.splitFSCheckpointPaths, "fs-checkpoint-paths", "filesystem checkpoint bundle targets")
	f.StringVar(&p.completion, "completion", "stopped", "completion to wait for: none|stopped|ready")
	f.DurationVar(&p.timeout, "timeout", 10*time.Minute, "maximum completion wait; zero waits indefinitely and a timeout leaves the worker running")
}

// Execute implements subcommands.Command.Execute.
func (p *Park) Execute(_ context.Context, f *flag.FlagSet, args ...any) subcommands.ExitStatus {
	if f.NArg() != 1 || p.imagePath == "" {
		f.Usage()
		return subcommands.ExitUsageError
	}
	if p.completion != "none" && p.completion != "stopped" && p.completion != "ready" {
		return util.Errorf("invalid park completion %q", p.completion)
	}
	if p.timeout < 0 {
		return util.Errorf("park timeout must be nonnegative")
	}
	if p.leaveRunning || p.filestoreSnapshotDir != "" {
		return util.Errorf("park does not support --leave-running or --filestore-snapshot-dir")
	}
	conf := args[0].(*config.Config)
	c, err := p.loadContainer(conf, f, container.LoadOpts{})
	if err != nil {
		return util.Errorf("loading park source: %v", err)
	}
	opts := sandbox.CheckpointOpts{Compression: p.compression.Level(), Direct: p.direct, ExcludeCommittedZeroPages: p.excludeCommittedZeroPages, SkipFilestorePages: true, CudaCheckpointPath: p.cudaCheckpointPath, CudaCheckpointSequential: p.cudaCheckpointSequential, SaveRestoreExecArgv: p.saveRestoreExecArgv, SaveRestoreExecTimeout: p.saveRestoreExecTimeout, SaveRestoreExecContainerID: c.ID, SplitFSCheckpointPaths: p.splitFSCheckpointPaths.targets}
	status, err := c.StartPark(conf, p.imagePath, opts)
	if err == nil {
		dir, absErr := filepath.Abs(p.imagePath)
		if absErr != nil {
			return util.Errorf("park path: %v", absErr)
		}
		status, err = park.Wait(dir, p.completion, p.timeout)
	}
	if status.Kind != "" {
		if outputErr := json.NewEncoder(os.Stdout).Encode(status); outputErr != nil {
			return util.Errorf("writing park status: %v", outputErr)
		}
	}
	if err != nil {
		return util.Errorf("park: %v", err)
	}
	return subcommands.ExitSuccess
}

// ParkStatus queries a task, optionally waiting for a completion barrier.
type ParkStatus struct {
	dir     string
	wait    string
	timeout time.Duration
}

// Name implements subcommands.Command.Name.
func (*ParkStatus) Name() string { return "park-status" }

// Synopsis implements subcommands.Command.Synopsis.
func (*ParkStatus) Synopsis() string { return "query asynchronous park progress" }

// Usage implements subcommands.Command.Usage.
func (*ParkStatus) Usage() string {
	return "park-status --job-dir=<directory> [--wait=none|stopped|ready]\n"
}

// SetFlags implements subcommands.Command.SetFlags.
func (p *ParkStatus) SetFlags(f *flag.FlagSet) {
	f.StringVar(&p.dir, "job-dir", "", "park job directory")
	f.StringVar(&p.wait, "wait", "none", "completion barrier: none|stopped|ready")
	f.DurationVar(&p.timeout, "timeout", 10*time.Minute, "maximum wait; timeout leaves the worker running")
}

// FetchSpec implements util.SubCommand.FetchSpec.
func (*ParkStatus) FetchSpec(*config.Config, *flag.FlagSet) (string, *specs.Spec, error) {
	return "", nil, nil
}

// Execute implements subcommands.Command.Execute.
func (p *ParkStatus) Execute(_ context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if p.dir == "" || f.NArg() != 0 {
		f.Usage()
		return subcommands.ExitUsageError
	}
	if p.timeout < 0 {
		return util.Errorf("park timeout must be nonnegative")
	}
	status, err := park.Wait(p.dir, p.wait, p.timeout)
	if status.Kind != "" {
		if outputErr := json.NewEncoder(os.Stdout).Encode(status); outputErr != nil {
			return util.Errorf("writing park status: %v", outputErr)
		}
	}
	if err != nil {
		return util.Errorf("park status: %v", err)
	}
	return subcommands.ExitSuccess
}

// ParkWorker is the internal detached task entrypoint.
type ParkWorker struct {
	dir    string
	lockFD int
}

// Name implements subcommands.Command.Name.
func (*ParkWorker) Name() string { return "park-worker" }

// Synopsis implements subcommands.Command.Synopsis.
func (*ParkWorker) Synopsis() string { return "internal asynchronous checkpoint worker" }

// Usage implements subcommands.Command.Usage.
func (*ParkWorker) Usage() string { return "park-worker --job-dir=<directory> --lock-fd=<fd>\n" }

// SetFlags implements subcommands.Command.SetFlags.
func (p *ParkWorker) SetFlags(f *flag.FlagSet) {
	f.StringVar(&p.dir, "job-dir", "", "task directory")
	f.IntVar(&p.lockFD, "lock-fd", -1, "inherited exclusive task lock")
}

// FetchSpec implements util.SubCommand.FetchSpec.
func (*ParkWorker) FetchSpec(*config.Config, *flag.FlagSet) (string, *specs.Spec, error) {
	return "", nil, nil
}

// Execute implements subcommands.Command.Execute.
func (p *ParkWorker) Execute(_ context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if p.dir == "" || p.lockFD < 3 || f.NArg() != 0 {
		f.Usage()
		return subcommands.ExitUsageError
	}
	if err := container.RunParkWorker(p.dir, os.NewFile(uintptr(p.lockFD), "park-lock")); err != nil {
		return util.Errorf("park worker: %v", err)
	}
	return subcommands.ExitSuccess
}
