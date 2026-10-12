// Copyright 2026 The gVisor Authors.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"gvisor.dev/gvisor/runsc/processbirth"
)

// This is structural classifier input only, not a production process birth.
// No successful classifier result below is used to send a signal.
func launchStageFixture(t *testing.T) *Container {
	t.Helper()
	c := birthOwnerFixture(t)
	c.ProcessLaunchVersion = 1
	c.GoferLaunchState, c.SentryLaunchState = processNotDispatched, processNotDispatched
	return c
}

func publishLaunchFixture(t *testing.T, c *Container) {
	t.Helper()
	if err := c.Saver.lock(BlockAcquire); err != nil {
		t.Fatal(err)
	}
	if err := c.saveLocked(); err != nil {
		t.Fatal(err)
	}
	c.Saver.UnlockOrDie()
}

func TestLaunchStagesDistinguishGoferBirthFromUndispatchedSentry(t *testing.T) {
	c := launchStageFixture(t)
	c.RuntimeDispatched, c.GoferRequired = true, true
	c.GoferLaunchState = processBirthPublished
	c.GoferBirth = &processbirth.Birth{PID: 12345}
	c.GoferPid.Store(c.GoferBirth.PID)
	sentry, gofer, err := c.managedProcessRequirements()
	if err != nil || sentry || !gofer {
		t.Fatalf("original gofer with undispatched sentry was not distinguished: sentry=%v gofer=%v err=%v", sentry, gofer, err)
	}
	// A persisted Sentry dispatch intent cannot be reinterpreted as no launch.
	c.SentryLaunchState = processDispatching
	if _, _, err := c.managedProcessRequirements(); err == nil {
		t.Fatal("missing first Sentry birth became cleanup authority")
	}
}

func TestLaunchDispatchWithoutBirthRetainsOriginalOwner(t *testing.T) {
	for _, role := range []string{"gofer", "sentry"} {
		t.Run(role, func(t *testing.T) {
			c := launchStageFixture(t)
			c.RuntimeDispatched = true
			if role == "gofer" {
				c.GoferRequired = true
				c.GoferLaunchState = processDispatching
			} else {
				c.SentryLaunchState = processDispatching
			}
			publishLaunchFixture(t, c)
			before := readBirthOwnerBytes(t, c)
			if err := c.DestroyIfGeneration(context.Background(), c.ResourceGeneration); err == nil {
				t.Fatal("unpublished child became completed retirement")
			}
			if string(readBirthOwnerBytes(t, c)) != string(before) {
				t.Fatal("unknown original launch state was modified or removed")
			}
		})
	}
}

func TestLaunchStageReplacementCannotUseCachedOwner(t *testing.T) {
	c := launchStageFixture(t)
	publishLaunchFixture(t, c)
	data := readBirthOwnerBytes(t, c)
	var replacement Container
	if err := json.Unmarshal(data, &replacement); err != nil {
		t.Fatal(err)
	}
	replacement.RuntimeDispatched = true
	replacement.SentryLaunchState = processDispatching
	changed, err := json.Marshal(&replacement)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Saver.statePath(), changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.DestroyIfGeneration(context.Background(), c.ResourceGeneration); err == nil {
		t.Fatal("cached launch stage authorized replacement deletion")
	}
	if string(readBirthOwnerBytes(t, c)) != string(changed) {
		t.Fatal("replacement launch state changed")
	}
}

func TestLaunchStagesRejectContradictoryOrUnsupportedFacts(t *testing.T) {
	for _, kind := range []string{"missing-stage", "unsupported-version", "legacy-stage-downgrade", "dispatch-without-intent", "required-without-dispatch", "birth-without-dispatch", "published-without-birth"} {
		t.Run(kind, func(t *testing.T) {
			c := launchStageFixture(t)
			switch kind {
			case "missing-stage":
				c.GoferLaunchState = ""
			case "unsupported-version":
				c.ProcessLaunchVersion = 2
			case "legacy-stage-downgrade":
				c.ProcessLaunchVersion = 0
			case "dispatch-without-intent":
				c.RuntimeDispatched = true
			case "required-without-dispatch":
				c.GoferRequired = true
			case "birth-without-dispatch":
				c.GoferBirth = &processbirth.Birth{PID: 12345}
			case "published-without-birth":
				c.RuntimeDispatched, c.GoferRequired = true, true
				c.GoferLaunchState = processBirthPublished
			}
			if _, _, err := c.managedProcessRequirements(); err == nil {
				t.Fatal("contradictory launch facts were accepted")
			}
		})
	}
}
