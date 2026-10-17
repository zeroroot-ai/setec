// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/credentials/spiffetest"
)

// A node agent that is down for a short time is not a failed snapshot
// (setec#315). The coordinator removes the Snapshot it made in the attempt
// and returns ErrNodeAgentUnavailable, so the reconciler tries again. A
// Failed Snapshot is terminal, so marking it Failed stopped the snapshot
// until a person deleted it.

// unavailableCases are the errors that mean "the node agent cannot be
// reached now, and it got no call". Each one comes from the dial or from
// the shim, which did not send the call.
func unavailableCases() []struct {
	name    string
	dialErr error
	rpcErr  error
} {
	notSent := &notSentError{state: connectivity.TransientFailure, wait: nodeAgentReadyWait}
	return []struct {
		name    string
		dialErr error
		rpcErr  error
	}{
		{name: "the shim did not send the call", rpcErr: notSent},
		{name: "the shim did not send the call, wrapped", rpcErr: fmt.Errorf("x: %w", notSent)},
		{name: "the resolver finds no Ready node agent Pod", dialErr: fmt.Errorf("grpcdialer: %w", ErrNodeAgentUnavailable)},
	}
}

func TestCreateSnapshot_NodeAgentUnavailableIsRetried(t *testing.T) {
	for _, tc := range unavailableCases() {
		t.Run(tc.name, func(t *testing.T) {
			sb := newSandboxForCoord()
			pod := newPodForSandbox(sb, "node-a")
			c := newFakeClient(t, sb, pod)
			na := &fakeNodeAgentClient{createErr: tc.rpcErr}
			dialer := &fakeDialer{client: na, dialErr: tc.dialErr}
			coord := newCoord(c, dialer)

			err := coord.CreateSnapshot(context.Background(), sb)
			if !errors.Is(err, ErrNodeAgentUnavailable) {
				t.Fatalf("CreateSnapshot: want ErrNodeAgentUnavailable, got %v", err)
			}
			got := &setecv1alpha1.Snapshot{}
			gerr := c.Get(context.Background(), types.NamespacedName{Namespace: "t-a", Name: "snap-1"}, got)
			if !apierrors.IsNotFound(gerr) {
				t.Fatalf("the Snapshot of the attempt must be removed, got err=%v phase=%q", gerr, got.Status.Phase)
			}

			// The next attempt, with the node agent back, takes the snapshot.
			dialer.dialErr = nil
			na.createErr = nil
			na.createResp = &setecgrpcv1.CreateSnapshotResponse{StorageRef: "t-a-snap-1", SizeBytes: 1}
			if err := coord.CreateSnapshot(context.Background(), sb); err != nil {
				t.Fatalf("CreateSnapshot after the node agent is back: %v", err)
			}
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t-a", Name: "snap-1"}, got); err != nil {
				t.Fatalf("get Snapshot: %v", err)
			}
			if got.Status.Phase != setecv1alpha1.SnapshotPhaseReady {
				t.Fatalf("phase = %q, want Ready", got.Status.Phase)
			}
		})
	}
}

// TestCreateSnapshot_OtherErrorsStayFailed keeps setec#129: an error that
// is not a short outage of the node agent still marks the Snapshot Failed.
func TestCreateSnapshot_OtherErrorsStayFailed(t *testing.T) {
	for name, rpcErr := range map[string]error{
		"Internal":         status.Error(codes.Internal, "firecracker pause"),
		"plain":            errors.New("fc: pause failed"),
		"DeadlineExceeded": status.Error(codes.DeadlineExceeded, "slow"),
		// A call that was sent and then lost its connection: the node
		// agent may have paused the machine or stored part of it.
		"sent and then Unavailable": status.Error(codes.Unavailable, "transport is closing"),
	} {
		t.Run(name, func(t *testing.T) {
			sb := newSandboxForCoord()
			c := newFakeClient(t, sb, newPodForSandbox(sb, "node-a"))
			coord := newCoord(c, &fakeDialer{client: &fakeNodeAgentClient{createErr: rpcErr}})

			err := coord.CreateSnapshot(context.Background(), sb)
			if err == nil || errors.Is(err, ErrNodeAgentUnavailable) {
				t.Fatalf("CreateSnapshot: want a terminal error, got %v", err)
			}
			got := &setecv1alpha1.Snapshot{}
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t-a", Name: "snap-1"}, got); err != nil {
				t.Fatalf("the Snapshot must be kept: %v", err)
			}
			if got.Status.Phase != setecv1alpha1.SnapshotPhaseFailed {
				t.Fatalf("phase = %q, want Failed", got.Status.Phase)
			}
		})
	}
}

func TestCreateBase_NodeAgentUnavailableLeavesNoFailedBase(t *testing.T) {
	for _, tc := range unavailableCases() {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{
				Namespace: "pool", Name: "base-1", UID: "base-uid",
				Spec: corev1.PodSpec{NodeName: "node-a"},
			}
			cls := &setecv1alpha1.SandboxClass{Name: "standard"}
			c := newFakeClient(t, pod)
			na := &fakeNodeAgentClient{createErr: tc.rpcErr}
			coord := newCoord(c, &fakeDialer{client: na, dialErr: tc.dialErr})

			err := coord.CreateBase(context.Background(), pod, cls, "img:v1", "key-1")
			if !errors.Is(err, ErrNodeAgentUnavailable) {
				t.Fatalf("CreateBase: want ErrNodeAgentUnavailable, got %v", err)
			}
			got := &setecv1alpha1.Snapshot{}
			gerr := c.Get(context.Background(), types.NamespacedName{Namespace: "pool", Name: "base-1"}, got)
			if !apierrors.IsNotFound(gerr) {
				t.Fatalf("the base of the attempt must be removed, got err=%v phase=%q", gerr, got.Status.Phase)
			}
		})
	}
}

// TestNodeAgentPodMissingIsUnavailable: a node agent Pod that is not Ready
// yet, or has no IP yet, is a short outage. A failure to list Pods is a
// fault of the configuration, and stays a plain error.
func TestNodeAgentPodMissingIsUnavailable(t *testing.T) {
	t.Parallel()
	notReady := nodeAgentPod(testPodName, testNamespace, testWorkerNode, corev1.PodRunning, corev1.ConditionFalse, testPodIP)
	r := &PodResolver{Client: newFakePodClient(t, notReady), Namespace: testNamespace}
	if _, err := r.ResolveNodeAgentPod(t.Context(), testWorkerNode); !errors.Is(err, ErrNodeAgentUnavailable) {
		t.Fatalf("ResolveNodeAgentPod with no Ready Pod: want ErrNodeAgentUnavailable, got %v", err)
	}
	if _, err := (&PodResolver{Namespace: testNamespace}).ResolveNodeAgentPod(t.Context(), testWorkerNode); err == nil ||
		errors.Is(err, ErrNodeAgentUnavailable) {
		t.Fatalf("ResolveNodeAgentPod with no client: want a plain error, got %v", err)
	}

	ca := spiffetest.NewCA(t)
	d := NewGRPCDialer(&fakeResolver{pod: podWithUID("uid", "")}, unusedAuthorityPattern, operatorCredentials(t, ca, ca))
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.Dial(t.Context(), testNodeName); !errors.Is(err, ErrNodeAgentUnavailable) {
		t.Fatalf("Dial to a Pod with no IP: want ErrNodeAgentUnavailable, got %v", err)
	}
}
