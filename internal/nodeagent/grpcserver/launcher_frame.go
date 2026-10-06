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

	"golang.org/x/sys/unix"
)

// The stored form of a launcher snapshot: the Firecracker state and memory
// files and the writable layer of the machine. The writable layer is a
// sparse file of the scratch size, so only its data extents are stored.
//
//	magic(8) stateSize(8) memSize(8) state mem
//	diskSize(8) extentCount(8) { offset(8) length(8) data }...
//
// A kata snapshot has another form (makeFramedReader), and the magic keeps
// one from loading as the other.
var launcherFrameMagic = [8]byte{'S', 'E', 'T', 'E', 'C', 'L', '1', '\n'}

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
// layer of a launcher snapshot.
func makeLauncherFramedReader(statePath, memPath, diskPath string) (io.ReadCloser, error) {
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
	state, stateSize, err := open(statePath)
	if err != nil {
		closeAll()
		return nil, err
	}
	mem, memSize, err := open(memPath)
	if err != nil {
		closeAll()
		return nil, err
	}
	disk, diskSize, err := open(diskPath)
	if err != nil {
		closeAll()
		return nil, err
	}
	exts, err := dataExtents(disk, diskSize)
	if err != nil {
		closeAll()
		return nil, err
	}

	var head bytes.Buffer
	head.Write(launcherFrameMagic[:])
	_ = binary.Write(&head, binary.BigEndian, [2]uint64{uint64(stateSize), uint64(memSize)}) //nolint:gosec // sizes are not negative
	readers := []io.Reader{&head, state, mem}
	var dh bytes.Buffer
	_ = binary.Write(&dh, binary.BigEndian, [2]uint64{uint64(diskSize), uint64(len(exts))}) //nolint:gosec // sizes are not negative
	readers = append(readers, &dh)
	for _, e := range exts {
		var eh bytes.Buffer
		_ = binary.Write(&eh, binary.BigEndian, [2]uint64{uint64(e.off), uint64(e.n)}) //nolint:gosec // extents are not negative
		readers = append(readers, &eh, io.NewSectionReader(disk, e.off, e.n))
	}
	closers := make([]io.Closer, 0, len(files))
	for _, f := range files {
		closers = append(closers, f)
	}
	return &multiReadCloser{reader: io.MultiReader(readers...), closers: closers}, nil
}

// writeLauncherFramedStream reverses makeLauncherFramedReader.
func writeLauncherFramedStream(r io.Reader, statePath, memPath, diskPath string) error {
	var magic [8]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return fmt.Errorf("read the frame magic: %w", err)
	}
	if magic != launcherFrameMagic {
		return errors.New("the snapshot is not a launcher snapshot")
	}
	var sizes [2]uint64
	if err := binary.Read(r, binary.BigEndian, &sizes); err != nil {
		return fmt.Errorf("read the frame header: %w", err)
	}
	if err := writeN(r, statePath, int64(sizes[0])); err != nil { //nolint:gosec // a size of the stored stream
		return fmt.Errorf("state: %w", err)
	}
	if err := writeN(r, memPath, int64(sizes[1])); err != nil { //nolint:gosec // a size of the stored stream
		return fmt.Errorf("memory: %w", err)
	}
	var dh [2]uint64
	if err := binary.Read(r, binary.BigEndian, &dh); err != nil {
		return fmt.Errorf("read the disk header: %w", err)
	}
	if dh[1] > maxExtents {
		return fmt.Errorf("the disk has %d extents, more than %d", dh[1], maxExtents)
	}
	f, err := os.OpenFile(diskPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // a path of the node agent
	if err != nil {
		return err
	}
	for range dh[1] {
		var e [2]uint64
		if err := binary.Read(r, binary.BigEndian, &e); err != nil {
			_ = f.Close()
			return fmt.Errorf("read an extent header: %w", err)
		}
		if e[0]+e[1] > dh[0] || e[0]+e[1] < e[0] {
			_ = f.Close()
			return errors.New("an extent of the disk is outside the disk")
		}
		if _, err := io.CopyN(io.NewOffsetWriter(f, int64(e[0])), r, int64(e[1])); err != nil { //nolint:gosec // checked against the disk size
			_ = f.Close()
			return fmt.Errorf("write an extent: %w", err)
		}
	}
	if err := f.Truncate(int64(dh[0])); err != nil { //nolint:gosec // a size of the stored stream
		_ = f.Close()
		return err
	}
	return f.Close()
}
