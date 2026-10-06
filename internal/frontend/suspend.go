// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// Suspend asks the operator to checkpoint a session and release its
// microVM (setec#193). The operator does the work: the frontend sets the
// desired state.
func (s *Service) Suspend(ctx context.Context, req *setecv1grpc.SuspendRequest) (*setecv1grpc.SuspendResponse, error) {
	if err := s.setDesiredState(ctx, req.GetSandboxId(), req.GetTenant(), setecv1alpha1.SandboxDesiredStateSuspended); err != nil {
		return nil, err
	}
	return &setecv1grpc.SuspendResponse{}, nil
}

// Resume asks the operator to bring a suspended session back.
func (s *Service) Resume(ctx context.Context, req *setecv1grpc.ResumeRequest) (*setecv1grpc.ResumeResponse, error) {
	if err := s.setDesiredState(ctx, req.GetSandboxId(), req.GetTenant(), setecv1alpha1.SandboxDesiredStateRunning); err != nil {
		return nil, err
	}
	return &setecv1grpc.ResumeResponse{}, nil
}

// setDesiredState checks the scope of the caller and that the Sandbox is a
// session of a class with checkpoints, then writes spec.desiredState.
func (s *Service) setDesiredState(ctx context.Context, sandboxID, tenant string, want setecv1alpha1.SandboxDesiredState) error {
	ns, name, err := parseSandboxID(sandboxID)
	if err != nil {
		return err
	}
	if err := s.checkTenantNamespace(ctx, tenant, ns); err != nil {
		return err
	}
	sb := &setecv1alpha1.Sandbox{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, sb); err != nil {
		return status.Errorf(grpcCodeFor(err), "get Sandbox: %v", err)
	}
	if !sb.Spec.IsSession() {
		return status.Error(codes.FailedPrecondition, "only a session suspends and resumes")
	}
	if want == setecv1alpha1.SandboxDesiredStateSuspended {
		cls := &setecv1alpha1.SandboxClass{}
		if sb.Spec.SandboxClassName == "" ||
			s.Client.Get(ctx, types.NamespacedName{Name: sb.Spec.SandboxClassName}, cls) != nil ||
			cls.Spec.SessionCheckpoint == nil {
			return status.Error(codes.FailedPrecondition,
				"the class of this session has no sessionCheckpoint, so it cannot suspend")
		}
	}
	if sb.Spec.DesiredState == want {
		return nil
	}
	original := sb.DeepCopy()
	sb.Spec.DesiredState = want
	if err := s.Client.Patch(ctx, sb, client.MergeFrom(original)); err != nil {
		return status.Errorf(grpcCodeFor(err), "set the desired state: %v", err)
	}
	return nil
}
