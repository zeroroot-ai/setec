// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package v1alpha1 contains API Schema definitions for the setec v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=setec.zeroroot.ai
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// SchemeGroupVersion is group version used to register these objects.
	// This name is used by applyconfiguration generators (e.g. controller-gen).
	SchemeGroupVersion = schema.GroupVersion{Group: "setec.zeroroot.ai", Version: "v1alpha1"}

	// GroupVersion is an alias for SchemeGroupVersion, for backward compatibility.
	GroupVersion = SchemeGroupVersion

	// SchemeBuilder adds the types of this group-version to a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// addKnownTypes registers the Sandbox, SandboxClass and Snapshot kinds and
// their lists.
func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(SchemeGroupVersion,
		&Sandbox{}, &SandboxList{},
		&SandboxClass{}, &SandboxClassList{},
		&Snapshot{}, &SnapshotList{},
	)
	metav1.AddToGroupVersion(s, SchemeGroupVersion)
	return nil
}
