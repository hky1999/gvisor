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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/unet"
)

// ParkFile identifies a backing file retained across a non-resuming save.
type ParkFile struct {
	ID         ResourceID `json:"id"`
	ChunkCount uint64     `json:"chunk_count"`
}

type parkPacket struct {
	File *ParkFile `json:"file,omitempty"`
	Done bool      `json:"done,omitempty"`
}

// ParkTransferError identifies a failure before the kernel state was captured.
// The kernel may resume after this error because no external save was started.
type ParkTransferError struct{ Err error }

func (e *ParkTransferError) Error() string {
	return fmt.Sprintf("retaining park filestores: %v", e.Err)
}

// Unwrap returns the transfer failure.
func (e *ParkTransferError) Unwrap() error { return e.Err }

// SendParkFile transfers a reference over a private SOCK_SEQPACKET connection.
// The sender retains ownership of file. The receiver must acknowledge all files
// before the sender may stop the sandbox.
func SendParkFile(socket *unet.Socket, entry ParkFile, file *os.File) error {
	data, err := json.Marshal(parkPacket{File: &entry})
	if err != nil {
		return err
	}
	if len(data) > 65536 {
		return fmt.Errorf("park filestore identity is too large")
	}
	w := socket.Writer(true)
	w.PackFDs(int(file.Fd()))
	n, err := w.WriteVec([][]byte{data})
	runtime.KeepAlive(file)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

// FinishParkTransfer waits for the receiver to confirm ownership and validation.
func FinishParkTransfer(socket *unet.Socket) error {
	data := []byte(`{"done":true}`)
	if n, err := socket.Write(data); err != nil {
		return err
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	var ack [1]byte
	if n, err := socket.Read(ack[:]); err != nil {
		return err
	} else if n != 1 || ack[0] != 1 {
		return fmt.Errorf("park receiver did not acknowledge filestores")
	}
	return nil
}

// ReceiveParkFiles receives references and validates each before accepting it.
// On success the caller owns all files and must explicitly acknowledge the
// transfer by writing byte 1. On failure all received files are closed.
func ReceiveParkFiles(socket *unet.Socket, validate func(ParkFile, *os.File) error) (entries []ParkFile, files []*os.File, err error) {
	defer func() {
		if err != nil {
			for _, file := range files {
				file.Close()
			}
			files = nil
		}
	}()
	seen := make(map[ResourceID]bool)
	buf := make([]byte, 65537)
	for {
		r := socket.Reader(true)
		r.EnableFDs(1)
		n, readErr := r.ReadVec([][]byte{buf})
		if readErr != nil {
			r.CloseFDs()
			return entries, files, readErr
		}
		fds, fdErr := r.ExtractFDs()
		if fdErr != nil {
			r.CloseFDs()
			return entries, files, fdErr
		}
		var received []*os.File
		for _, fd := range fds {
			unix.CloseOnExec(fd)
			received = append(received, os.NewFile(uintptr(fd), "park-filestore"))
		}
		files = append(files, received...)
		if n == 0 {
			return entries, files, io.ErrUnexpectedEOF
		}
		if n > 65536 {
			return entries, files, fmt.Errorf("oversized park packet")
		}
		var packet parkPacket
		if err := json.Unmarshal(buf[:n], &packet); err != nil {
			return entries, files, err
		}
		if packet.Done {
			if packet.File != nil || len(received) != 0 {
				return entries, files, fmt.Errorf("invalid park completion packet")
			}
			return entries, files, nil
		}
		if packet.File == nil || len(received) != 1 || len(entries) >= 1024 {
			return entries, files, fmt.Errorf("invalid park filestore packet")
		}
		if seen[packet.File.ID] {
			return entries, files, fmt.Errorf("duplicate park resource %+v", packet.File.ID)
		}
		if err := validate(*packet.File, received[0]); err != nil {
			return entries, files, err
		}
		seen[packet.File.ID] = true
		entries = append(entries, *packet.File)
	}
}
