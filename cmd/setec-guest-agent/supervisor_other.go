// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build !linux

package main

import (
	"context"
	"errors"

	"github.com/zeroroot-ai/setec/internal/uniquify"
)

func prepareMachine() error {
	return errors.New("setec-guest-agent: the supervisor mode is only supported on linux")
}

func endMachine() {}

func runSupervisor(context.Context, func(string, ...any)) error {
	return errors.New("setec-guest-agent: the supervisor mode is only supported on linux")
}

func workloadIdentity(bool) *uniquify.LinuxIdentity { return uniquify.NewLinuxIdentity() }
