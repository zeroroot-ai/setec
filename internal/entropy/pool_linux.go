//go:build linux

/*
Copyright 2026 The Setec Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package entropy

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// rndAddEntropy is the Linux RNDADDENTROPY ioctl request:
// _IOW('R', 0x03, int[2]) — writes a struct rand_pool_info into the
// kernel, mixing the buffer into the CRNG input pool AND crediting
// entropy_count bits. A plain write(2) to /dev/urandom would mix but
// not credit; crediting matters because it forces a CRNG reseed and
// unblocks getrandom(2)-style consumers immediately after restore.
const rndAddEntropy uint = 0x40085203

// KernelPool injects entropy into the running kernel via the
// RNDADDENTROPY ioctl on a random device node. This is the production
// Pool implementation inside the guest; it requires CAP_SYS_ADMIN,
// which the setec-guest-agent has (it runs as PID-space root in the
// microVM).
type KernelPool struct {
	// DevicePath is the device node to issue the ioctl against.
	// Defaults to /dev/urandom (the ioctl is accepted on both
	// /dev/random and /dev/urandom; they share the CRNG).
	DevicePath string

	// ioctl is injectable for tests. nil uses the real syscall.
	ioctl func(fd uintptr, req uint, arg unsafe.Pointer) error
}

// AddEntropy mixes p into the kernel entropy pool and credits
// len(p)*8 bits.
func (k *KernelPool) AddEntropy(p []byte) error {
	if len(p) == 0 {
		return fmt.Errorf("entropy: refusing to credit an empty payload")
	}
	if len(p) > MaxPayloadBytes {
		return fmt.Errorf("entropy: payload %d exceeds max %d", len(p), MaxPayloadBytes)
	}
	dev := k.DevicePath
	if dev == "" {
		dev = "/dev/urandom"
	}
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("entropy: open %q: %w", dev, err)
	}
	defer func() { _ = f.Close() }()

	// struct rand_pool_info { int entropy_count; int buf_size; __u32 buf[]; }
	// Backed by a []uint32 so the kernel-facing pointer is word-aligned.
	words := (len(p) + 3) / 4
	info := make([]uint32, 2+words)
	info[0] = uint32(len(p) * 8) // entropy_count, in bits
	info[1] = uint32(len(p))     // buf_size, in bytes
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&info[2])), words*4), p)

	doIoctl := k.ioctl
	if doIoctl == nil {
		doIoctl = realIoctl
	}
	if err := doIoctl(f.Fd(), rndAddEntropy, unsafe.Pointer(&info[0])); err != nil {
		return fmt.Errorf("entropy: RNDADDENTROPY on %q: %w", dev, err)
	}
	return nil
}

// realIoctl issues the raw ioctl syscall.
func realIoctl(fd uintptr, req uint, arg unsafe.Pointer) error {
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, uintptr(req), uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}
