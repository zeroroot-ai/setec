//go:build e2e

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

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// probeScript runs three reachability checks from inside the Sandbox and
// prints one marker line per check. Each probe has a hard timeout so a
// dropped packet shows up as a failure rather than as a hung Pod.
//
// The probes are, in order:
//
//	EXTERNAL — a public address. MUST be reachable: a scanning workload
//	           that cannot leave the cluster is a broken product.
//	RESERVED — an address inside the operator's reserved ranges,
//	           standing in for the control plane. MUST NOT be reachable.
//	IMDS     — the cloud instance-metadata endpoint. MUST NOT be
//	           reachable; reaching it yields the node's role credentials.
//
// The script always exits 0 so the Sandbox completes and its logs can be
// read; the assertions are on the markers, not the exit code.
const probeScript = `
probe() {
  if timeout 8 nc -z -w 5 "$2" "$3" 2>/dev/null; then
    echo "PROBE $1 REACHABLE"
  else
    echo "PROBE $1 BLOCKED"
  fi
}
probe EXTERNAL 1.1.1.1 443
probe RESERVED %s %d
probe IMDS 169.254.169.254 80
echo PROBES-DONE
`

// TestEgress_ExternalReachableControlPlaneNot is the end-to-end proof of
// the containment posture. It asserts BOTH halves, because a policy that
// blocks the control plane by also blocking the internet has not solved
// the problem — it has replaced it with a different one.
//
// It requires a cluster whose CNI actually enforces NetworkPolicy. A
// rendered policy is not evidence of an enforced policy, which is exactly
// why this test exists rather than another manifest assertion.
func TestEgress_ExternalReachableControlPlaneNot(t *testing.T) {
	if !envtestOK(t) {
		t.Skip("requires the chart installed against a real cluster")
	}
	if !cniEnforcesNetworkPolicy(t) {
		t.Skip("cluster CNI does not enforce NetworkPolicy; " +
			"run this against Cilium, Calico, or VPC CNI with enableNetworkPolicy=true")
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultWait)
	defer cancel()

	ns := "e2e-egress"
	createTenantNamespace(ctx, t, ns)

	// A reserved-range address stands in for any in-cluster endpoint.
	// Using a literal from the reserved list rather than resolving the
	// daemon Service keeps the test independent of which services happen
	// to be installed.
	const reservedHost = "10.0.0.1"
	const reservedPort = 443

	name := "egress-probe"
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: setecv1alpha1.SandboxSpec{
			Image:   "busybox:1.36",
			Command: []string{"sh", "-c", fmt.Sprintf(probeScript, reservedHost, reservedPort)},
			Resources: setecv1alpha1.Resources{
				VCPU: 1, Memory: resource.MustParse("128Mi"),
			},
			Network: &setecv1alpha1.Network{Mode: setecv1alpha1.NetworkModeExternalOnly},
		},
	}
	if err := k8sClient.Create(ctx, sb); err != nil {
		t.Fatalf("create Sandbox: %v", err)
	}

	waitForPhaseCtx(ctx, t, ns, name, setecv1alpha1.SandboxPhaseCompleted, 2*time.Minute)

	raw, err := exec.Command("kubectl", "-n", ns, "logs", name+"-vm").CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl logs %s: %v (%s)", name, err, raw)
	}
	logs := string(raw)
	if !strings.Contains(logs, "PROBES-DONE") {
		t.Fatalf("probe script did not finish; logs:\n%s", logs)
	}

	// The permissive half. If this fails the change has broken the
	// product: external scan targets are unreachable.
	if !strings.Contains(logs, "PROBE EXTERNAL REACHABLE") {
		t.Errorf("external address is NOT reachable under mode=external-only. "+
			"Legitimate outbound scanning is broken by this policy.\nlogs:\n%s", logs)
	}

	// The restrictive half.
	if !strings.Contains(logs, "PROBE RESERVED BLOCKED") {
		t.Errorf("reserved range %s:%d is reachable from a Sandbox; "+
			"in-cluster endpoints are not confined.\nlogs:\n%s", reservedHost, reservedPort, logs)
	}
	if !strings.Contains(logs, "PROBE IMDS BLOCKED") {
		t.Errorf("instance-metadata endpoint is reachable from a Sandbox; "+
			"the node role can be assumed from inside the guest.\nlogs:\n%s", logs)
	}
}

// TestEgress_ModeNoneBlocksEverything checks the deny-all posture that an
// unstated spec.network now resolves to.
func TestEgress_ModeNoneBlocksEverything(t *testing.T) {
	if !envtestOK(t) {
		t.Skip("requires the chart installed against a real cluster")
	}
	if !cniEnforcesNetworkPolicy(t) {
		t.Skip("cluster CNI does not enforce NetworkPolicy")
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultWait)
	defer cancel()

	ns := "e2e-egress-none"
	createTenantNamespace(ctx, t, ns)

	name := "denied-probe"
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: setecv1alpha1.SandboxSpec{
			Image:   "busybox:1.36",
			Command: []string{"sh", "-c", fmt.Sprintf(probeScript, "10.0.0.1", 443)},
			Resources: setecv1alpha1.Resources{
				VCPU: 1, Memory: resource.MustParse("128Mi"),
			},
			Network: &setecv1alpha1.Network{Mode: setecv1alpha1.NetworkModeNone},
		},
	}
	if err := k8sClient.Create(ctx, sb); err != nil {
		t.Fatalf("create Sandbox: %v", err)
	}

	waitForPhaseCtx(ctx, t, ns, name, setecv1alpha1.SandboxPhaseCompleted, 2*time.Minute)

	raw, err := exec.Command("kubectl", "-n", ns, "logs", name+"-vm").CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl logs %s: %v (%s)", name, err, raw)
	}
	logs := string(raw)

	for _, probe := range []string{"EXTERNAL", "RESERVED", "IMDS"} {
		if !strings.Contains(logs, "PROBE "+probe+" BLOCKED") {
			t.Errorf("mode=none left %s reachable.\nlogs:\n%s", probe, logs)
		}
	}
}
