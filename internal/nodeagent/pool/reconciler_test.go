/*
Copyright 2026 The Setec Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package pool

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

func TestTickReconciler_FiresImmediatelyAndOnTick(t *testing.T) {
	m := newTestManager(newFakeStorage(), &countingPrefetcher{}, &fakeFirecracker{}, 4)

	// Use a Lister that records how many times it's been asked.
	var listed atomic.Int32
	buf := make([]setecv1alpha1.SandboxClass, 0, 1)
	buf = append(buf, newClass("img:v1", 0, time.Hour)) // size 0 → no boot work, fast

	r := &TickReconciler{
		Manager: m,
		Lister: func() []setecv1alpha1.SandboxClass {
			listed.Add(1)
			return buf
		},
		Interval: 25 * time.Millisecond,
		Logger:   func(string, ...any) {},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	r.Run(ctx)

	// Expect immediate fire plus at least two more ticks in 120ms at 25ms cadence.
	if listed.Load() < 3 {
		t.Fatalf("lister was called %d times, expected >=3", listed.Load())
	}
}

func TestTickReconciler_ExitsOnCancel(t *testing.T) {
	m := newTestManager(newFakeStorage(), &countingPrefetcher{}, &fakeFirecracker{}, 4)
	r := &TickReconciler{
		Manager:  m,
		Lister:   func() []setecv1alpha1.SandboxClass { return nil },
		Interval: 10 * time.Millisecond,
		Logger:   func(string, ...any) {},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	// Let it tick at least once then cancel.
	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("reconciler did not exit after context cancel")
	}
}

func TestTickReconciler_MissingDeps(t *testing.T) {
	r := &TickReconciler{Logger: func(string, ...any) {}}
	ctx := t.Context()
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("missing deps path should exit immediately")
	}
}
