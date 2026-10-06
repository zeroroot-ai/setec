// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package grpcserver

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/zeroroot-ai/setec/internal/snapshot/secretscan"
)

// The stored form of a launcher snapshot: the Firecracker state, the
// memory and the writable layer of the machine. The memory and the
// writable layer are sparse, so only their data extents are stored. A diff
// snapshot names its parent: its memory holds only the pages that changed
// since the parent, and a restore puts them on the memory of the parent.
//
//	magic(8) parentLen(8) parent
//	stateSize(8) state
//	memSize(8) extentCount(8) { offset(8) length(8) data }...
//	diskSize(8) extentCount(8) { offset(8) length(8) data }...
//
// A kata snapshot has another form (makeFramedReader), and the magic keeps
// one from loading as the other.
var launcherFrameMagic = [8]byte{'S', 'E', 'T', 'E', 'C', 'L', '1', '\n'}

// maxParentChain bounds the parents that one restore follows, and
// maxParentRef the length of a parent reference.
const (
	maxParentChain = 64
	maxParentRef   = 4096
)

// maxExtents bounds the extent count that a restore accepts.
const maxExtents = 1 << 20

type extent struct{ off, n int64 }

// dataExtents returns the data extents of f, whose size is size. A file
// system with no SEEK_DATA support reports the whole file as data.
func dataExtents(f *os.File, size int64) ([]extent, error) {
	var out []extent
	fd := int(f.Fd()) //nolint:gosec // a file descriptor fits in an int
	for off := int64(0); off < size; {
		start, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break
		}
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			return []extent{{0, size}}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("seek data: %w", err)
		}
		end, err := unix.Seek(fd, start, unix.SEEK_HOLE)
		if err != nil {
			return nil, fmt.Errorf("seek hole: %w", err)
		}
		end = min(end, size)
		out = append(out, extent{start, end - start})
		off = end
	}
	return out, nil
}

// copySparse copies src to dst and keeps the holes of src. The node agent
// copies the writable layer while the machine is paused, so the copy and
// the memory file are one consistent state.
func copySparse(src, dst string) (err error) {
	in, err := os.Open(src) //nolint:gosec // a path in the work volume of the Pod
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // a path of the node agent
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	exts, err := dataExtents(in, st.Size())
	if err != nil {
		return err
	}
	for _, e := range exts {
		if _, err := io.Copy(io.NewOffsetWriter(out, e.off), io.NewSectionReader(in, e.off, e.n)); err != nil {
			return err
		}
	}
	return out.Truncate(st.Size())
}

// makeLauncherFramedReader frames the state, the memory and the writable
// layer of a launcher snapshot. parentRef is empty for a full snapshot.
func makeLauncherFramedReader(parentRef, statePath, memPath, diskPath string) (io.ReadCloser, error) {
	var files []*os.File
	closeAll := func() {
		for _, f := range files {
			_ = f.Close()
		}
	}
	open := func(p string) (*os.File, int64, error) {
		f, err := os.Open(p) //nolint:gosec // a path of the node agent
		if err != nil {
			return nil, 0, err
		}
		files = append(files, f)
		st, err := f.Stat()
		if err != nil {
			return nil, 0, err
		}
		return f, st.Size(), nil
	}
	u64 := func(v ...int64) io.Reader {
		var b bytes.Buffer
		for _, x := range v {
			_ = binary.Write(&b, binary.BigEndian, uint64(x)) //nolint:gosec // sizes and offsets are not negative
		}
		return &b
	}
	readers := []io.Reader{bytes.NewReader(launcherFrameMagic[:]), u64(int64(len(parentRef))), strings.NewReader(parentRef)}
	state, stateSize, err := open(statePath)
	if err != nil {
		closeAll()
		return nil, err
	}
	readers = append(readers, u64(stateSize), state)
	for _, p := range []string{memPath, diskPath} {
		f, size, err := open(p)
		if err != nil {
			closeAll()
			return nil, err
		}
		exts, err := dataExtents(f, size)
		if err != nil {
			closeAll()
			return nil, err
		}
		readers = append(readers, u64(size, int64(len(exts))))
		for _, e := range exts {
			readers = append(readers, u64(e.off, e.n), io.NewSectionReader(f, e.off, e.n))
		}
	}
	closers := make([]io.Closer, 0, len(files))
	for _, f := range files {
		closers = append(closers, f)
	}
	return &multiReadCloser{reader: io.MultiReader(readers...), closers: closers}, nil
}

// openParentFunc opens the stored stream of a parent snapshot.
type openParentFunc func(ref string) (io.ReadCloser, error)

// writeLauncherFramedStream reverses makeLauncherFramedReader. A diff
// snapshot loads its parents first, with openParent, and then puts its
// changed pages on their memory.
func writeLauncherFramedStream(r io.Reader, statePath, memPath, diskPath string, openParent openParentFunc) error {
	return writeLauncherFrame(r, statePath, memPath, diskPath, openParent, 0)
}

func writeLauncherFrame(r io.Reader, statePath, memPath, diskPath string, openParent openParentFunc, depth int) error {
	var magic [8]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return fmt.Errorf("read the frame magic: %w", err)
	}
	if magic != launcherFrameMagic {
		return errors.New("the snapshot is not a launcher snapshot")
	}
	var n uint64
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return fmt.Errorf("read the parent length: %w", err)
	}
	if n > maxParentRef {
		return fmt.Errorf("the parent reference is %d bytes, more than %d", n, maxParentRef)
	}
	parent := make([]byte, n)
	if _, err := io.ReadFull(r, parent); err != nil {
		return fmt.Errorf("read the parent reference: %w", err)
	}
	if n > 0 {
		if depth >= maxParentChain {
			return fmt.Errorf("the snapshot has more than %d parents", maxParentChain)
		}
		if openParent == nil {
			return errors.New("the snapshot is a diff and no parent can be opened")
		}
		prc, err := openParent(string(parent))
		if err != nil {
			return fmt.Errorf("open the parent %s: %w", parent, err)
		}
		err = writeLauncherFrame(prc, statePath, memPath, diskPath, openParent, depth+1)
		_ = prc.Close()
		if err != nil {
			return fmt.Errorf("parent %s: %w", parent, err)
		}
	}
	var stateSize uint64
	if err := binary.Read(r, binary.BigEndian, &stateSize); err != nil {
		return fmt.Errorf("read the state size: %w", err)
	}
	if err := writeN(r, statePath, int64(stateSize)); err != nil { //nolint:gosec // a size of the stored stream
		return fmt.Errorf("state: %w", err)
	}
	// The memory of a diff goes on the memory of its parent. The writable
	// layer is always whole.
	if err := writeExtents(r, memPath, n == 0); err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	if err := writeExtents(r, diskPath, true); err != nil {
		return fmt.Errorf("writable layer: %w", err)
	}
	return nil
}

// writeExtents reads a size, an extent count and the extents, and writes
// them into path. fresh starts from an empty file; otherwise the extents go
// on the bytes that are there.
func writeExtents(r io.Reader, path string, fresh bool) error {
	var h [2]uint64
	if err := binary.Read(r, binary.BigEndian, &h); err != nil {
		return fmt.Errorf("read the header: %w", err)
	}
	size, count := h[0], h[1]
	if count > maxExtents {
		return fmt.Errorf("%d extents, more than %d", count, maxExtents)
	}
	flags := os.O_CREATE | os.O_WRONLY
	if fresh {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o600) //nolint:gosec // a path of the node agent
	if err != nil {
		return err
	}
	for range count {
		var e [2]uint64
		if err := binary.Read(r, binary.BigEndian, &e); err != nil {
			_ = f.Close()
			return fmt.Errorf("read an extent header: %w", err)
		}
		if e[0]+e[1] > size || e[0]+e[1] < e[0] {
			_ = f.Close()
			return errors.New("an extent is outside the file")
		}
		if _, err := io.CopyN(io.NewOffsetWriter(f, int64(e[0])), r, int64(e[1])); err != nil { //nolint:gosec // checked against the size
			_ = f.Close()
			return fmt.Errorf("write an extent: %w", err)
		}
	}
	if err := f.Truncate(int64(size)); err != nil { //nolint:gosec // a size of the stored stream
		_ = f.Close()
		return err
	}
	return f.Close()
}

// scanSparseFiles scans the data extents of each file for secret-shaped
// material. A hole holds no data, so a sparse writable layer of 10 GiB
// costs only its data. Any finding is an error.
func scanSparseFiles(paths []string) error {
	for _, p := range paths {
		f, err := os.Open(p) //nolint:gosec // a path of the node agent
		if err != nil {
			return err
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return err
		}
		exts, err := dataExtents(f, st.Size())
		if err != nil {
			_ = f.Close()
			return err
		}
		readers := make([]io.Reader, 0, len(exts))
		for _, e := range exts {
			readers = append(readers, io.NewSectionReader(f, e.off, e.n))
		}
		findings, err := secretscan.New().Scan(io.MultiReader(readers...))
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		if len(findings) > 0 {
			return fmt.Errorf("%s: %w (%d findings)", filepath.Base(p), secretscan.ErrSecretsFound, len(findings))
		}
	}
	return nil
}
