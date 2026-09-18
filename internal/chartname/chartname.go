// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package chartname derives the object names the setec Helm chart renders
// for a release, so a Go caller that reads those objects back agrees with
// the chart on what they are called.
package chartname

import "strings"

// ChartName is the chart's name (Chart.yaml), the value the fullname
// helper folds into a release name that does not already contain it.
const ChartName = "setec"

// maxNameLength is the Kubernetes name limit the chart truncates to.
const maxNameLength = 63

// Fullname mirrors the chart's `setec.fullname` template for a release
// with no fullnameOverride and no nameOverride:
//
//   - a release name that contains the chart name is used unchanged
//     (setec-e2e-suites renders Deployment setec-e2e-suites);
//   - any other release name gets the chart name appended
//     (chain6-exit renders Deployment chain6-exit-setec);
//   - the result is cut to 63 characters and loses a trailing "-".
//
// Every component name hangs off it: <fullname>-runtime-agent,
// <fullname>-node-agent, <fullname>-installer, <fullname>-webhook.
func Fullname(release string) string {
	name := release
	if !strings.Contains(release, ChartName) {
		name = release + "-" + ChartName
	}
	if len(name) > maxNameLength {
		name = name[:maxNameLength]
	}
	return strings.TrimSuffix(name, "-")
}
