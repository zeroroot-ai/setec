// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// selectorProbeScript runs the reachability checks for setec#76 from inside
// a Sandbox and prints one marker per check. Each probe has a hard timeout
// so a dropped packet is a BLOCKED marker, not a hung Pod. The script
// always exits 0 so the Sandbox completes and its logs can be read.
//
//	DNS-POLICY  a lookup sent to the kube-dns ClusterIP by address. Only
//	            the NetworkPolicy decides this one: after kube-proxy's
//	            translation the packet reaches a CoreDNS Pod, which a
//	            selector allowance permits and an ipBlock cannot.
//	DNS-CONFIG  the same lookup through /etc/resolv.conf. This one also
//	            needs the operator to have pointed the Pod at cluster DNS,
//	            which it does only for a class with a port-53 allowance.
//	SERVICE     a TCP connect to a Service ClusterIP whose backend Pod
//	            carries a named port the allowance lists.
//	RESERVED    an address inside the reserved ranges. MUST stay blocked
//	            with the allowance present.
//
// %[1]s is the kube-dns ClusterIP, %[2]s the Service ClusterIP, %[3]d the
// Service port.
const selectorProbeScript = `
probe() {
  if timeout 8 sh -c "$2" >/dev/null 2>&1; then
    echo "PROBE $1 REACHABLE"
  else
    echo "PROBE $1 BLOCKED"
  fi
}
probe DNS-POLICY "nslookup kubernetes.default.svc.cluster.local %[1]s"
probe DNS-CONFIG "nslookup kubernetes.default.svc.cluster.local"
probe SERVICE "nc -z -w 5 %[2]s %[3]d"
probe RESERVED "nc -z -w 5 10.0.0.1 443"
echo PROBES-DONE
`

const (
	// selectorTargetNamespace hosts the Service the allowed class reaches.
	// It is NOT a Sandbox namespace: the baseline default-deny the chart
	// lays on those would refuse the target Pod's ingress.
	selectorTargetNamespace = "e2e-egress-target"
	selectorTargetName      = "egress-target"
	selectorTargetPortName  = "web"
	selectorTargetPort      = 8080
	selectorServicePort     = 80
)

// TestEgress_SelectorAllowance is the e2e proof for setec#76 on a
// NetworkPolicy-enforcing CNI: a Sandbox in a class allowed to reach
// kube-dns resolves a record CoreDNS carries, one allowed to reach a
// Service's Pods on a named port connects, and a Sandbox in a class with
// no allowance can do neither. The reserved ranges stay blocked for both.
//
// Both classes are built here rather than taken from the chart: the
// suite installs with sandboxClasses.enabled=false, and the plain class
// is the shipped `tool` posture (external-only, no allowance) on the
// backend this run uses.
func TestEgress_SelectorAllowance(t *testing.T) {
	if !envtestOK(t) {
		t.Skip("requires the chart installed against a real cluster")
	}
	if !cniEnforcesNetworkPolicy(t) {
		t.Skip("cluster CNI does not enforce NetworkPolicy; " +
			"run this against kind (kindnet), Cilium, Calico, or VPC CNI with enableNetworkPolicy=true")
	}
	if clusterDNSIP == "" {
		t.Skip("no kube-system/kube-dns Service on this cluster; the operator has no cluster DNS to point a class at")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*defaultWait)
	defer cancel()

	serviceIP := createSelectorTarget(ctx, t)
	backend := chain6Backend()

	kubeDNS := setecv1alpha1.EgressAllowSelector{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"},
		},
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"k8s-app": "kube-dns"},
		},
		Ports: []setecv1alpha1.EgressAllowPort{
			{Protocol: corev1.ProtocolUDP, Port: intstr.FromInt32(53)},
			{Protocol: corev1.ProtocolTCP, Port: intstr.FromInt32(53)},
		},
	}
	target := setecv1alpha1.EgressAllowSelector{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": selectorTargetNamespace},
		},
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"app": selectorTargetName},
		},
		// The named port, on purpose: the Service maps 80 to it, and the
		// policy is evaluated against the Pod's own port after
		// kube-proxy's translation.
		Ports: []setecv1alpha1.EgressAllowPort{{Port: intstr.FromString(selectorTargetPortName)}},
	}

	classSpec := func(allowances ...setecv1alpha1.EgressAllowSelector) setecv1alpha1.SandboxClassSpec {
		return setecv1alpha1.SandboxClassSpec{
			VMM:                  setecv1alpha1.VMMFirecracker,
			Runtime:              &setecv1alpha1.SandboxClassRuntime{Backend: backend},
			DefaultNetworkMode:   setecv1alpha1.NetworkModeExternalOnly,
			EgressAllowSelectors: allowances,
		}
	}
	allowed := createSelectorClass(ctx, t, "e2e-egress-allowed", classSpec(kubeDNS, target))
	plain := createSelectorClass(ctx, t, "e2e-egress-plain", classSpec())

	script := fmt.Sprintf(selectorProbeScript, clusterDNSIP, serviceIP, selectorServicePort)
	allowedLogs := runSelectorProbe(ctx, t, "egress-allowed-probe", allowed, script)
	plainLogs := runSelectorProbe(ctx, t, "egress-plain-probe", plain, script)

	// The allowed class reaches exactly what it was granted.
	requireMarker(t, allowedLogs, "DNS-POLICY", "REACHABLE",
		"the kube-dns allowance did not let the Sandbox query CoreDNS by address; "+
			"the selector peer is not enforced as expected")
	requireMarker(t, allowedLogs, "DNS-CONFIG", "REACHABLE",
		"the Sandbox was not pointed at cluster DNS although its class grants port 53 to kube-dns "+
			"(netpol.Config.ResolversFor)")
	requireMarker(t, allowedLogs, "SERVICE", "REACHABLE",
		"the named-port allowance did not let the Sandbox reach the Service's Pod")
	requireMarker(t, allowedLogs, "RESERVED", "BLOCKED",
		"a reserved-range address is reachable with a selector allowance present; the allowance widened more than its peer")

	// The plain class reaches none of it.
	requireMarker(t, plainLogs, "DNS-POLICY", "BLOCKED",
		"a class with no allowance can query CoreDNS by address; the kube-dns hole is open to every class")
	requireMarker(t, plainLogs, "DNS-CONFIG", "BLOCKED",
		"a class with no allowance resolves a cluster record; its Pod was pointed at cluster DNS")
	requireMarker(t, plainLogs, "SERVICE", "BLOCKED",
		"a class with no allowance reaches the Service's Pod")
	requireMarker(t, plainLogs, "RESERVED", "BLOCKED",
		"a reserved-range address is reachable from the plain class")

	t.Logf("setec#76 e2e passed (backend=%s, kube-dns=%s, service=%s:%d)",
		backend, clusterDNSIP, serviceIP, selectorServicePort)
}

// createSelectorTarget creates the target namespace, a busybox httpd Pod
// with a named container port, and a Service in front of it. It returns
// the Service ClusterIP once the Pod runs.
func createSelectorTarget(ctx context.Context, t *testing.T) string {
	t.Helper()

	applyYAML(ctx, t, fmt.Sprintf(`
apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
---
apiVersion: v1
kind: Pod
metadata:
  name: %[2]s
  namespace: %[1]s
  labels:
    app: %[2]s
spec:
  containers:
    - name: httpd
      image: busybox:1.36
      command: ["httpd", "-f", "-p", "%[4]d"]
      ports:
        - name: %[3]s
          containerPort: %[4]d
---
apiVersion: v1
kind: Service
metadata:
  name: %[2]s
  namespace: %[1]s
spec:
  selector:
    app: %[2]s
  ports:
    - port: %[5]d
      targetPort: %[3]s
`, selectorTargetNamespace, selectorTargetName, selectorTargetPortName, selectorTargetPort, selectorServicePort))
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), briefWait)
		defer cancel()
		_ = deleteNamespace(c, selectorTargetNamespace)
	})

	if !eventuallyPodRunning(ctx, selectorTargetNamespace, selectorTargetName, defaultWait, defaultPoll) {
		t.Fatalf("target Pod %s/%s did not reach Running", selectorTargetNamespace, selectorTargetName)
	}

	var svc corev1.Service
	key := client.ObjectKey{Namespace: selectorTargetNamespace, Name: selectorTargetName}
	if err := k8sClient.Get(ctx, key, &svc); err != nil {
		t.Fatalf("get target Service: %v", err)
	}
	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == corev1.ClusterIPNone {
		t.Fatalf("target Service has no ClusterIP: %q", svc.Spec.ClusterIP)
	}
	return svc.Spec.ClusterIP
}

// createSelectorClass creates a SandboxClass through newSandboxClass and
// removes it at the end of the test.
func createSelectorClass(ctx context.Context, t *testing.T, name string, spec setecv1alpha1.SandboxClassSpec) string {
	t.Helper()
	cls := newSandboxClass(name, spec)
	if err := k8sClient.Create(ctx, cls); err != nil {
		t.Fatalf("create SandboxClass %q: %v", name, err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &setecv1alpha1.SandboxClass{
			ObjectMeta: metav1.ObjectMeta{Name: name},
		})
	})
	return name
}

// runSelectorProbe runs the probe script in a Sandbox of the given class
// and returns the Pod's logs once the Sandbox completes.
func runSelectorProbe(ctx context.Context, t *testing.T, name, className, script string) string {
	t.Helper()

	spec := minimalSpec("sh", "-c", script)
	spec.SandboxClassName = className
	spec.Network = &setecv1alpha1.Network{Mode: setecv1alpha1.NetworkModeExternalOnly}
	sb := newSandbox(name, spec)
	createAndCleanup(t, sb)

	waitForPhase(t, client.ObjectKeyFromObject(sb), defaultWait, setecv1alpha1.SandboxPhaseCompleted)

	raw, err := exec.CommandContext(ctx, "kubectl", "-n", sandboxNamespace, "logs", name+"-vm").CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl logs %s: %v (%s)", name, err, raw)
	}
	logs := string(raw)
	if !strings.Contains(logs, "PROBES-DONE") {
		t.Fatalf("probe script in %s did not finish; logs:\n%s", name, logs)
	}
	t.Logf("%s (class %s) probes:\n%s", name, className, strings.TrimSpace(logs))
	return logs
}

// requireMarker asserts one probe outcome and explains what a miss means.
func requireMarker(t *testing.T, logs, probe, want, meaning string) {
	t.Helper()
	marker := fmt.Sprintf("PROBE %s %s", probe, want)
	if !strings.Contains(logs, marker) {
		t.Errorf("missing %q: %s\nlogs:\n%s", marker, meaning, logs)
	}
}
