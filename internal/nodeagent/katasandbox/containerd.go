// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package katasandbox

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// CRI plugin labels on a sandbox container.
const (
	labelPodUID = "io.kubernetes.pod.uid"
	labelKind   = "io.cri-containerd.kind"
	kindSandbox = "sandbox"
)

// ContainerdLookup finds a Pod's CRI sandbox id in containerd.
type ContainerdLookup struct {
	Client *client.Client
	// Namespace is the containerd namespace the kubelet uses, k8s.io.
	Namespace string
}

// SandboxID returns the id of the sandbox container that containerd's
// CRI plugin created for the Pod with the given UID.
func (c ContainerdLookup) SandboxID(ctx context.Context, podUID string) (string, error) {
	if !podUIDPattern.MatchString(podUID) {
		return "", fmt.Errorf("katasandbox: %q is not a pod UID", podUID)
	}
	ctx = namespaces.WithNamespace(ctx, c.Namespace)
	// One filter string: its comma-separated terms are ANDed. Separate
	// strings would be ORed.
	filter := fmt.Sprintf(`labels.%q==%s,labels.%q==%s`, labelPodUID, podUID, labelKind, kindSandbox)
	containers, err := c.Client.Containers(ctx, filter)
	if err != nil {
		return "", fmt.Errorf("katasandbox: list containerd sandboxes: %w", err)
	}
	switch len(containers) {
	case 0:
		return "", fmt.Errorf("%w: pod %s has no sandbox container in namespace %s", ErrNotFound, podUID, c.Namespace)
	case 1:
		return containers[0].ID(), nil
	default:
		ids := make([]string, 0, len(containers))
		for _, ct := range containers {
			ids = append(ids, ct.ID())
		}
		return "", fmt.Errorf("katasandbox: pod %s has %d sandbox containers: %v", podUID, len(containers), ids)
	}
}
