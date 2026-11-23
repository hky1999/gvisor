package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"
)

const blockSize int64 = 4096

type source uint8

const (
	parent source = iota
	local
	zero
	priorDelta
)

type extent struct {
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	Kind   string `json:"kind"`
}

type counters struct {
	BaseReadBytes   int64 `json:"baseReadBytes"`
	PriorReadBytes  int64 `json:"priorReadBytes"`
	LocalReadBytes  int64 `json:"localReadBytes"`
	LocalWriteBytes int64 `json:"localWriteBytes"`
	PunchCalls      int64 `json:"punchCalls"`
	PunchBytes      int64 `json:"punchBytes"`
}

type snapshot struct {
	counters
	Size        int64    `json:"size"`
	BaseLimit   int64    `json:"baseLimit"`
	DirtyBlocks int      `json:"dirtyBlocks"`
	ZeroBlocks  int      `json:"zeroBlocks"`
	Extents     []extent `json:"extents,omitempty"`
}

type image struct {
	fs.Inode
	mu        sync.Mutex
	base      *os.File
	prior     *os.File
	delta     *os.File
	size      int64
	baseLimit int64
	blocks    map[int64]source
	inherited map[int64]int64
	frozen    bool
	count     counters
}

func (img *image) kind(block int64) source {
	if kind, exists := img.blocks[block]; exists {
		return kind
	}
	if offset, exists := img.inherited[block]; exists {
		if offset < 0 {
			return zero
		}
		return priorDelta
	}
	if block*blockSize < img.baseLimit {
		return parent
	}
	return zero
}

func (img *image) read(buf []byte, offset int64) error {
	for len(buf) != 0 {
		kind := img.kind(offset / blockSize)
		end := min(offset+int64(len(buf)), (offset/blockSize+1)*blockSize)
		for end < offset+int64(len(buf)) && img.kind(end/blockSize) == kind {
			end = min(offset+int64(len(buf)), end+blockSize)
		}
		part := buf[:end-offset]
		var err error
		switch kind {
		case parent:
			_, err = img.base.ReadAt(part, offset)
			img.count.BaseReadBytes += int64(len(part))
		case local:
			_, err = img.delta.ReadAt(part, offset)
			img.count.LocalReadBytes += int64(len(part))
		case priorDelta:
			_, err = img.prior.ReadAt(part, img.inherited[offset/blockSize]+offset%blockSize)
			img.count.PriorReadBytes += int64(len(part))
		case zero:
			clear(part)
		}
		if err != nil {
			return err
		}
		buf = buf[len(part):]
		offset = end
	}
	return nil
}

func (img *image) attr(attr *fuse.Attr) {
	attr.Mode = unix.S_IFREG | 0600
	attr.Nlink = 1
	attr.Size = uint64(img.size)
	attr.Blksize = uint32(blockSize)
}

func (img *image) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	img.mu.Lock()
	defer img.mu.Unlock()
	img.attr(&out.Attr)
	return 0
}

func (img *image) Open(_ context.Context, _ uint32) (fs.FileHandle, uint32, syscall.Errno) {
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (img *image) Read(_ context.Context, _ fs.FileHandle, buf []byte, offset int64) (fuse.ReadResult, syscall.Errno) {
	img.mu.Lock()
	defer img.mu.Unlock()
	if offset < 0 {
		return nil, syscall.EINVAL
	}
	if offset >= img.size {
		return fuse.ReadResultData(nil), 0
	}
	buf = buf[:min(int64(len(buf)), img.size-offset)]
	if err := img.read(buf, offset); err != nil {
		log.Printf("read offset=%d: %v", offset, err)
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(buf), 0
}

func (img *image) Write(_ context.Context, _ fs.FileHandle, data []byte, offset int64) (uint32, syscall.Errno) {
	img.mu.Lock()
	defer img.mu.Unlock()
	if img.frozen {
		return 0, syscall.EROFS
	}
	if offset < 0 || offset+int64(len(data)) > img.size {
		return 0, syscall.EINVAL
	}
	var written uint32
	for len(data) > 0 {
		block := offset / blockSize
		blockStart := block * blockSize
		length := min(int64(len(data)), blockSize-(offset-blockStart))
		value := data[:length]
		if length != blockSize {
			// Preserve the untouched portion of a partially overwritten block.
			var merged [blockSize]byte
			if err := img.read(merged[:], blockStart); err != nil {
				return written, syscall.EIO
			}
			copy(merged[offset-blockStart:], value)
			value = merged[:]
		}
		if _, err := img.delta.WriteAt(value, blockStart); err != nil {
			return written, fs.ToErrno(err)
		}
		img.blocks[block] = local
		img.count.LocalWriteBytes += blockSize
		written += uint32(length)
		offset += length
		data = data[length:]
	}
	return written, 0
}

func (img *image) resize(size int64) syscall.Errno {
	if size == img.size {
		return 0
	}
	if size < 0 || size%blockSize != 0 {
		return syscall.EINVAL
	}
	if img.frozen {
		return syscall.EROFS
	}
	if err := img.delta.Truncate(size); err != nil {
		return fs.ToErrno(err)
	}
	if size < img.size {
		img.baseLimit = min(img.baseLimit, size)
		for block := range img.blocks {
			if block*blockSize >= size {
				delete(img.blocks, block)
			}
		}
		for block := range img.inherited {
			if block*blockSize >= size {
				delete(img.inherited, block)
			}
		}
	}
	img.size = size
	return 0
}

func (img *image) Setattr(_ context.Context, _ fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	img.mu.Lock()
	defer img.mu.Unlock()
	if size, ok := in.GetSize(); ok {
		if errno := img.resize(int64(size)); errno != 0 {
			return errno
		}
	}
	img.attr(&out.Attr)
	return 0
}

func (img *image) Allocate(_ context.Context, _ fs.FileHandle, offset, length uint64, mode uint32) syscall.Errno {
	img.mu.Lock()
	defer img.mu.Unlock()
	if img.frozen {
		return syscall.EROFS
	}
	if offset > uint64(img.size) || length > uint64(img.size)-offset || length == 0 ||
		offset%uint64(blockSize) != 0 || length%uint64(blockSize) != 0 {
		return syscall.EINVAL
	}
	switch mode {
	case unix.FALLOC_FL_PUNCH_HOLE | unix.FALLOC_FL_KEEP_SIZE:
		if err := unix.Fallocate(int(img.delta.Fd()), mode, int64(offset), int64(length)); err != nil {
			return fs.ToErrno(err)
		}
		for pos := int64(offset); pos < int64(offset+length); pos += blockSize {
			if pos < img.baseLimit {
				img.blocks[pos/blockSize] = zero
			} else {
				delete(img.blocks, pos/blockSize)
			}
		}
		img.count.PunchCalls++
		img.count.PunchBytes += int64(length)
	case 0, unix.FALLOC_FL_KEEP_SIZE:
		return fs.ToErrno(unix.Fallocate(int(img.delta.Fd()), mode, int64(offset), int64(length)))
	default:
		return syscall.EOPNOTSUPP
	}
	return 0
}

func (img *image) Fsync(_ context.Context, _ fs.FileHandle, _ uint32) syscall.Errno {
	img.mu.Lock()
	defer img.mu.Unlock()
	return fs.ToErrno(img.delta.Sync())
}

func (img *image) state(freeze bool) (snapshot, error) {
	img.mu.Lock()
	defer img.mu.Unlock()
	report := snapshot{counters: img.count, Size: img.size, BaseLimit: img.baseLimit}
	for _, kind := range img.blocks {
		if kind == local {
			report.DirtyBlocks++
		} else if kind == zero {
			report.ZeroBlocks++
		}
	}
	if !freeze {
		return report, nil
	}
	if err := img.delta.Sync(); err != nil {
		return report, err
	}
	img.frozen = true
	blocks := make([]int64, 0, len(img.blocks))
	for block := range img.blocks {
		blocks = append(blocks, block)
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i] < blocks[j] })
	for _, block := range blocks {
		kind := "data"
		if img.blocks[block] == zero {
			kind = "zero"
		}
		offset := block * blockSize
		if len(report.Extents) != 0 {
			last := &report.Extents[len(report.Extents)-1]
			if last.Kind == kind && last.Offset+last.Length == offset {
				last.Length += blockSize
				continue
			}
		}
		report.Extents = append(report.Extents, extent{Offset: offset, Length: blockSize, Kind: kind})
	}
	return report, nil
}

type directory struct {
	fs.Inode
	img *image
}

func (dir *directory) OnAdd(ctx context.Context) {
	entry := dir.NewPersistentInode(ctx, dir.img, fs.StableAttr{Mode: unix.S_IFREG})
	dir.AddChild("filestore-0", entry, false)
}

func control(listener net.Listener, img *image) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		conn.SetDeadline(time.Now().Add(30 * time.Second))
		request, err := io.ReadAll(io.LimitReader(conn, 16))
		if err == nil {
			op := strings.TrimSpace(string(request))
			if op == "stats" || op == "freeze" {
				report, err := img.state(op == "freeze")
				if err == nil {
					json.NewEncoder(conn).Encode(report)
				} else {
					json.NewEncoder(conn).Encode(map[string]string{"error": err.Error()})
				}
			}
		}
		conn.Close()
	}
}

// usePublishedDelta adds the immutable previous generation to the read path.
// Data records appear in offset order and are stored back-to-back in the pack.
func (img *image) usePublishedDelta(indexPath, dataPath string) error {
	var saved snapshot
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		return err
	}
	if saved.BaseLimit > img.size || saved.BaseLimit < 0 || saved.BaseLimit%blockSize != 0 {
		return fmt.Errorf("invalid parent limit %d", saved.BaseLimit)
	}
	if errno := img.resize(saved.Size); errno != 0 {
		return errno
	}
	img.baseLimit = saved.BaseLimit
	img.inherited = make(map[int64]int64)
	var packSize, priorEnd int64
	for _, record := range saved.Extents {
		if record.Offset < priorEnd || record.Offset%blockSize != 0 || record.Length <= 0 ||
			record.Length%blockSize != 0 || record.Length > img.size-record.Offset {
			return fmt.Errorf("invalid prior extent %+v", record)
		}
		priorEnd = record.Offset + record.Length
		for offset := record.Offset; offset < priorEnd; offset += blockSize {
			switch record.Kind {
			case "zero":
				img.inherited[offset/blockSize] = -1
			case "data":
				img.inherited[offset/blockSize] = packSize
				packSize += blockSize
			default:
				return fmt.Errorf("unknown prior extent kind %q", record.Kind)
			}
		}
	}
	file, err := os.Open(dataPath)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil || info.Size() != packSize {
		file.Close()
		return fmt.Errorf("published delta does not have expected size %d: %v", packSize, err)
	}
	img.prior = file
	return nil
}

func run() error {
	if len(os.Args) != 5 && len(os.Args) != 7 {
		return errors.New("usage: cowfs MOUNTPOINT BASE DELTA SOCKET [PRIOR_INDEX PRIOR_DATA]")
	}
	mount, basePath, deltaPath, socket := os.Args[1], os.Args[2], os.Args[3], os.Args[4]
	base, err := os.Open(basePath)
	if err != nil {
		return err
	}
	defer base.Close()
	info, err := base.Stat()
	if err != nil {
		return err
	}
	if info.Size()%blockSize != 0 {
		return fmt.Errorf("base size %d is not page aligned", info.Size())
	}
	delta, err := os.OpenFile(deltaPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer delta.Close()
	if err := delta.Truncate(info.Size()); err != nil {
		return err
	}
	img := &image{base: base, delta: delta, size: info.Size(), baseLimit: info.Size(), blocks: make(map[int64]source)}
	if len(os.Args) == 7 {
		if err := img.usePublishedDelta(os.Args[5], os.Args[6]); err != nil {
			return err
		}
		defer img.prior.Close()
	}
	server, err := fs.Mount(mount, &directory{img: img}, &fs.Options{MountOptions: fuse.MountOptions{
		Name: "rsi-cow", FsName: "rsi-cow", DirectMountStrict: true, AllowOther: true,
		MaxReadAhead: int(blockSize), MaxWrite: 64 << 10,
	}})
	if err != nil {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		server.Unmount()
		return err
	}
	defer listener.Close()
	go control(listener, img)
	server.Wait()
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
