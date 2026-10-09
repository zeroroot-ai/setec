// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package credguard fails the build when an mTLS credential is assembled
// anywhere but internal/credentials. The guard is a test: `go test
// ./internal/credguard/...` (make guard-credentials) scans the tree. Its
// scanner lives in scan_test.go and its allow-list in exemptions_test.go,
// because nothing but that test runs them.
package credguard
