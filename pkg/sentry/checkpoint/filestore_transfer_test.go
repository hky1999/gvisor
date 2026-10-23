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

package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/unet"
)

func TestParkTransferOwnsFilesAndRequiresAcknowledgement(t *testing.T) {
	for _, count := range []int{0, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			sender, receiver, err := unet.SocketPair(true)
			if err != nil {
				t.Fatal(err)
			}
			defer sender.Close()
			defer receiver.Close()
			var originals []*os.File
			for i := 0; i < count; i++ {
				file, err := os.Create(filepath.Join(t.TempDir(), "file"))
				if err != nil {
					t.Fatal(err)
				}
				file.WriteString(fmt.Sprint(i))
				originals = append(originals, file)
				defer file.Close()
			}
			finished := make(chan error, 1)
			go func() {
				for i, file := range originals {
					if err := SendParkFile(sender, ParkFile{ID: ResourceID{Path: fmt.Sprint(i)}}, file); err != nil {
						finished <- err
						return
					}
				}
				err := FinishParkTransfer(sender)
				for _, file := range originals {
					file.Close()
				}
				finished <- err
			}()
			entries, files, err := ReceiveParkFiles(receiver, func(ParkFile, *os.File) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				for _, file := range files {
					file.Close()
				}
			}()
			if len(entries) != count || len(files) != count {
				t.Fatalf("received %d entries/%d files", len(entries), len(files))
			}
			select {
			case err := <-finished:
				t.Fatalf("sender finished before acknowledgement: %v", err)
			default:
			}
			if _, err := receiver.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("sender did not observe acknowledgement")
			}
			for i, file := range files {
				var buf [1]byte
				if _, err := file.ReadAt(buf[:], 0); err != nil || string(buf[:]) != fmt.Sprint(i) {
					t.Fatalf("lost retained file: %q %v", buf, err)
				}
			}
		})
	}
}

func TestRejectedParkTransferClosesReceivedReferences(t *testing.T) {
	sender, receiver, err := unet.SocketPair(true)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	defer receiver.Close()
	source, err := os.CreateTemp(t.TempDir(), "file")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	done := make(chan error, 1)
	go func() {
		err := SendParkFile(sender, ParkFile{ID: ResourceID{Path: "/"}}, source)
		if err == nil {
			err = FinishParkTransfer(sender)
		}
		done <- err
	}()
	var rejected *os.File
	_, files, err := ReceiveParkFiles(receiver, func(_ ParkFile, file *os.File) error { rejected = file; return fmt.Errorf("reject resource") })
	if err == nil || files != nil || rejected == nil {
		t.Fatalf("rejection result: %v %v", files, err)
	}
	if _, err := rejected.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("received file was not closed")
	}
	receiver.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("sender accepted rejected transfer")
		}
	case <-time.After(time.Second):
		t.Fatal("sender remained blocked after rejection")
	}
	if _, err := source.Stat(); err != nil {
		t.Fatalf("rejection closed sender-owned file: %v", err)
	}
}

func TestInterruptedParkTransferClosesPartialReferences(t *testing.T) {
	sender, receiver, err := unet.SocketPair(true)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	source, err := os.CreateTemp(t.TempDir(), "file")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := SendParkFile(sender, ParkFile{ID: ResourceID{Path: "/"}}, source); err != nil {
		t.Fatal(err)
	}
	sender.Close()
	var received *os.File
	_, files, err := ReceiveParkFiles(receiver, func(_ ParkFile, file *os.File) error {
		received = file
		return nil
	})
	if err == nil || files != nil || received == nil {
		t.Fatalf("interrupted transfer: %v %v", files, err)
	}
	if _, err := received.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("partial reference survived disconnect: %v", err)
	}
	if _, err := source.Stat(); err != nil {
		t.Fatalf("sender-owned reference closed: %v", err)
	}
}
