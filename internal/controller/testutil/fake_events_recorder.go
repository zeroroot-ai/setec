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
