// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build !linux

package main

import (
	"context"
	"errors"
)

func prepareMachine() error {
	return errors.New("setec-guest-agent: the supervisor mode is only supported on linux")
}

func runSupervisor(context.Context, func(string, ...any)) error {
	return errors.New("setec-guest-agent: the supervisor mode is only supported on linux")
}
