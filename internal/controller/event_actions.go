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

package controller

// Action constants for events emitted by setec controllers. These populate the
// `action` field of eventsv1.Event (added in the controller-runtime/events
// API migration). See spec setec-events-api-migration for the mapping rationale.

// SandboxReconciler actions.
const (
	actionReconcileSandbox    = "ReconcileSandbox"
	actionResolveTenant       = "ResolveTenant"
	actionResolveSandboxClass = "ResolveSandboxClass"
	actionValidateConstraints = "ValidateConstraints"
	actionResolveRuntime      = "ResolveRuntime"
	actionResolveSnapshot     = "ResolveSnapshot"
	actionCreateSandboxPod    = "CreateSandboxPod"
	actionApplyNetworkPolicy  = "ApplyNetworkPolicy"
	actionPauseSandbox        = "PauseSandbox"
	actionResumeSandbox       = "ResumeSandbox"
	actionRequestSnapshot     = "RequestSnapshot"
	actionRunRuntimeFallback  = "RunRuntimeFallback"
	actionEnforceTimeout      = "EnforceTimeout"
	actionFinalizeSandbox     = "FinalizeSandbox"
)

// SnapshotReconciler actions.
const (
	actionDeleteSnapshot = "DeleteSnapshot"
)

// Coordinator actions are defined in internal/snapshot/coordinator.go to avoid
// an import cycle (internal/controller imports internal/snapshot, so coordinator
// cannot import this package). The constant actionRecordSnapshotPhase = "RecordSnapshotPhase"
// lives there.
