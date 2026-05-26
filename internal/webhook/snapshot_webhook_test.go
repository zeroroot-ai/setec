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

package webhook

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

func newSchemeForSnapshot(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(setecv1alpha1.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
	return s
}

func newFakeClientSnapshot(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newSchemeForSnapshot(t)).WithObjects(objs...).Build()
}

func mkSnapshotCR(ns, name string, ttl *time.Duration) *setecv1alpha1.Snapshot {
	snap := &setecv1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: setecv1alpha1.SnapshotSpec{
			SandboxClass:   "standard",
			ImageRef:       "img:v1",
			VMM:            setecv1alpha1.VMMFirecracker,
			StorageBackend: "local-disk",
			StorageRef:     name,
			Node:           "node-a",
		},
	}
	if ttl != nil {
		snap.Spec.TTL = &metav1.Duration{Duration: *ttl}
	}
	return snap
}

func TestSnapshotValidator_TTLMinimum(t *testing.T) {
	t.Parallel()
	c := newFakeClientSnapshot(t)
	v := &SnapshotValidator{Client: c}

	short := 30 * time.Second
	_, err := v.ValidateCreate(context.Background(), mkSnapshotCR("team-a", "s", &short))
	if err == nil || !strings.Contains(err.Error(), "below the minimum") {
		t.Fatalf("expected TTL-minimum error, got %v", err)
	}
}

func TestSnapshotValidator_TTLAccepted(t *testing.T) {
	t.Parallel()
	c := newFakeClientSnapshot(t)
	v := &SnapshotValidator{Client: c}
	long := 2 * time.Hour
	_, err := v.ValidateCreate(context.Background(), mkSnapshotCR("team-a", "s", &long))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSnapshotValidator_NoTTL(t *testing.T) {
	t.Parallel()
	c := newFakeClientSnapshot(t)
	v := &SnapshotValidator{Client: c}
	if _, err := v.ValidateCreate(context.Background(), mkSnapshotCR("team-a", "s", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSnapshotValidator_DuplicateName(t *testing.T) {
	t.Parallel()
	existing := mkSnapshotCR("team-a", "dup", nil)
	c := newFakeClientSnapshot(t, existing)
	v := &SnapshotValidator{Client: c}
	_, err := v.ValidateCreate(context.Background(), mkSnapshotCR("team-a", "dup", nil))
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected duplicate-name error, got %v", err)
	}
}

func TestSnapshotValidator_QuotaEnforced(t *testing.T) {
	t.Parallel()

	// One existing snapshot; quota allows exactly 1.
	existing := mkSnapshotCR("team-a", "s-existing", nil)
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "q"},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				SnapshotResourceName: resource.MustParse("1"),
			},
		},
	}
	c := newFakeClientSnapshot(t, existing, quota)
	v := &SnapshotValidator{Client: c}
	_, err := v.ValidateCreate(context.Background(), mkSnapshotCR("team-a", "s-new", nil))
	if err == nil || !strings.Contains(err.Error(), "ResourceQuota") {
		t.Fatalf("expected quota error, got %v", err)
	}
}

func TestSnapshotValidator_QuotaHeadroom(t *testing.T) {
	t.Parallel()
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "q"},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				SnapshotResourceName: resource.MustParse("5"),
			},
		},
	}
	c := newFakeClientSnapshot(t, quota)
	v := &SnapshotValidator{Client: c}
	if _, err := v.ValidateCreate(context.Background(), mkSnapshotCR("team-a", "s", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSnapshotValidator_UpdateRunsTTLOnly(t *testing.T) {
	t.Parallel()
	c := newFakeClientSnapshot(t)
	v := &SnapshotValidator{Client: c}

	// A duplicate name would normally reject on create, but an update
	// path shouldn't consult the duplicate check because the object is
	// by definition the same object being updated.
	old := mkSnapshotCR("team-a", "s", nil)
	short := 10 * time.Second
	newSnap := mkSnapshotCR("team-a", "s", &short)
	_, err := v.ValidateUpdate(context.Background(), old, newSnap)
	if err == nil || !strings.Contains(err.Error(), "below the minimum") {
		t.Fatalf("expected TTL error on update, got %v", err)
	}
}

func TestSnapshotValidator_Delete(t *testing.T) {
	t.Parallel()
	v := &SnapshotValidator{Client: newFakeClientSnapshot(t)}
	if _, err := v.ValidateDelete(context.Background(), mkSnapshotCR("ns", "s", nil)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}
