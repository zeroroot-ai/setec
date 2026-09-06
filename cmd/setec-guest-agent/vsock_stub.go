// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build !linux

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

package main

import (
	"errors"
	"net"

	"github.com/zeroroot-ai/setec/internal/entropy"
)

// listenVsock is Linux-only; the agent runs inside a Linux microVM.
func listenVsock(uint32) (net.Listener, error) {
	return nil, errors.New("setec-guest-agent: AF_VSOCK is only supported on linux")
}

// newKernelPool has no non-Linux implementation; it is never reached
// because listenVsock fails first.
func newKernelPool(string) entropy.Pool { return failingPool{} }

type failingPool struct{}

func (failingPool) AddEntropy([]byte) error {
	return errors.New("setec-guest-agent: RNDADDENTROPY is only supported on linux")
}
