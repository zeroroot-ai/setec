// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

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
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

// TestKernelPool_AddEntropyPacksRandPoolInfo verifies the exact
// rand_pool_info struct handed to the RNDADDENTROPY ioctl: entropy
// credit in BITS, buffer size in bytes, and the payload copied intact.
func TestKernelPool_AddEntropyPacksRandPoolInfo(t *testing.T) {
	dev := filepath.Join(t.TempDir(), "random")
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatalf("create fake device: %v", err)
	}

	var gotReq uint
	var gotInfo []byte
	pool := &KernelPool{
		DevicePath: dev,
		ioctl: func(fd uintptr, req uint, arg unsafe.Pointer) error {
			gotReq = req
			// Capture header + payload from the pointed-to buffer.
			header := unsafe.Slice((*byte)(arg), 8)
			count := int(binary.NativeEndian.Uint32(header[4:8]))
			gotInfo = append([]byte(nil), unsafe.Slice((*byte)(arg), 8+count)...)
			return nil
		},
	}

	payload := bytes.Repeat([]byte{0xC3}, 100) // deliberately not word-aligned
	if err := pool.AddEntropy(payload); err != nil {
		t.Fatalf("AddEntropy: %v", err)
	}
	if gotReq != rndAddEntropy {
		t.Fatalf("ioctl request = %#x, want RNDADDENTROPY %#x", gotReq, rndAddEntropy)
	}
	entropyBits := binary.NativeEndian.Uint32(gotInfo[0:4])
	bufSize := binary.NativeEndian.Uint32(gotInfo[4:8])
	if entropyBits != uint32(len(payload)*8) {
		t.Fatalf("entropy_count = %d bits, want %d", entropyBits, len(payload)*8)
	}
	if bufSize != uint32(len(payload)) {
		t.Fatalf("buf_size = %d, want %d", bufSize, len(payload))
	}
	if !bytes.Equal(gotInfo[8:], payload) {
		t.Fatal("payload was not copied intact into rand_pool_info.buf")
	}
}

func TestKernelPool_AddEntropyRejectsEmpty(t *testing.T) {
	pool := &KernelPool{}
	if err := pool.AddEntropy(nil); err == nil {
		t.Fatal("AddEntropy must reject an empty payload")
	}
}

func TestKernelPool_PropagatesIoctlError(t *testing.T) {
	dev := filepath.Join(t.TempDir(), "random")
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatalf("create fake device: %v", err)
	}
	pool := &KernelPool{
		DevicePath: dev,
		ioctl: func(uintptr, uint, unsafe.Pointer) error {
			return errors.New("EPERM")
		},
	}
	if err := pool.AddEntropy([]byte{1, 2, 3}); err == nil {
		t.Fatal("AddEntropy must propagate ioctl failures (fail closed)")
	}
}

func TestKernelPool_FailsWhenDeviceMissing(t *testing.T) {
	pool := &KernelPool{DevicePath: filepath.Join(t.TempDir(), "nope")}
	if err := pool.AddEntropy([]byte{1}); err == nil {
		t.Fatal("AddEntropy must fail when the device node is missing")
	}
}
