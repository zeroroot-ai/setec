// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/podspec"
)

// The labels and annotations of a warm pool base (setec#103). A base is a
// full Snapshot of a launcher machine that booted the image of a class
// and ran no workload. It holds no tenant data and serves every tenant.
const (
	// BaseLabel marks a base Snapshot and the launcher Pod that builds it.
	BaseLabel = podspec.BaseLabel
	// BaseLabelValue is the value of BaseLabel, and of
	// CleanBaseAnnotation on a base whose scan found nothing.
	BaseLabelValue = "true"
	// BaseClassLabel names the SandboxClass of a base.
	BaseClassLabel = "setec.zeroroot.ai/base-class"
	// BaseKeyAnnotation holds the hash of every input of a base: the
	// image digest, the launcher image (with the guest kernel), the CPU
	// template and the machine size. A base whose key differs from the
	// key of its class is stale and is never loaded.
	BaseKeyAnnotation = "setec.zeroroot.ai/base-key"
	// CleanBaseAnnotation records that the secret scan of the node agent
	// found nothing in the base.
	CleanBaseAnnotation = "setec.zeroroot.ai/clean-base"
)

// ErrBaseNotClean is returned when the node agent did not confirm a clean
// secret scan of a base.
var ErrBaseNotClean = errors.New("coordinator: the base has no clean secret scan")

// isCleanBase reports whether snap is a base that the operator built in
// the pool namespace and that passed the secret scan. Such a base holds no
// tenant data, so a Sandbox of any tenant may load it.
func (c *Coordinator) isCleanBase(snap *setecv1alpha1.Snapshot) bool {
	return c.PoolNamespace != "" && snap.Namespace == c.PoolNamespace &&
		snap.Labels[BaseLabel] == BaseLabelValue && snap.Annotations[CleanBaseAnnotation] == BaseLabelValue &&
		snap.Annotations[BaseKeyAnnotation] != ""
}

// CreateBase takes the base snapshot of the machine of a base Pod. The
// node agent scans the files for secrets before it stores them, and a base
// with no clean scan is Failed. The Snapshot has the name of the Pod.
func (c *Coordinator) CreateBase(
	ctx context.Context, pod *corev1.Pod, cls *setecv1alpha1.SandboxClass, image, key string,
) error {
	ctx, span := c.startSpan(ctx, "snapshot.CreateBase")
	defer span.End()
	if pod.Spec.NodeName == "" {
		return fmt.Errorf("coordinator: base Pod %q is not scheduled", pod.Name)
	}
	snap := &setecv1alpha1.Snapshot{
		Namespace: pod.Namespace,
		Name:      pod.Name,
		Labels:    map[string]string{BaseLabel: BaseLabelValue, BaseClassLabel: cls.Name},
		Annotations: map[string]string{
			BaseKeyAnnotation: key,
			setecv1alpha1.SnapshotSourcePodUIDAnnotation: string(pod.UID),
		},
		Spec: setecv1alpha1.SnapshotSpec{
			SandboxClass:   cls.Name,
			ImageRef:       image,
			CPUTemplate:    cls.Spec.CPUTemplate,
			InstanceType:   c.instanceType(ctx, pod.Spec.NodeName),
			StorageBackend: c.backendName(),
			Node:           pod.Spec.NodeName,
		},
	}
	if err := c.Client.Create(ctx, snap); err != nil {
		return fmt.Errorf("coordinator: create base Snapshot: %w", err)
	}
	if err := c.markPhase(ctx, snap, setecv1alpha1.SnapshotPhaseCreating, "SnapshotWriteInFlight"); err != nil {
		return err
	}
	na, err := c.Dialer.Dial(ctx, pod.Spec.NodeName)
	if err != nil {
		c.failSnapshot(ctx, snap, "NodeAgentUnreachable", err)
		return fmt.Errorf("coordinator: dial node-agent: %w", err)
	}
	resp, err := na.CreateSnapshot(ctx, &setecgrpcv1.CreateSnapshotRequest{
		SandboxId:      pod.Namespace + "/" + pod.Name,
		SnapshotId:     pod.Namespace + "-" + pod.Name,
		StorageBackend: c.backendName(),
		SourcePodUid:   string(pod.UID),
		ScanForSecrets: true,
	})
	if err != nil {
		c.failSnapshot(ctx, snap, EventReasonSnapshotCreateFailed, err)
		return fmt.Errorf("coordinator: base CreateSnapshot RPC: %w", err)
	}
	if !resp.GetCleanBaseVerified() {
		c.failSnapshot(ctx, snap, "BaseNotClean", ErrBaseNotClean)
		return ErrBaseNotClean
	}
	original := snap.DeepCopy()
	snap.Spec.StorageRef = resp.GetStorageRef()
	snap.Spec.Size = resp.GetSizeBytes()
	snap.Spec.SHA256 = resp.GetSha256()
	snap.Annotations[CleanBaseAnnotation] = BaseLabelValue
	if err := c.Client.Patch(ctx, snap, client.MergeFrom(original)); err != nil {
		c.failSnapshot(ctx, snap, "StorageRefNotRecorded", err)
		return fmt.Errorf("coordinator: record the base: %w", err)
	}
	return c.markPhase(ctx, snap, setecv1alpha1.SnapshotPhaseReady, "")
}

// BaseKey is the hash of every input of a base. Two bases with one key are
// the same machine at the same point, and a change of any input makes a
// new key, so a stale base is never loaded.
func BaseKey(image, launcherImage, cpuTemplate string, res setecv1alpha1.Resources) string {
	h := sha256.New()
	for _, part := range []string{image, launcherImage, cpuTemplate, strconv.Itoa(int(res.VCPU)), res.Memory.String()} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:20]
}
