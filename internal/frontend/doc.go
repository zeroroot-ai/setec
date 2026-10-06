// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package frontend serves SandboxService, the gRPC API of setec. It turns
// each call of an enrolled client into Sandbox objects in the namespace of
// the pair of client and tenant, and reads their state back
// (docs/frontend-api.md).
package frontend
