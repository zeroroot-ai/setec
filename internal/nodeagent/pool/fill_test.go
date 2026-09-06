// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package pool

import (
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

func TestFillByClass_ReportsPerClassCounts(t *testing.T) {
	m := newTestManager(newFakeStorage(), &countingPrefetcher{}, &fakeFirecracker{}, 4)
	classA := newClass("img:v1", 2, time.Hour)
	classB := setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "beta"},
		Spec: setecv1alpha1.SandboxClassSpec{
			VMM:             setecv1alpha1.VMMFirecracker,
			PreWarmPoolSize: 3,
			PreWarmImage:    "img:v2",
			PreWarmTTL:      &metav1.Duration{Duration: time.Hour},
		},
	}

	if err := m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{classA, classB}); err != nil {
		t.Fatalf("ReconcilePools: %v", err)
	}

	fills := m.FillByClass()
	if fills["std"] != 2 || fills["beta"] != 3 {
		t.Fatalf("fills = %v, want std=2 beta=3", fills)
	}

	// Dropping a class from the desired set drains it — and it must
	// vanish from the fill map so gauge exporters can Reset+set.
	if err := m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{classA}); err != nil {
		t.Fatalf("ReconcilePools (drop beta): %v", err)
	}
	fills = m.FillByClass()
	if _, ok := fills["beta"]; ok {
		t.Fatalf("fills = %v, drained class must not appear", fills)
	}
	if fills["std"] != 2 {
		t.Fatalf("fills = %v, want std=2", fills)
	}
}

func TestTickReconciler_FillObserverSeesCounts(t *testing.T) {
	m := newTestManager(newFakeStorage(), &countingPrefetcher{}, &fakeFirecracker{}, 4)
	cls := newClass("img:v1", 2, time.Hour)

	var mu sync.Mutex
	var last map[string]int
	r := &TickReconciler{
		Manager:  m,
		Lister:   func() []setecv1alpha1.SandboxClass { return []setecv1alpha1.SandboxClass{cls} },
		Interval: 20 * time.Millisecond,
		Logger:   func(string, ...any) {},
		FillObserver: func(fills map[string]int) {
			mu.Lock()
			last = fills
			mu.Unlock()
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if last == nil {
		t.Fatal("FillObserver was never invoked")
	}
	if last["std"] != 2 {
		t.Fatalf("observed fills = %v, want std=2", last)
	}
}
