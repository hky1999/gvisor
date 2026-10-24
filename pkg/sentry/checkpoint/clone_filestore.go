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
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// CloneFilestore copies a stable filestore into a distinct, empty regular
// file on the same filesystem. It tries FICLONE first, then copy_file_range
// when reflink is unsupported. This lets filesystems such as JuiceFS use
// their native range-clone implementation without a userspace read/write
// fallback. copy_file_range does not itself guarantee a metadata-only copy.
//
// The caller must prevent source writes until this returns, discard the
// destination after any error, and sync it before publishing a checkpoint.
// Neither file's seek offset is changed.
func CloneFilestore(src, dst *os.File) error {
	return cloneFilestore(src, dst, func(out, in int) error {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(out), unix.FICLONE, uintptr(in))
		if errno != 0 {
			return errno
		}
		return nil
	}, unix.CopyFileRange)
}

func cloneFilestore(src, dst *os.File, reflink func(int, int) error, copyRange func(int, *int64, int, *int64, int, int) (int, error)) error {
	var source, target unix.Stat_t
	if err := unix.Fstat(int(src.Fd()), &source); err != nil {
		return fmt.Errorf("stat filestore source: %w", err)
	}
	if err := unix.Fstat(int(dst.Fd()), &target); err != nil {
		return fmt.Errorf("stat filestore destination: %w", err)
	}
	if source.Mode&unix.S_IFMT != unix.S_IFREG || target.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("filestore clone requires regular files")
	}
	if source.Dev != target.Dev {
		return fmt.Errorf("filestore clone requires the same filesystem: %w", unix.EXDEV)
	}
	if source.Ino == target.Ino {
		return fmt.Errorf("filestore source and destination refer to the same inode")
	}
	if target.Size != 0 {
		return fmt.Errorf("filestore clone destination must be empty")
	}
	if err := reflink(int(dst.Fd()), int(src.Fd())); err == nil {
		return nil
	} else if err != unix.EOPNOTSUPP && err != unix.ENOTTY && err != unix.EXDEV {
		return fmt.Errorf("reflink filestore: %w", err)
	}
	// Explicit offsets preserve the source's current seek position. Bound each
	// request for platforms whose syscall length is a signed 32-bit value.
	var inOffset, outOffset int64
	for copied := int64(0); copied < source.Size; {
		length := source.Size - copied
		if length > 16<<20 {
			length = 16 << 20
		}
		n, err := copyRange(int(src.Fd()), &inOffset, int(dst.Fd()), &outOffset, int(length), 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("copy filestore range after %d bytes: %w", copied, err)
		}
		if n <= 0 {
			return fmt.Errorf("copy filestore range after %d of %d bytes: %w", copied, source.Size, io.ErrUnexpectedEOF)
		}
		if int64(n) > length {
			return fmt.Errorf("copy filestore range returned %d bytes for a %d-byte request", n, length)
		}
		copied += int64(n)
	}
	if err := dst.Truncate(source.Size); err != nil {
		return fmt.Errorf("set cloned filestore size: %w", err)
	}
	return nil
}
