// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package snapshot

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NodeAgentComponentLabel is the label the node-agent DaemonSet stamps
// on every Pod it owns (charts/setec/templates/daemonset.yaml).
// PodResolver uses it to find the node-agent Pod running on a given
// node.
//
// There is no per-Pod DNS record to resolve instead (setec#92). A
// headless Service only gives a Pod a name of its own when the Pod
// sets hostname and subdomain, and the node-agent DaemonSet sets
// neither. So a name such as
// "<node>.<fullname>-node-agent.<namespace>.svc" has never resolved
// anywhere. The operator finds the Pod through the API instead and
// dials its Pod IP directly.
const NodeAgentComponentLabel = "app.kubernetes.io/component"

// nodeAgentComponentValue is the value NodeAgentComponentLabel carries
// on a node-agent Pod.
const nodeAgentComponentValue = "node-agent"

// NodeAgentPodResolver locates the node-agent Pod currently serving a
// given node. GRPCDialer depends on this interface, rather than on
// PodResolver directly, so it can be unit-tested against a fake.
type NodeAgentPodResolver interface {
	// ResolveNodeAgentPod returns the Running and Ready node-agent Pod
	// scheduled onto nodeName. It returns an error if no such Pod
	// exists, including a Pod that exists but is not yet Ready. The
	// caller must treat that the same as "not found" rather than dial
	// a Pod that cannot yet serve traffic.
	ResolveNodeAgentPod(ctx context.Context, nodeName string) (*corev1.Pod, error)
}

// PodResolver is the production NodeAgentPodResolver. It lists the
// node-agent DaemonSet's Pods through a controller-runtime client. The
// operator's manager already caches Pods cluster-wide (see
// charts/setec/templates/clusterrole.yaml), so this is a local cache
// read, never a live API call. It picks the Pod on the requested node.
type PodResolver struct {
	// Client reads Pods. In production this is the manager's cached
	// client (mgr.GetClient()).
	Client client.Reader

	// Namespace is the namespace the node-agent DaemonSet runs in.
	Namespace string
}

// ResolveNodeAgentPod implements NodeAgentPodResolver.
func (r *PodResolver) ResolveNodeAgentPod(ctx context.Context, nodeName string) (*corev1.Pod, error) {
	if nodeName == "" {
		return nil, errors.New("podresolver: nodeName is required")
	}
	if r.Client == nil {
		return nil, errors.New("podresolver: Client is required")
	}

	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods,
		client.InNamespace(r.Namespace),
		client.MatchingLabels{NodeAgentComponentLabel: nodeAgentComponentValue},
	); err != nil {
		return nil, fmt.Errorf("podresolver: list node-agent pods in %q: %w", r.Namespace, err)
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != nodeName {
			continue
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		if !podRunningAndReady(pod) {
			continue
		}
		return pod, nil
	}
	return nil, fmt.Errorf(
		"podresolver: no Running and Ready node-agent pod found on node %q in namespace %q",
		nodeName, r.Namespace)
}

// podRunningAndReady reports whether pod is in Phase Running with its
// Ready condition True. A Pod still starting, terminating, or without
// a Ready condition yet is not a dial target.
func podRunningAndReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}
