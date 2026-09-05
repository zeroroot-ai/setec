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

package frontend

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// activityHeartbeatInterval is how often the frontend refreshes a
// session Sandbox's last-activity annotation while a client stream is
// open. One minute keeps the write rate negligible while staying far
// inside any sane SandboxClass sessionIdleTimeout, so an attached
// client can never be idle-evicted mid-stream.
const activityHeartbeatInterval = time.Minute

// finalActivityStampTimeout bounds the best-effort last-activity write
// performed after a client stream ends. The stream context is already
// canceled at that point, so the stamp runs on its own deadline.
const finalActivityStampTimeout = 5 * time.Second

// Attach resolves a session handle to its live session Sandbox
// (ADR-0006 reattach-by-handle). Resolution is stateless: the handle is
// parsed and checked against cluster state alone — the frontend keeps
// no session table — so a caller reattaches identically whether it lost
// its connection, moved to another frontend replica, or the frontend
// restarted in between. On success the caller continues with the
// streaming RPCs (StreamLogs, Wait) using the same handle.
//
// Attach registers caller activity: it stamps the Sandbox's
// last-activity annotation, which the operator's idle-eviction policy
// consults, so attaching (and streaming, which heartbeats the same
// annotation) exempts the session from idle reaping.
func (s *Service) Attach(ctx context.Context, req *setecv1grpc.AttachRequest) (*setecv1grpc.AttachResponse, error) {
	ns, name, uid, err := parseSessionHandle(req.GetSandboxId())
	if err != nil {
		return nil, err
	}
	if err := s.checkTenantNamespace(ctx, ns); err != nil {
		return nil, err
	}

	// Handle validation is shared with Exec (see exec.go) so both verbs
	// accept and refuse exactly the same handles for the same reasons.
	sb, err := s.resolveLiveSession(ctx, ns, name, uid, req.GetSandboxId())
	if err != nil {
		return nil, err
	}

	// Registering activity is part of attaching — a session with an
	// attached caller must never be idle-evicted — so a failed stamp
	// fails the Attach loudly instead of handing out a handle the idle
	// reaper might pull out from under the caller.
	if err := s.touchSessionActivity(ctx, ns, name, time.Now()); err != nil {
		return nil, status.Errorf(grpcCodeFor(err), "record session activity: %v", err)
	}

	resp := &setecv1grpc.AttachResponse{
		SandboxId: req.GetSandboxId(),
		Name:      sb.Name,
		Namespace: sb.Namespace,
		Phase:     string(sb.Status.Phase),
		// A reattaching caller never saw the LaunchResponse, so the
		// class and the selected backend are reported here too.
		SandboxClass: sb.Spec.SandboxClassName,
	}
	if rt := sb.Status.Runtime; rt != nil {
		resp.Runtime = rt.Chosen
	}
	return resp, nil
}

// parseSessionHandle splits a session handle of the form
// <namespace>/<name>/<uid> and requires all three components. Unlike
// parseSandboxID, the UID is mandatory here: it is what makes a handle
// name a session rather than merely a resource.
func parseSessionHandle(id string) (ns, name, uid string, err error) {
	parts := strings.Split(id, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", status.Errorf(codes.InvalidArgument,
			"sandbox_id %q must be <namespace>/<name>/<uid>", id)
	}
	return parts[0], parts[1], parts[2], nil
}

// attachFailure builds a gRPC status carrying an AttachFailure detail
// so callers can switch on the typed reason. If detail attachment ever
// fails the plain status is returned — the code and message still
// describe the failure.
func attachFailure(
	code codes.Code,
	reason setecv1grpc.AttachFailure_Reason,
	phase string,
	format string,
	args ...any,
) error {
	st := status.Newf(code, format, args...)
	if detailed, err := st.WithDetails(&setecv1grpc.AttachFailure{
		Reason: reason,
		Phase:  phase,
	}); err == nil {
		st = detailed
	}
	return st.Err()
}

// touchSessionActivity stamps the Sandbox's last-activity annotation
// with the given instant (RFC 3339, UTC). It uses a metadata-only merge
// patch so it can never conflict with the operator's status writes.
func (s *Service) touchSessionActivity(ctx context.Context, ns, name string, t time.Time) error {
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{
				setecv1alpha1.AnnotationLastActivity: t.UTC().Format(time.RFC3339),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("marshal activity patch: %w", err)
	}
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
	return s.Client.Patch(ctx, sb, client.RawPatch(types.MergePatchType, body))
}

// keepSessionActive marks the session active for the duration of a
// client stream: an immediate stamp, a heartbeat every
// activityHeartbeatInterval while the stream lives, and a final stamp
// when it ends so the idle clock starts at the disconnect, not at the
// last heartbeat. The returned stop function must be called (deferred)
// when the stream ends; it is idempotent in effect and safe on every
// exit path. All writes are best-effort — a transient patch failure
// must not tear down a healthy log stream — except that the very first
// stamp's error is returned for callers that want to fail loudly.
func (s *Service) keepSessionActive(ctx context.Context, ns, name string) (stop func()) {
	_ = s.touchSessionActivity(ctx, ns, name, time.Now())

	hbCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(activityHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				_ = s.touchSessionActivity(hbCtx, ns, name, time.Now())
			}
		}
	}()

	return func() {
		cancel()
		<-done
		// The stream context is canceled once the client is gone, so
		// the disconnect stamp gets its own short-lived context.
		fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), finalActivityStampTimeout)
		defer fcancel()
		_ = s.touchSessionActivity(fctx, ns, name, time.Now())
	}
}
