// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package testutil

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
)

// FakeEventsRecorder implements events.EventRecorder by appending formatted
// strings to a buffered channel. Mirrors the surface of record.FakeRecorder
// for test ergonomics. Set channel capacity per test (default 32).
type FakeEventsRecorder struct {
	Events chan string
}

// NewFakeEventsRecorder returns a recorder with a buffered channel of size n.
func NewFakeEventsRecorder(n int) *FakeEventsRecorder {
	return &FakeEventsRecorder{Events: make(chan string, n)}
}

var _ events.EventRecorder = (*FakeEventsRecorder)(nil)

func (f *FakeEventsRecorder) Eventf(_ runtime.Object, _ runtime.Object, eventtype, reason, action, note string, args ...any) {
	f.Events <- fmt.Sprintf("%s %s %s %s", eventtype, reason, action, fmt.Sprintf(note, args...))
}
