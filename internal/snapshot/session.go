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

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	corev1 "k8s.io/api/core/v1"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// Session-checkpoint event reasons (setec#194).
const (
	EventReasonCheckpointCreated       = "SessionCheckpointCreated"
	EventReasonCheckpointCreateFailed  = "SessionCheckpointCreateFailed"
	EventReasonCheckpointRestored      = "SessionCheckpointRestored"
	EventReasonCheckpointRestoreFailed = "SessionCheckpointRestoreFailed"
	EventReasonCheckpointDeleteFailed  = "SessionCheckpointDeleteFailed"
)

// SessionCheckpointID renders the storage snapshot id for one
// checkpoint of a session Sandbox. The sequence namespaces successive
// checkpoints so a new one never collides with the one it replaces.
func SessionCheckpointID(sb *setecv1alpha1.Sandbox, sequence int64) string {
	return fmt.Sprintf("%s-%s-ckpt-%d", sb.Namespace, sb.Name, sequence)
}

// CheckpointSession pauses the session VM just long enough for
// Firecracker to write its state+memory pair, streams it — encrypted
// under a fresh DEK sealed with the forwarded per-session KEK — into
// the portable checkpoint backend on the Pod's node, and resumes the
// VM (the node-agent resumes the source unconditionally after the
// state files land). Returns the storage ref and stored size.
func (c *Coordinator) CheckpointSession(
	ctx context.Context,
	sb *setecv1alpha1.Sandbox,
	backendName string,
	sequence int64,
	sessionKEK []byte,
) (string, int64, error) {
	ctx, span := c.startSpan(ctx, "snapshot.CheckpointSession")
	defer span.End()
	span.SetAttributes(
		attribute.String("setec.sandbox", sb.Namespace+"/"+sb.Name),
		attribute.Int64("setec.checkpoint.sequence", sequence),
	)
	start := time.Now()

	pod, err := c.getPod(ctx, sb)
	if err != nil {
		setSpanErr(span, err.Error())
		return "", 0, err
	}
	if pod.Spec.NodeName == "" {
		setSpanErr(span, "pod not scheduled")
		return "", 0, fmt.Errorf("coordinator: Pod %q has no NodeName; cannot checkpoint", pod.Name)
	}
	na, dialErr := c.Dialer.Dial(ctx, pod.Spec.NodeName)
	if dialErr != nil {
		c.emit(sb, corev1.EventTypeWarning, EventReasonNodeAgentUnreachable, dialErr.Error())
		setSpanErr(span, dialErr.Error())
		return "", 0, fmt.Errorf("coordinator: dial node-agent: %w", dialErr)
	}

	resp, rpcErr := na.CreateSnapshot(ctx, &setecgrpcv1.CreateSnapshotRequest{
		SandboxId:        sb.Namespace + "/" + sb.Name,
		SnapshotId:       SessionCheckpointID(sb, sequence),
		StorageBackend:   backendName,
		SourceKataSocket: c.socketForPod(pod),
		SessionKek:       sessionKEK,
	})
	if rpcErr != nil {
		c.emit(sb, corev1.EventTypeWarning, EventReasonCheckpointCreateFailed, rpcErr.Error())
		setSpanErr(span, rpcErr.Error())
		c.recordDuration("checkpoint", sb, time.Since(start))
		return "", 0, fmt.Errorf("coordinator: CreateSnapshot (checkpoint) RPC: %w", rpcErr)
	}
	c.emit(sb, corev1.EventTypeNormal, EventReasonCheckpointCreated,
		fmt.Sprintf("session checkpoint #%d persisted to %q (%d bytes)", sequence, backendName, resp.GetSizeBytes()))
	c.recordDuration("checkpoint", sb, time.Since(start))
	return resp.GetStorageRef(), resp.GetSizeBytes(), nil
}

// RestoreSessionCheckpoint loads a session checkpoint into the
// session's CURRENT Pod — on whatever node the scheduler placed it,
// which is the point: the checkpoint store and the per-session KEK
// are both cluster-scoped, so no node pinning applies (unlike the
// local-disk Snapshot restore path). The node-agent's restore
// invariants (entropy reseed, restore uniquification) apply
// unchanged.
func (c *Coordinator) RestoreSessionCheckpoint(
	ctx context.Context,
	sb *setecv1alpha1.Sandbox,
	ref string,
	backendName string,
	sessionKEK []byte,
) error {
	ctx, span := c.startSpan(ctx, "snapshot.RestoreSessionCheckpoint")
	defer span.End()
	span.SetAttributes(attribute.String("setec.sandbox", sb.Namespace+"/"+sb.Name))
	start := time.Now()

	pod, err := c.getPod(ctx, sb)
	if err != nil {
		setSpanErr(span, err.Error())
		return err
	}
	if pod.Spec.NodeName == "" {
		setSpanErr(span, "pod not scheduled")
		return fmt.Errorf("coordinator: Pod %q has no NodeName; restore requires a scheduled pod", pod.Name)
	}
	na, dialErr := c.Dialer.Dial(ctx, pod.Spec.NodeName)
	if dialErr != nil {
		c.emit(sb, corev1.EventTypeWarning, EventReasonNodeAgentUnreachable, dialErr.Error())
		setSpanErr(span, dialErr.Error())
		return fmt.Errorf("coordinator: dial node-agent: %w", dialErr)
	}

	// SandboxId/PodIp/Hostname feed the node-agent's per-restore
	// uniquification (ADR-0005 invariant 2, setec#189): a session
	// resume is a restore like any other, so the resumed guest gets a
	// fresh machine identity, reconciles to its new Pod IP, and takes
	// a node-unique vsock CID — fail-closed like the E10 path.
	resp, rpcErr := na.RestoreSandbox(ctx, &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId:       ref,
		StorageRef:       ref,
		StorageBackend:   backendName,
		KataSocketTarget: c.socketForPod(pod),
		SessionKek:       sessionKEK,
		SandboxId:        sb.Namespace + "/" + sb.Name,
		PodIp:            pod.Status.PodIP,
		Hostname:         sb.Name,
	})
	if rpcErr != nil || (resp != nil && !resp.Success) {
		msg := errString(rpcErr, resp)
		c.emit(sb, corev1.EventTypeWarning, EventReasonCheckpointRestoreFailed, msg)
		setSpanErr(span, msg)
		c.recordDuration("checkpoint_restore", sb, time.Since(start))
		return fmt.Errorf("coordinator: RestoreSandbox (checkpoint) RPC: %s", msg)
	}

	c.emit(sb, corev1.EventTypeNormal, EventReasonCheckpointRestored,
		fmt.Sprintf("session resumed from checkpoint on node %q", pod.Spec.NodeName))
	if resp.GetEntropyReseeded() {
		c.emit(sb, corev1.EventTypeNormal, EventReasonEntropyReseeded,
			"restored session guest CSPRNG reseeded with fresh entropy")
	}
	if resp.GetUniquified() {
		c.emit(sb, corev1.EventTypeNormal, EventReasonSandboxUniquified,
			"resumed session guest identity uniquified: fresh machine-id/boot-id/hostname, Pod IP verified, vsock CID unique")
	}
	c.recordDuration("checkpoint_restore", sb, time.Since(start))
	return nil
}

// DeleteSessionCheckpoint asks a node-agent to remove a checkpoint's
// objects from the portable store. Any node can perform the delete —
// the store is cluster-scoped — so the routing prefers the session
// Pod's node and falls back to any node advertising a setec runtime.
// Note the ciphertext delete is belt-and-braces: the checkpoint is
// already cryptographically erased the moment the per-session KEK
// Secret is deleted.
func (c *Coordinator) DeleteSessionCheckpoint(
	ctx context.Context,
	sb *setecv1alpha1.Sandbox,
	ref string,
	backendName string,
) error {
	ctx, span := c.startSpan(ctx, "snapshot.DeleteSessionCheckpoint")
	defer span.End()
	span.SetAttributes(attribute.String("setec.sandbox", sb.Namespace+"/"+sb.Name))

	na, err := c.dialSessionAgent(ctx, sb)
	if err != nil {
		setSpanErr(span, err.Error())
		return err
	}
	resp, rpcErr := na.DeleteSnapshot(ctx, &setecgrpcv1.DeleteSnapshotRequest{
		SnapshotId:     ref,
		StorageRef:     ref,
		StorageBackend: backendName,
	})
	if rpcErr != nil || (resp != nil && !resp.Success) {
		msg := errString(rpcErr, resp)
		c.emit(sb, corev1.EventTypeWarning, EventReasonCheckpointDeleteFailed, msg)
		setSpanErr(span, msg)
		return fmt.Errorf("coordinator: DeleteSnapshot (checkpoint) RPC: %s", msg)
	}
	return nil
}

// dialSessionAgent resolves a node-agent that can reach the portable
// checkpoint store: the session Pod's node when the Pod exists and is
// scheduled, otherwise any node advertising a setec runtime label
// (every such node runs the node-agent DaemonSet).
func (c *Coordinator) dialSessionAgent(ctx context.Context, sb *setecv1alpha1.Sandbox) (NodeAgentClient, error) {
	if pod, err := c.getPod(ctx, sb); err == nil && pod.Spec.NodeName != "" {
		if na, dialErr := c.Dialer.Dial(ctx, pod.Spec.NodeName); dialErr == nil {
			return na, nil
		}
	}
	nodeList := &corev1.NodeList{}
	if err := c.Client.List(ctx, nodeList); err != nil {
		return nil, fmt.Errorf("coordinator: list nodes for checkpoint routing: %w", err)
	}
	var errs []error
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		if !nodeAdvertisesSetecRuntime(node) || !nodeIsReady(node) {
			continue
		}
		na, dialErr := c.Dialer.Dial(ctx, node.Name)
		if dialErr == nil {
			return na, nil
		}
		errs = append(errs, fmt.Errorf("node %q: %w", node.Name, dialErr))
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("coordinator: no reachable node-agent for checkpoint routing: %w", errors.Join(errs...))
	}
	return nil, errors.New("coordinator: no setec-runtime node available for checkpoint routing")
}

// nodeAdvertisesSetecRuntime reports whether the node carries any
// setec.zeroroot.ai/runtime.<backend>=true capability label — the
// marker the runtime-agent stamps on every node the node-agent
// DaemonSet targets.
func nodeAdvertisesSetecRuntime(node *corev1.Node) bool {
	for k, v := range node.Labels {
		if v == "true" && len(k) > len("setec.zeroroot.ai/runtime.") &&
			k[:len("setec.zeroroot.ai/runtime.")] == "setec.zeroroot.ai/runtime." {
			return true
		}
	}
	return false
}

// nodeIsReady reports the node's Ready condition.
func nodeIsReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}
