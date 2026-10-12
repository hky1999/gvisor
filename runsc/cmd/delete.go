// Copyright 2018 The gVisor Authors.
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
	"errors"
	"fmt"
	"os"

	"github.com/google/subcommands"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/runsc/cmd/util"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/flag"
)

// Delete implements subcommands.Command for the "delete" command.
type Delete struct {
	containerLoader
	expectedGeneration string
	offlineCgroupFD    int
	// force indicates that the container should be terminated if running.
	force bool
}

// Name implements subcommands.Command.Name.
func (*Delete) Name() string {
	return "delete"
}

// Synopsis implements subcommands.Command.Synopsis.
func (*Delete) Synopsis() string {
	return "delete resources held by a container"
}

// Usage implements subcommands.Command.Usage.
func (*Delete) Usage() string {
	return "delete [flags] <container ids>\n"
}

// SetFlags implements subcommands.Command.SetFlags.
func (d *Delete) SetFlags(f *flag.FlagSet) {
	f.StringVar(&d.expectedGeneration, "expected-resource-generation", "", "reject another scheduler execution before deleting runtime children")
	f.IntVar(&d.offlineCgroupFD, "scheduler-offline-cgroup-fd", 0, "original offline cgroup descriptor for managed cleanup; zero disables this mode")
	f.BoolVar(&d.force, "force", false, "terminate container if running")
}

// FetchSpec implements util.SubCommand.FetchSpec.
func (d *Delete) FetchSpec(conf *config.Config, f *flag.FlagSet) (string, *specs.Spec, error) {
	c, err := d.loadContainer(conf, f, container.LoadOpts{})
	if err != nil {
		if os.IsNotExist(err) && d.force {
			return "", nil, nil
		}
		return "", nil, fmt.Errorf("loading container: %w", err)
	}
	return c.ID, c.Spec, nil
}

// Execute implements subcommands.Command.Execute.
func (d *Delete) Execute(ctx context.Context, f *flag.FlagSet, args ...any) subcommands.ExitStatus {
	if f.NArg() == 0 {
		f.Usage()
		return subcommands.ExitUsageError
	}

	conf := args[0].(*config.Config)
	if err := d.executeContext(ctx, f, conf); err != nil {
		util.Fatalf("%v", err)
	}
	return subcommands.ExitSuccess
}

func (d *Delete) execute(f *flag.FlagSet, conf *config.Config) error {
	return d.executeContext(context.Background(), f, conf)
}

func (d *Delete) executeContext(ctx context.Context, f *flag.FlagSet, conf *config.Config) error {
	ids := f.Args()
	if d.offlineCgroupFD != 0 && (d.expectedGeneration == "" || d.offlineCgroupFD < 3) {
		return errors.New("offline cgroup cleanup requires the original generation and directory descriptor")
	}
	if d.expectedGeneration != "" && len(ids) != 1 {
		return fmt.Errorf("generation-conditional deletion requires exactly one full container ID")
	}
	for i, id := range ids {
		var (
			c   *container.Container
			err error
		)
		if i == 0 {
			// The first container is cached via the FetchSpec() mechanism.
			c, err = d.loadContainer(conf, f, container.LoadOpts{})
		} else {
			c, err = container.Load(conf.RootDir, container.FullID{ContainerID: id}, container.LoadOpts{})
		}
		if err != nil {
			if os.IsNotExist(err) && d.force && d.expectedGeneration == "" {
				log.Warningf("couldn't find container %q: %v", id, err)
				return nil
			}
			return fmt.Errorf("loading container %q: %v", id, err)
		}
		if d.expectedGeneration != "" && c.ID != id {
			return fmt.Errorf("generation-conditional deletion rejects abbreviated container IDs")
		}
		if !d.force && c.Status != container.Created && c.Status != container.Stopped {
			return fmt.Errorf("cannot delete container that is not stopped without --force flag")
		}
		var destroyErr error
		if d.expectedGeneration != "" {
			if d.offlineCgroupFD != 0 {
				destroyErr = c.DestroyIfGenerationAfterCgroupOffline(ctx, d.expectedGeneration, d.offlineCgroupFD)
			} else {
				destroyErr = c.DestroyIfGeneration(ctx, d.expectedGeneration)
			}
		} else {
			destroyErr = c.Destroy()
		}
		if destroyErr != nil {
			return fmt.Errorf("destroying container: %v", destroyErr)
		}
	}
	return nil
}
