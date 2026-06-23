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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/leasepool"
)

// crBackend implements leasepool.Backend against the controller-runtime
// client by creating, polling, and deleting Sandbox CRs. A warm pool
// entry is a Sandbox running the class's pre-warm image with a long-lived
// idle command, optionally restored from a Snapshot (the warm-start
// mechanism). It stays Running until leased; on Release the CR is deleted
// (destroy-on-release).
type crBackend struct {
	client client.Client
	// idleCommand is what a warm entry runs while waiting to be leased.
	// A warm sandbox must stay Running (not exit) so it can be claimed,
	// hence a long sleep rather than a one-shot command.
	idleCommand []string
}

// newCRBackend builds a CR-backed pool Backend.
func newCRBackend(c client.Client) *crBackend {
	return &crBackend{
		client:      c,
		idleCommand: []string{"/bin/sh", "-c", "sleep infinity"},
	}
}

// Launch creates a warm Sandbox for the template and returns its ref.
func (b *crBackend) Launch(ctx context.Context, tmpl leasepool.PoolTemplate) (leasepool.SandboxRef, error) {
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "warm-",
			Namespace:    tmpl.Namespace,
			Labels: map[string]string{
				leaseClassLabel: tmpl.SandboxClass,
				leasePoolLabel:  "true",
			},
		},
		Spec: setecv1alpha1.SandboxSpec{
			SandboxClassName: tmpl.SandboxClass,
			Image:            tmpl.Image,
			Command:          appendStrings(tmpl.Command, b.idleCommand),
		},
	}
	if tmpl.SnapshotName != "" {
		sb.Spec.SnapshotRef = &setecv1alpha1.SandboxSnapshotRef{Name: tmpl.SnapshotName}
	}
	if err := b.client.Create(ctx, sb); err != nil {
		return leasepool.SandboxRef{}, fmt.Errorf("create warm Sandbox: %w", err)
	}
	return leasepool.SandboxRef{
		ID:        fmt.Sprintf("%s/%s/%s", sb.Namespace, sb.Name, string(sb.UID)),
		Name:      sb.Name,
		Namespace: sb.Namespace,
	}, nil
}

// Ready reports whether the Sandbox is Running (ready to lease) and
// whether it is still alive (not terminal). A NotFound is treated as a
// dead entry so the pool prunes it.
func (b *crBackend) Ready(ctx context.Context, ref leasepool.SandboxRef) (ready, alive bool, err error) {
	sb := &setecv1alpha1.Sandbox{}
	getErr := b.client.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, sb)
	if getErr != nil {
		if apiIsNotFound(getErr) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("get Sandbox: %w", getErr)
	}
	switch sb.Status.Phase {
	case setecv1alpha1.SandboxPhaseRunning:
		return true, true, nil
	case setecv1alpha1.SandboxPhaseCompleted, setecv1alpha1.SandboxPhaseFailed:
		return false, false, nil
	default:
		// Pending / Restoring / Snapshotting / Paused: still warming, alive.
		return false, true, nil
	}
}

// Destroy deletes the Sandbox CR. NotFound is a success (already gone).
func (b *crBackend) Destroy(ctx context.Context, ref leasepool.SandboxRef) error {
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name},
	}
	if err := b.client.Delete(ctx, sb); err != nil && !apiIsNotFound(err) {
		return fmt.Errorf("delete Sandbox: %w", err)
	}
	return nil
}

const (
	// leaseClassLabel records the SandboxClass a warm pool entry belongs
	// to so operators can observe pool membership with kubectl.
	leaseClassLabel = "setec.zeroroot.ai/lease-class"
	// leasePoolLabel marks a Sandbox as a lease-pool warm entry.
	leasePoolLabel = "setec.zeroroot.ai/lease-pool"
)

// appendStrings returns a if non-empty, else fallback. Used so an
// explicit warm command on the template wins over the backend default.
func appendStrings(a, fallback []string) []string {
	if len(a) > 0 {
		return append([]string(nil), a...)
	}
	return append([]string(nil), fallback...)
}
