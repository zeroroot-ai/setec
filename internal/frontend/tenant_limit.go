// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"context"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// The limit of Sandboxes of one tenant (docs/multitenancy.md).
//
// MaxSandboxesPerTenant bounds the Sandboxes of one pair namespace that
// hold a machine. Launch and Fork refuse to pass it, with ResourceExhausted. A
// Sandbox that is Completed, Failed or Suspended holds no machine and
// has no place in the count, so a finished session that waits for its
// retention does not block a new one.
//
// A ResourceQuota cannot express this: it counts each Sandbox object,
// also the finished ones.

// tenantGate serializes the launches of each namespace in this process,
// so two launches at the same time cannot both take the last place. Two
// frontend replicas do not share it: each can admit one launch at the
// same instant, so the count can pass the limit by the number of
// replicas less one.
type tenantGate struct {
	mu sync.Mutex
	by map[string]*sync.Mutex
}

func (g *tenantGate) lock(ns string) func() {
	g.mu.Lock()
	if g.by == nil {
		g.by = map[string]*sync.Mutex{}
	}
	m := g.by[ns]
	if m == nil {
		m = &sync.Mutex{}
		g.by[ns] = m
	}
	g.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// holdsMachine reports whether a Sandbox in phase p has a place in the
// count of its tenant.
func holdsMachine(p setecv1alpha1.SandboxPhase) bool {
	switch p {
	case setecv1alpha1.SandboxPhaseCompleted, setecv1alpha1.SandboxPhaseFailed,
		setecv1alpha1.SandboxPhaseSuspended:
		return false
	case setecv1alpha1.SandboxPhasePending, setecv1alpha1.SandboxPhaseRunning,
		setecv1alpha1.SandboxPhasePaused, setecv1alpha1.SandboxPhaseSnapshotting,
		setecv1alpha1.SandboxPhaseRestoring:
		return true
	}
	// A Sandbox with no phase yet was just created: its machine comes.
	return true
}

// admitLaunch takes the launch lock of ns and checks that the tenant has
// room for n more Sandboxes. The caller creates them and then calls the
// returned function. With no limit it takes no lock and reads nothing.
func (s *Service) admitLaunch(ctx context.Context, ns string, n int) (func(), error) {
	limit := s.MaxSandboxesPerTenant
	if limit <= 0 {
		return func() {}, nil
	}
	unlock := s.gate.lock(ns)
	list := &setecv1alpha1.SandboxList{}
	if err := s.Client.List(ctx, list, client.InNamespace(ns)); err != nil {
		unlock()
		return nil, status.Errorf(grpcCodeFor(err), "count the Sandboxes of the tenant: %v", err)
	}
	live := 0
	for i := range list.Items {
		sb := &list.Items[i]
		if sb.DeletionTimestamp.IsZero() && holdsMachine(sb.Status.Phase) {
			live++
		}
	}
	if live+n > limit {
		unlock()
		return nil, status.Errorf(codes.ResourceExhausted,
			"the tenant runs %d Sandboxes and the limit is %d; end one and try again", live, limit)
	}
	return unlock, nil
}
