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

package uniquify

import "errors"

// errLinuxOnly is returned by every stub applier: the guest side of
// the uniquification exchange only runs inside a Linux microVM.
var errLinuxOnly = errors.New("uniquify: guest appliers are only supported on linux")

// LinuxIdentity has no non-Linux implementation; every method fails
// so a mis-built guest agent acks StatusError and the host fails the
// restore closed.
type LinuxIdentity struct{}

// NewLinuxIdentity returns the failing stub.
func NewLinuxIdentity() *LinuxIdentity { return &LinuxIdentity{} }

func (*LinuxIdentity) ApplyMachineID(string) error { return errLinuxOnly }
func (*LinuxIdentity) ApplyBootID(string) error    { return errLinuxOnly }
func (*LinuxIdentity) ApplyHostname(string) error  { return errLinuxOnly }
func (*LinuxIdentity) Read() (string, string, string, error) {
	return "", "", "", errLinuxOnly
}

// LinuxNetwork has no non-Linux implementation.
type LinuxNetwork struct{}

// NewLinuxNetwork returns the failing stub.
func NewLinuxNetwork() *LinuxNetwork { return &LinuxNetwork{} }

func (*LinuxNetwork) Reconcile(string) ([]string, error) { return nil, errLinuxOnly }

// VsockCID has no non-Linux implementation.
type VsockCID struct{}

// LocalCID fails on non-Linux hosts.
func (VsockCID) LocalCID() (uint32, error) { return 0, errLinuxOnly }
