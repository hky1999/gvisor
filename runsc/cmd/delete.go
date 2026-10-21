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
	// force indicates that the container should be terminated if running.
	force bool
	// expectedGeneration enables fail-closed AKernel reader retirement.
	expectedGeneration string
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
	f.BoolVar(&d.force, "force", false, "terminate container if running")
	f.StringVar(&d.expectedGeneration, "expected-generation", "", "require this AKernel creation and kernel-confirmed reader exit")
}

// FetchSpec implements util.SubCommand.FetchSpec.
func (d *Delete) FetchSpec(conf *config.Config, f *flag.FlagSet) (string, *specs.Spec, error) {
	c, err := d.loadForDelete(conf, f)
	if err != nil {
		if os.IsNotExist(err) && d.force && d.expectedGeneration == "" {
			return "", nil, nil
		}
		return "", nil, fmt.Errorf("loading container: %w", err)
	}
	return c.ID, c.Spec, nil
}

// Execute implements subcommands.Command.Execute.
func (d *Delete) Execute(_ context.Context, f *flag.FlagSet, args ...any) subcommands.ExitStatus {
	if f.NArg() == 0 {
		f.Usage()
		return subcommands.ExitUsageError
	}

	conf := args[0].(*config.Config)
	if err := d.execute(f, conf); err != nil {
		util.Fatalf("%v", err)
	}
	return subcommands.ExitSuccess
}

func (d *Delete) execute(f *flag.FlagSet, conf *config.Config) error {
	ids := f.Args()
	if d.expectedGeneration != "" {
		c, err := d.loadForDelete(conf, f)
		if err != nil {
			return err
		}
		return c.DestroyIfGeneration(d.expectedGeneration)
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
			if os.IsNotExist(err) && d.force {
				log.Warningf("couldn't find container %q: %v", id, err)
				return nil
			}
			return fmt.Errorf("loading container %q: %v", id, err)
		}
		if !d.force && c.Status != container.Created && c.Status != container.Stopped {
			return fmt.Errorf("cannot delete container that is not stopped without --force flag")
		}
		if err := c.Destroy(); err != nil {
			return fmt.Errorf("destroying container: %v", err)
		}
	}
	return nil
}

func (d *Delete) loadForDelete(conf *config.Config, f *flag.FlagSet) (*container.Container, error) {
	if d.expectedGeneration == "" {
		return d.loadContainer(conf, f, container.LoadOpts{})
	}
	if !d.force || f.NArg() != 1 {
		return nil, fmt.Errorf("expected-generation requires --force and exactly one full container ID")
	}
	id := f.Arg(0)
	return container.Load(conf.RootDir, container.FullID{SandboxID: id, ContainerID: id}, container.LoadOpts{Exact: true, SkipCheck: true})
}
