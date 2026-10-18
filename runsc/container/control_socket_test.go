// Copyright 2026 The gVisor Authors.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/sandbox"
)

// These tests exercise exact file retirement only. They do not fabricate
// process births or claim an offline cgroup or a completed physical deletion.
func controlSocketOwner(t *testing.T) (*Container, string) {
	t.Helper()
	c := birthOwnerFixture(t)
	path := filepath.Join(c.Saver.RootDir, "runsc-"+c.ID+".sock")
	c.Sandbox = &sandbox.Sandbox{ID: c.ID, ResourceGeneration: c.ResourceGeneration, ControlSocketPath: path}
	if err := c.Saver.lock(BlockAcquire); err != nil {
		t.Fatal(err)
	}
	if err := c.Saver.SaveLockedDurable(c); err != nil {
		t.Fatal(err)
	}
	c.Saver.UnlockOrDie()
	t.Cleanup(func() { _ = c.Saver.close() })
	return c, path
}

func bindControlSocketFile(t *testing.T, path string) {
	t.Helper()
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	address := filepath.Join("/proc/self/fd", strconv.Itoa(int(directory.Fd())), filepath.Base(path))
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: address}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
}

func TestManagedControlSocketRetirementAllowsSameCanonicalNameAgain(t *testing.T) {
	c, path := controlSocketOwner(t)
	bindControlSocketFile(t, path)
	lockControlSocketTestOwner(t, c)
	original, err := c.prepareManagedControlSocket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer original.close()
	if err := original.remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("retired original socket remains: %v", err)
	}
	bindControlSocketFile(t, path)
	if err := original.remove(context.Background()); err == nil {
		t.Fatal("original retirement removed a replacement socket")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("replacement socket changed: %v", err)
	}
}

func TestManagedControlSocketRetirementRejectsForeignPathsAndObjects(t *testing.T) {
	for _, kind := range []string{"foreign-root", "foreign-name", "regular", "symlink", "hardlink", "writable"} {
		t.Run(kind, func(t *testing.T) {
			c, path := controlSocketOwner(t)
			switch kind {
			case "foreign-root":
				c.Sandbox.ControlSocketPath = filepath.Join(t.TempDir(), filepath.Base(path))
				bindControlSocketFile(t, c.Sandbox.ControlSocketPath)
			case "foreign-name":
				c.Sandbox.ControlSocketPath = filepath.Join(c.Saver.RootDir, "runsc-neighbor.sock")
				bindControlSocketFile(t, c.Sandbox.ControlSocketPath)
			case "regular":
				if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				foreign := filepath.Join(t.TempDir(), "foreign")
				if err := os.WriteFile(foreign, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(foreign, path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				bindControlSocketFile(t, path)
				if err := os.Link(path, path+".neighbor"); err != nil {
					t.Fatal(err)
				}
			case "writable":
				bindControlSocketFile(t, path)
				if err := os.Chmod(path, 0777); err != nil {
					t.Fatal(err)
				}
			}
			lockControlSocketTestOwner(t, c)
			if original, err := c.prepareManagedControlSocket(context.Background()); err == nil {
				original.close()
				t.Fatal("foreign or unsafe socket accepted")
			}
			if _, err := os.Lstat(c.Sandbox.ControlSocketPath); err != nil {
				t.Fatalf("refused object changed: %v", err)
			}
		})
	}
}

func TestManagedControlSocketRetirementKeepsChangedOriginalAndNeighbor(t *testing.T) {
	for _, kind := range []string{"replacement", "new-after-absence", "directory", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			c, path := controlSocketOwner(t)
			if kind != "new-after-absence" {
				bindControlSocketFile(t, path)
			}
			lockControlSocketTestOwner(t, c)
			original, err := c.prepareManagedControlSocket(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer original.close()
			ctx := context.Background()
			switch kind {
			case "replacement":
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				bindControlSocketFile(t, path)
			case "new-after-absence":
				bindControlSocketFile(t, path)
			case "directory":
				if err := os.Rename(c.Saver.RootDir, c.Saver.RootDir+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(c.Saver.RootDir, 0700); err != nil {
					t.Fatal(err)
				}
				bindControlSocketFile(t, path)
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := original.remove(ctx); err == nil {
				t.Fatal("changed or cancelled retirement accepted")
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("replacement or original changed: %v", err)
			}
		})
	}
}

// 此用例构造已经持久保存的改名中间态，只验证精确文件重放。
// 它不伪造运行时出生或离线分组证明，也不声称执行了真实进程崩溃。
func controlSocketRetirementFixture(t *testing.T, phase string) (*Container, string, string, map[string]any) {
	t.Helper()
	c, canonical := controlSocketOwner(t)
	bindControlSocketFile(t, canonical)
	var root, original unix.Stat_t
	if err := unix.Lstat(c.Saver.RootDir, &root); err != nil {
		t.Fatal(err)
	}
	if err := unix.Lstat(canonical, &original); err != nil {
		t.Fatal(err)
	}
	private := ".scheduler-control-retire-0123456789abcdef0123456789abcdef"
	intent := map[string]any{
		"version": 1, "containerID": c.ID, "resourceGeneration": c.ResourceGeneration,
		"rootPath": c.Saver.RootDir, "rootDevice": root.Dev, "rootInode": root.Ino,
		"rootMode": root.Mode, "rootUID": root.Uid, "rootGID": root.Gid,
		"canonicalName": filepath.Base(canonical), "privateName": private, "phase": phase,
		"socket": map[string]any{
			"device": original.Dev, "inode": original.Ino, "mode": original.Mode,
			"uid": original.Uid, "gid": original.Gid, "links": original.Nlink,
		},
	}
	if err := os.Rename(canonical, filepath.Join(c.Saver.RootDir, private)); err != nil {
		t.Fatal(err)
	}
	return c, canonical, filepath.Join(c.Saver.RootDir, private), intent
}

func reloadControlSocketRetirement(t *testing.T, c *Container, intent map[string]any) *Container {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["controlSocketRetirement"], err = json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Saver.statePath(), raw, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(c.Saver.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	directory, err := os.Open(c.Saver.RootDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Sync(); err != nil {
		directory.Close()
		t.Fatal(err)
	}
	directory.Close()
	var reloaded Container
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatal(err)
	}
	reloaded.Saver = StateFile{RootDir: c.Saver.RootDir, ID: c.Saver.ID}
	if err := reloaded.Saver.lock(BlockAcquire); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		reloaded.Saver.UnlockOrDie()
		_ = reloaded.Saver.close()
	})
	if err := reloaded.verifyLockedProcessOwner(reloaded.ResourceGeneration); err != nil {
		t.Fatal(err)
	}
	return &reloaded
}

func TestManagedControlSocketRetirementReplaysDurableRename(t *testing.T) {
	for _, phase := range []string{"prepared", "renamed"} {
		for _, replacement := range []bool{false, true} {
			t.Run(phase+"/replacement="+strconv.FormatBool(replacement), func(t *testing.T) {
				c, canonical, private, intent := controlSocketRetirementFixture(t, phase)
				neighbor := filepath.Join(c.Saver.RootDir, "runsc-neighbor.sock")
				bindControlSocketFile(t, neighbor)
				beforeNeighbor, err := os.Lstat(neighbor)
				if err != nil {
					t.Fatal(err)
				}
				var beforeReplacement os.FileInfo
				if replacement {
					bindControlSocketFile(t, canonical)
					beforeReplacement, err = os.Lstat(canonical)
					if err != nil {
						t.Fatal(err)
					}
				}
				reloaded := reloadControlSocketRetirement(t, c, intent)
				lease, err := reloaded.prepareManagedControlSocket(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer lease.close()
				if err := lease.remove(context.Background()); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(private); !os.IsNotExist(err) {
					t.Fatalf("durably recorded original private socket remains after replay: %v", err)
				}
				if replacement {
					after, err := os.Lstat(canonical)
					if err != nil || !os.SameFile(beforeReplacement, after) {
						t.Fatalf("replay changed the replacement canonical socket: %v", err)
					}
				} else if _, err := os.Lstat(canonical); !os.IsNotExist(err) {
					t.Fatalf("replay recreated the retired canonical socket: %v", err)
				}
				afterNeighbor, err := os.Lstat(neighbor)
				if err != nil || !os.SameFile(beforeNeighbor, afterNeighbor) {
					t.Fatalf("replay changed a neighbor socket: %v", err)
				}
				raw, err := os.ReadFile(reloaded.Saver.statePath())
				if err != nil {
					t.Fatal(err)
				}
				var final map[string]json.RawMessage
				if err := json.Unmarshal(raw, &final); err != nil {
					t.Fatal(err)
				}
				if value, ok := final["controlSocketRetirement"]; ok && string(value) != "null" {
					t.Fatal("completed socket retirement intent remains after durable replay")
				}
			})
		}
	}
}

func lockControlSocketTestOwner(t *testing.T, c *Container) {
	t.Helper()
	if err := c.Saver.lock(BlockAcquire); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c.Saver.flock != nil && c.Saver.flock.Locked() {
			c.Saver.UnlockOrDie()
		}
		_ = c.Saver.close()
	})
}

func reloadSavedControlSocketOwner(t *testing.T, c *Container) *Container {
	t.Helper()
	raw, err := os.ReadFile(c.Saver.statePath())
	if err != nil {
		t.Fatal(err)
	}
	c.Saver.UnlockOrDie()
	var fresh Container
	if err := json.Unmarshal(raw, &fresh); err != nil {
		t.Fatal(err)
	}
	fresh.Saver = StateFile{RootDir: c.Saver.RootDir, ID: c.Saver.ID}
	lockControlSocketTestOwner(t, &fresh)
	if err := fresh.verifyLockedProcessOwner(fresh.ResourceGeneration); err != nil {
		t.Fatal(err)
	}
	return &fresh
}

func TestManagedControlSocketRetirementReplaysBeforeRename(t *testing.T) {
	c, canonical, private, intent := controlSocketRetirementFixture(t, "prepared")
	if err := os.Rename(private, canonical); err != nil {
		t.Fatal(err)
	}
	fresh := reloadControlSocketRetirement(t, c, intent)
	lease, err := fresh.prepareManagedControlSocket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.close()
	if err := lease.remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{canonical, private} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("original socket remains at %q: %v", path, err)
		}
	}
}

func TestManagedControlSocketRetirementReplaysAfterUnlink(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(strconv.FormatBool(replacement), func(t *testing.T) {
			c, canonical, private, intent := controlSocketRetirementFixture(t, "renamed")
			if err := os.Remove(private); err != nil {
				t.Fatal(err)
			}
			var before os.FileInfo
			var err error
			if replacement {
				bindControlSocketFile(t, canonical)
				before, err = os.Lstat(canonical)
				if err != nil {
					t.Fatal(err)
				}
			}
			fresh := reloadControlSocketRetirement(t, c, intent)
			lease, err := fresh.prepareManagedControlSocket(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer lease.close()
			if err := lease.remove(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !fresh.ControlSocketRetired || fresh.ControlSocketRetirement != nil {
				t.Fatal("original file completion witness was not recorded")
			}
			if replacement {
				after, err := os.Lstat(canonical)
				if err != nil || !os.SameFile(before, after) {
					t.Fatal("post-unlink replay changed a canonical replacement")
				}
			}
		})
	}
}

func TestManagedControlSocketRetirementCompletionSurvivesReload(t *testing.T) {
	c, canonical := controlSocketOwner(t)
	bindControlSocketFile(t, canonical)
	lockControlSocketTestOwner(t, c)
	lease, err := c.prepareManagedControlSocket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.close()
	if err := lease.remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	bindControlSocketFile(t, canonical)
	before, err := os.Lstat(canonical)
	if err != nil {
		t.Fatal(err)
	}
	fresh := reloadSavedControlSocketOwner(t, c)
	if !fresh.ControlSocketRetired || fresh.ControlSocketRetirement != nil {
		t.Fatal("durable socket completion was lost during reload")
	}
	replay, err := fresh.prepareManagedControlSocket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer replay.close()
	if err := replay.remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(canonical)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("completed-state retry removed a later canonical socket")
	}
}

func TestManagedControlSocketRetirementRejectsChangedIntentAndPrivateFile(t *testing.T) {
	for _, kind := range []string{"version", "container", "generation", "root-inode", "root-path", "canonical", "private-name", "phase", "socket", "private-replacement"} {
		t.Run(kind, func(t *testing.T) {
			c, _, private, intent := controlSocketRetirementFixture(t, "renamed")
			switch kind {
			case "version":
				intent["version"] = 2
			case "container":
				intent["containerID"] = "neighbor"
			case "generation":
				intent["resourceGeneration"] = "generation-neighbor"
			case "root-inode":
				intent["rootInode"] = 0
			case "root-path":
				intent["rootPath"] = filepath.Dir(c.Saver.RootDir)
			case "canonical":
				intent["canonicalName"] = "runsc-neighbor.sock"
			case "private-name":
				intent["privateName"] = "../neighbor"
			case "phase":
				intent["phase"] = "complete"
			case "socket":
				intent["socket"] = nil
			case "private-replacement":
				if err := os.Rename(private, private+".original"); err != nil {
					t.Fatal(err)
				}
				bindControlSocketFile(t, private)
			}
			before, err := os.Lstat(private)
			if err != nil {
				t.Fatal(err)
			}
			fresh := reloadControlSocketRetirement(t, c, intent)
			stateBefore, err := os.ReadFile(fresh.Saver.statePath())
			if err != nil {
				t.Fatal(err)
			}
			if lease, err := fresh.prepareManagedControlSocket(context.Background()); err == nil {
				lease.close()
				t.Fatal("changed retirement intent or private file was accepted")
			}
			after, err := os.Lstat(private)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("refused retirement changed the private file")
			}
			stateAfter, err := os.ReadFile(fresh.Saver.statePath())
			if err != nil || string(stateBefore) != string(stateAfter) {
				t.Fatal("refused retirement changed persistent owner state")
			}
		})
	}
}

func TestManagedControlSocketRetirementRequiresOwnerLock(t *testing.T) {
	c, canonical := controlSocketOwner(t)
	bindControlSocketFile(t, canonical)
	if lease, err := c.prepareManagedControlSocket(context.Background()); err == nil {
		lease.close()
		t.Fatal("unlocked native owner accepted socket retirement")
	}
	if _, err := os.Lstat(canonical); err != nil {
		t.Fatal(err)
	}
}

func TestManagedControlSocketRetirementJournalKeepsOriginalDirectory(t *testing.T) {
	c, _ := controlSocketOwner(t)
	lockControlSocketTestOwner(t, c)
	directory, original, err := originalStateDirectory(c.Saver.RootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	originalBytes, err := os.ReadFile(c.Saver.statePath())
	if err != nil {
		t.Fatal(err)
	}
	kept := c.Saver.RootDir + ".original"
	if err := os.Rename(c.Saver.RootDir, kept); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(kept) })
	if err := os.Mkdir(c.Saver.RootDir, 0700); err != nil {
		t.Fatal(err)
	}
	const replacement = "foreign replacement owner"
	if err := os.WriteFile(c.Saver.statePath(), []byte(replacement), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.Saver.saveLockedDurableAt(c, directory, original); err == nil {
		t.Fatal("journal save accepted a replacement state directory")
	}
	if raw, err := os.ReadFile(c.Saver.statePath()); err != nil || string(raw) != replacement {
		t.Fatal("journal save overwrote a replacement owner")
	}
	oldPath := filepath.Join(kept, filepath.Base(c.Saver.statePath()))
	if raw, err := os.ReadFile(oldPath); err != nil || string(raw) != string(originalBytes) {
		t.Fatal("refused journal save changed original owner bytes")
	}
}

func TestManagedControlSocketRetirementPersistsIntentBeforeRename(t *testing.T) {
	c, canonical := controlSocketOwner(t)
	bindControlSocketFile(t, canonical)
	lockControlSocketTestOwner(t, c)
	lease, err := c.prepareManagedControlSocket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.close()
	before, err := os.Lstat(canonical)
	if err != nil {
		t.Fatal(err)
	}
	statePath := c.Saver.statePath()
	if err := os.Rename(statePath, statePath+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := lease.remove(context.Background()); err == nil {
		t.Fatal("socket moved despite failed durable intent publication")
	}
	after, err := os.Lstat(canonical)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("failed intent publication changed the original canonical socket")
	}
	private, err := filepath.Glob(filepath.Join(c.Saver.RootDir, controlRetirementPrefix+"*"))
	if err != nil || len(private) != 0 {
		t.Fatalf("failed intent publication left an unrecorded private socket: %v %v", private, err)
	}
}
