// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build e2e

package e2e

// The abilities that only the launcher has (setec#197): a restore with the
// five isolation invariants of ADR-0145, the warm pool, a fork into three
// Sandboxes and a kept snapshot. Suspend, resume and a resume on another
// node after a drain are the scenarios of session_checkpoint_test.go, run
// on the launcher backend. Each scenario here runs only on that backend.

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/snapshot"
	"github.com/zeroroot-ai/setec/internal/status"
)

// launcherRestoreWait bounds a restore: the node agent fetches the
// snapshot, and the launcher loads it and confirms the guest.
const launcherRestoreWait = 6 * time.Minute

// launcherImage is the image of each launcher scenario.
var launcherImage = func() string { return testImage("docker.io/library/alpine:3.19") }

// launcherResources is the machine size of each launcher scenario.
func launcherResources() setecv1alpha1.Resources {
	return setecv1alpha1.Resources{VCPU: 1, Memory: resource.MustParse("512Mi")}
}

// requireLauncher skips a scenario of the launcher on another backend. The
// launcher pass never skips: it runs on the launcher backend only.
func requireLauncher(t *testing.T) {
	t.Helper()
	if !onLauncher() {
		t.Skipf("a launcher scenario; SETEC_E2E_BACKEND=%s", sandboxBackend)
	}
}

// launcherSandbox returns a Sandbox of the suite class that runs cmd.
func launcherSandbox(name string, cmd string) *setecv1alpha1.Sandbox {
	spec := minimalSpec("/bin/sh", "-c", cmd)
	spec.Image = launcherImage()
	spec.Resources = launcherResources()
	return newSandbox(name, spec)
}

// guestIdentity is what a guest reports about itself.
type guestIdentity struct {
	machineID, bootID, hostname, ip, rand string
	clock                                 int64
}

// identityScript prints the identity of the guest, one key=value a line.
const identityScript = `echo machine-id=$(cat /etc/machine-id 2>/dev/null)
echo boot-id=$(cat /proc/sys/kernel/random/boot_id)
echo hostname=$(hostname)
echo ip=$(ip -o -4 addr show scope global | awk '{print $4}' | cut -d/ -f1 | head -n1)
echo rand=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
echo clock=$(date +%s)`

// readGuestIdentity runs identityScript in the guest of a Sandbox.
func readGuestIdentity(t *testing.T, ns, name string) guestIdentity {
	t.Helper()
	out := mustLauncherExec(t, ns, name, "sh", "-c", identityScript)
	f := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			f[k] = v
		}
	}
	clock, _ := strconv.ParseInt(f["clock"], 10, 64)
	id := guestIdentity{machineID: f["machine-id"], bootID: f["boot-id"], hostname: f["hostname"],
		ip: f["ip"], rand: f["rand"], clock: clock}
	t.Logf("%s/%s: %+v", ns, name, id)
	return id
}

// sandboxEventReasons returns the reasons of the Events of a Sandbox. It
// reads the Events of this object only, by UID: an earlier Sandbox of the
// same name has its own Events.
func sandboxEventReasons(t *testing.T, ns, name string) []string {
	t.Helper()
	sb, err := getSandboxE2E(types.NamespacedName{Namespace: ns, Name: name})
	if err != nil {
		t.Fatalf("get %s/%s: %v", ns, name, err)
	}
	out, err := exec.Command("kubectl", "-n", ns, "get", "events",
		"--field-selector", "involvedObject.kind=Sandbox,involvedObject.uid="+string(sb.UID),
		"-o", "jsonpath={.items[*].reason}").CombinedOutput()
	if err != nil {
		t.Fatalf("events of %s/%s: %v (%s)", ns, name, err, out)
	}
	return strings.Fields(string(out))
}

// assertRestoredGuest checks invariant 2 of ADR-0145 on a restored
// Sandbox: the gate saw fresh randomness and a new identity, nothing was
// served without that evidence, and the guest has its own machine-id,
// randomness and clock and sees the address of its own Pod. Invariant 5
// has no other check here: the gate refuses a restore whose snapshot is
// not sealed at rest, so a Running restore proves it. It returns the
// identity of the guest.
func assertRestoredGuest(t *testing.T, ns, name string, others ...guestIdentity) guestIdentity {
	t.Helper()
	reasons := sandboxEventReasons(t, ns, name)
	for _, want := range []string{snapshot.EventReasonEntropyReseeded, snapshot.EventReasonSandboxUniquified} {
		if !slices.Contains(reasons, want) {
			t.Errorf("%s/%s has no %s event; events: %v", ns, name, want, reasons)
		}
	}
	if slices.Contains(reasons, snapshot.EventReasonUnverifiedRestoreAllowed) {
		t.Errorf("%s/%s was served without evidence: %v", ns, name, reasons)
	}
	id := readGuestIdentity(t, ns, name)
	if podIP := sandboxPod(t, ns, name).Status.PodIP; id.ip != podIP {
		t.Errorf("%s/%s: the guest sees %q, its Pod has %q", ns, name, id.ip, podIP)
	}
	if skew := time.Now().Unix() - id.clock; skew > 30 || skew < -30 {
		t.Errorf("%s/%s: the guest clock is %d s off", ns, name, skew)
	}
	if id.machineID == "" || id.rand == "" {
		t.Errorf("%s/%s: no machine-id or no randomness: %+v", ns, name, id)
	}
	for _, o := range others {
		if id.machineID == o.machineID {
			t.Errorf("%s/%s shares the machine-id %q", ns, name, id.machineID)
		}
		if id.rand == o.rand {
			t.Errorf("%s/%s shares the random bytes %q", ns, name, id.rand)
		}
	}
	return id
}

// waitForSnapshotReady waits until a Snapshot is Ready and fails on Failed.
func waitForSnapshotReady(t *testing.T, ns, name string, timeout time.Duration) *setecv1alpha1.Snapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	snap := &setecv1alpha1.Snapshot{}
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, snap); err == nil {
			switch snap.Status.Phase {
			case setecv1alpha1.SnapshotPhaseReady:
				return snap
			case setecv1alpha1.SnapshotPhaseFailed:
				t.Fatalf("snapshot %s/%s failed: %s", ns, name, snap.Status.Reason)
			}
		}
		time.Sleep(defaultPoll)
	}
	t.Fatalf("snapshot %s/%s is not Ready after %s; phase %q", ns, name, timeout, snap.Status.Phase)
	return nil
}

// takeSnapshot asks a Running Sandbox for a snapshot and waits for it.
func takeSnapshot(
	t *testing.T, sb *setecv1alpha1.Sandbox, req setecv1alpha1.SandboxSnapshotSpec,
) *setecv1alpha1.Snapshot {
	t.Helper()
	req.Create = true
	if req.AfterCreate == "" {
		req.AfterCreate = setecv1alpha1.SandboxSnapshotAfterCreateRunning
	}
	patch := client.MergeFrom(sb.DeepCopy())
	sb.Spec.Snapshot = &req
	if err := k8sClient.Patch(context.Background(), sb, patch); err != nil {
		t.Fatalf("ask %s for a snapshot: %v", sb.Name, err)
	}
	start := time.Now()
	snap := waitForSnapshotReady(t, sb.Namespace, req.Name, launcherRestoreWait)
	t.Logf("snapshot %s Ready in %s", req.Name, time.Since(start).Round(time.Millisecond))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), snap) })
	return snap
}

// restoreFrom creates a Sandbox that starts from a snapshot.
func restoreFrom(t *testing.T, name, snap string, edit func(*setecv1alpha1.SandboxSpec)) *setecv1alpha1.Sandbox {
	t.Helper()
	sb := launcherSandbox(name, "true")
	sb.Spec.SnapshotRef = &setecv1alpha1.SandboxSnapshotRef{Name: snap}
	if edit != nil {
		edit(&sb.Spec)
	}
	createAndCleanup(t, sb)
	return sb
}

// waitRunning waits for a Sandbox to be Running and returns the time it
// took from its creation.
func waitRunning(t *testing.T, sb *setecv1alpha1.Sandbox, timeout time.Duration) time.Duration {
	t.Helper()
	got := waitForPhase(t, client.ObjectKeyFromObject(sb), timeout, setecv1alpha1.SandboxPhaseRunning)
	return time.Since(got.CreationTimestamp.Time)
}

// TestLauncher_SnapshotRestore takes a snapshot of a Running Sandbox and
// resumes it as a new machine of the same Sandbox. The state survives, and
// the restore passes invariant 2 and 5. Invariant 3: another Sandbox of the
// namespace and a Sandbox of another tenant cannot load the snapshot.
func TestLauncher_SnapshotRestore(t *testing.T) {
	requireLauncher(t)
	src := launcherSandbox("lr-src", "echo before-snapshot > /tmp/marker; sleep 3600")
	createAndCleanup(t, src)
	waitRunning(t, src, defaultWait)
	before := readGuestIdentity(t, sandboxNamespace, src.Name)

	takeSnapshot(t, src, setecv1alpha1.SandboxSnapshotSpec{Name: "lr-snap"})

	// One owner: a second Sandbox of the namespace is refused before any
	// state loads.
	other := restoreFrom(t, "lr-other", "lr-snap", nil)
	final := waitForPhase(t, client.ObjectKeyFromObject(other), launcherRestoreWait, setecv1alpha1.SandboxPhaseFailed)
	if final.Status.Reason != status.ReasonInvariantGateViolation {
		t.Errorf("another Sandbox loaded the snapshot of lr-src: reason %q", final.Status.Reason)
	}
	// A Sandbox of another tenant does not see the snapshot at all.
	ctx, cancel := context.WithTimeout(context.Background(), launcherRestoreWait)
	defer cancel()
	createTenantNamespace(ctx, t, "lr-tenant")
	foreign := launcherSandbox("lr-foreign", "true")
	foreign.Namespace = "lr-tenant"
	foreign.Spec.SandboxClassName = e2eDefaultClassName
	foreign.Spec.SnapshotRef = &setecv1alpha1.SandboxSnapshotRef{Name: "lr-snap"}
	// The webhook refuses it at admission. Without the webhook the
	// operator never finds the snapshot.
	if err := k8sClient.Create(ctx, foreign); err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), foreign) })
		time.Sleep(20 * time.Second)
		got, err := getSandboxE2E(client.ObjectKeyFromObject(foreign))
		if err == nil && got.Status.Phase == setecv1alpha1.SandboxPhaseRunning {
			t.Errorf("a Sandbox of another tenant loaded lr-snap")
		}
	} else if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("create lr-foreign: %v", err)
	}

	// The resume: the same Sandbox, a new machine.
	if err := k8sClient.Delete(context.Background(), src); err != nil {
		t.Fatalf("delete lr-src: %v", err)
	}
	waitGone(t, client.ObjectKeyFromObject(src), defaultWait)
	resumed := restoreFrom(t, "lr-src", "lr-snap", nil)
	took := waitRunning(t, resumed, launcherRestoreWait)
	t.Logf("restore of lr-src Running in %s", took.Round(time.Millisecond))
	if got := mustLauncherExec(t, sandboxNamespace, "lr-src", "cat", "/tmp/marker"); got != "before-snapshot" {
		t.Errorf("the state did not survive: /tmp/marker = %q", got)
	}
	assertRestoredGuest(t, sandboxNamespace, "lr-src", before)
}

// waitGone waits until a Sandbox and its Pod no longer exist. A new
// Sandbox of the same name must not find the old Pod.
func waitGone(t *testing.T, key client.ObjectKey, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	podKey := client.ObjectKey{Namespace: key.Namespace, Name: key.Name + "-vm"}
	for time.Now().Before(deadline) {
		_, sbErr := getSandboxE2E(key)
		podErr := k8sClient.Get(context.Background(), podKey, &corev1.Pod{})
		if apierrors.IsNotFound(sbErr) && apierrors.IsNotFound(podErr) {
			return
		}
		time.Sleep(defaultPoll)
	}
	t.Fatalf("sandbox %s or its Pod still exists after %s", key, timeout)
}

// waitForWarmPoolReady waits until the warm pool of a class has a Ready
// base and returns the class.
func waitForWarmPoolReady(
	ctx context.Context, t *testing.T, class string, timeout time.Duration,
) *setecv1alpha1.SandboxClass {
	t.Helper()
	return waitForWarmPool(ctx, t, class, timeout, func(*setecv1alpha1.SandboxClassWarmPoolStatus) bool { return true })
}

// waitForWarmPool waits until the warm pool of a class has a Ready base
// for which ok holds.
func waitForWarmPool(ctx context.Context, t *testing.T, class string, timeout time.Duration,
	ok func(*setecv1alpha1.SandboxClassWarmPoolStatus) bool,
) *setecv1alpha1.SandboxClass {
	t.Helper()
	deadline := time.Now().Add(timeout)
	cls := &setecv1alpha1.SandboxClass{}
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: class}, cls); err == nil {
			if ws := cls.Status.WarmPool; ws != nil && ws.Ready >= 1 && ok(ws) {
				return cls
			}
		}
		time.Sleep(5 * time.Second)
	}
	out, _ := exec.Command("kubectl", "-n", launcherCfg.warmPoolNamespace, "get", "pods,snapshots,events").CombinedOutput()
	t.Fatalf("the warm pool of %s has no Ready base after %s; status %+v\n%s", class, timeout, cls.Status.WarmPool, out)
	return nil
}

// poolBases returns the base Snapshots of a class.
func poolBases(t *testing.T, class string) []setecv1alpha1.Snapshot {
	t.Helper()
	list := &setecv1alpha1.SnapshotList{}
	if err := k8sClient.List(context.Background(), list, client.InNamespace(launcherCfg.warmPoolNamespace),
		client.MatchingLabels{snapshot.BaseLabel: snapshot.BaseLabelValue, snapshot.BaseClassLabel: class}); err != nil {
		t.Fatalf("list the bases of %s: %v", class, err)
	}
	return list.Items
}

// TestLauncher_WarmPool builds a base, warm starts two Sandboxes from it
// and changes the class so the base is stale. It checks invariant 1 and 4
// on the base and invariant 2 on each warm start. It reports the time of a
// warm start next to a cold start: a number never fails the test (D58).
func TestLauncher_WarmPool(t *testing.T) {
	requireLauncher(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	res := launcherResources()
	clsName := "e2e-pool-" + testNamespace
	cls := newSandboxClass(clsName, setecv1alpha1.SandboxClassSpec{
		Runtime:          &setecv1alpha1.SandboxClassRuntime{Backend: backendLauncher},
		PreWarmPoolSize:  1,
		PreWarmImage:     launcherImage(),
		DefaultResources: &res,
	})
	if err := k8sClient.Create(ctx, cls); err != nil {
		t.Fatalf("create %s: %v", clsName, err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cls) })

	start := time.Now()
	ready := waitForWarmPoolReady(ctx, t, clsName, 15*time.Minute)
	key := ready.Status.WarmPool.Key
	t.Logf("first base Ready in %s, key %s", time.Since(start).Round(time.Second), key)

	// Invariant 1 and 4: each base comes from the class image, in the pool
	// namespace, with a clean secret scan.
	for _, b := range poolBases(t, clsName) {
		if b.Status.Phase == setecv1alpha1.SnapshotPhaseReady &&
			b.Annotations[snapshot.CleanBaseAnnotation] != snapshot.BaseLabelValue {
			t.Errorf("base %s has no clean scan: %v", b.Name, b.Annotations)
		}
	}

	cold := launcherSandbox("wp-cold", "sleep 600")
	createAndCleanup(t, cold)
	coldTook := waitRunning(t, cold, defaultWait)

	seen := make([]guestIdentity, 0, 2)
	for i := range 2 {
		name := fmt.Sprintf("wp-warm-%d", i)
		warm := launcherSandbox(name, "sleep 600")
		warm.Spec.SandboxClassName = clsName
		createAndCleanup(t, warm)
		warmTook := waitRunning(t, warm, launcherRestoreWait)
		got, err := getSandboxE2E(client.ObjectKeyFromObject(warm))
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if got.Status.WarmStart == nil || got.Status.WarmStart.Outcome != setecv1alpha1.SandboxWarmStartPoolRestored {
			t.Fatalf("%s did not warm start: %+v", name, got.Status.WarmStart)
		}
		t.Logf("warm start %s Running in %s; cold start Running in %s", name,
			warmTook.Round(time.Millisecond), coldTook.Round(time.Millisecond))
		seen = append(seen, assertRestoredGuest(t, sandboxNamespace, name, seen...))
		if seen[len(seen)-1].hostname != name+"-vm" {
			t.Errorf("%s has the hostname %q", name, seen[len(seen)-1].hostname)
		}
		if i == 0 {
			waitForWarmPoolReady(ctx, t, clsName, 15*time.Minute)
		}
	}

	// A stale base: a new machine size gives a new key. The pool drops
	// the old base and builds a new one.
	patch := client.MergeFrom(cls.DeepCopy())
	bigger := setecv1alpha1.Resources{VCPU: 1, Memory: resource.MustParse("640Mi")}
	cls.Spec.DefaultResources = &bigger
	if err := k8sClient.Patch(ctx, cls, patch); err != nil {
		t.Fatalf("change the size of %s: %v", clsName, err)
	}
	waitForWarmPool(ctx, t, clsName, 15*time.Minute,
		func(ws *setecv1alpha1.SandboxClassWarmPoolStatus) bool { return ws.Key != "" && ws.Key != key })
	deadline := time.Now().Add(5 * time.Minute)
	for {
		stale := 0
		for _, b := range poolBases(t, clsName) {
			if b.Annotations[snapshot.BaseKeyAnnotation] == key {
				stale++
			}
		}
		if stale == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d stale bases of key %s remain", stale, key)
		}
		time.Sleep(5 * time.Second)
	}
}

// TestLauncher_ForkIntoThree forks a Running Sandbox into three. Each fork
// has the state of the source and its own identity, and the forks diverge.
// The source ends with its snapshot.
func TestLauncher_ForkIntoThree(t *testing.T) {
	requireLauncher(t)
	src := launcherSandbox("fk-src", "echo source-state > /tmp/marker; sleep 3600")
	createAndCleanup(t, src)
	waitRunning(t, src, defaultWait)
	before := readGuestIdentity(t, sandboxNamespace, src.Name)
	// The source ends after the snapshot, so the three forks fit on the
	// node of the snapshot with the CPU of a CI runner.
	takeSnapshot(t, src, setecv1alpha1.SandboxSnapshotSpec{Name: "fk-snap", Forkable: true,
		AfterCreate: setecv1alpha1.SandboxSnapshotAfterCreateTerminated,
		TTL:         &metav1.Duration{Duration: 30 * time.Minute}})
	waitGone(t, client.ObjectKeyFromObject(src), defaultWait)

	start := time.Now()
	names := []string{"fk-1", "fk-2", "fk-3"}
	for _, n := range names {
		restoreFrom(t, n, "fk-snap", nil)
	}
	seen := make([]guestIdentity, 0, 1+len(names))
	seen = append(seen, before)
	for _, n := range names {
		waitForPhase(t, types.NamespacedName{Namespace: sandboxNamespace, Name: n}, launcherRestoreWait,
			setecv1alpha1.SandboxPhaseRunning)
	}
	t.Logf("three forks Running in %s", time.Since(start).Round(time.Millisecond))
	for _, n := range names {
		if got := mustLauncherExec(t, sandboxNamespace, n, "cat", "/tmp/marker"); got != "source-state" {
			t.Errorf("%s has /tmp/marker = %q, want the state of the source", n, got)
		}
		id := assertRestoredGuest(t, sandboxNamespace, n, seen...)
		if id.hostname != n+"-vm" {
			t.Errorf("%s has the hostname %q", n, id.hostname)
		}
		seen = append(seen, id)
	}
	mustLauncherExec(t, sandboxNamespace, "fk-1", "sh", "-c", "echo fk-1-change > /tmp/marker")
	for _, n := range names[1:] {
		if got := mustLauncherExec(t, sandboxNamespace, n, "cat", "/tmp/marker"); got != "source-state" {
			t.Errorf("a write in fk-1 reached %s: %q", n, got)
		}
	}
}

// TestLauncher_KeptSnapshot keeps the state of a Sandbox for review. A
// normal Sandbox cannot load it. A review Sandbox with no network can. A
// pin stops the expiry, and after the unpin the snapshot expires.
func TestLauncher_KeptSnapshot(t *testing.T) {
	requireLauncher(t)
	src := launcherSandbox("kp-src", "echo kept-state > /tmp/marker; sleep 3600")
	createAndCleanup(t, src)
	waitRunning(t, src, defaultWait)
	const ttl = 3 * time.Minute
	snap := takeSnapshot(t, src, setecv1alpha1.SandboxSnapshotSpec{Name: "kp-snap", Kept: true,
		TTL: &metav1.Duration{Duration: ttl}})
	if !snap.Spec.Kept {
		t.Fatalf("kp-snap is not kept: %+v", snap.Spec)
	}

	// A normal Sandbox: the webhook refuses it, or without the webhook the
	// operator keeps it Pending.
	normal := launcherSandbox("kp-normal", "true")
	normal.Spec.SnapshotRef = &setecv1alpha1.SandboxSnapshotRef{Name: "kp-snap"}
	normalErr := k8sClient.Create(context.Background(), normal)
	if normalErr == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), normal) })
	} else if !strings.Contains(normalErr.Error(), "is kept") {
		t.Fatalf("create kp-normal: %v", normalErr)
	}
	review := restoreFrom(t, "kp-review", "kp-snap", func(s *setecv1alpha1.SandboxSpec) {
		s.Review = true
		s.Network = &setecv1alpha1.Network{Mode: setecv1alpha1.NetworkModeNone}
	})
	waitRunning(t, review, launcherRestoreWait)
	if got := mustLauncherExec(t, sandboxNamespace, review.Name, "cat", "/tmp/marker"); got != "kept-state" {
		t.Errorf("the review Sandbox has /tmp/marker = %q", got)
	}
	if normalErr == nil {
		if got, err := getSandboxE2E(client.ObjectKeyFromObject(normal)); err != nil ||
			got.Status.Phase == setecv1alpha1.SandboxPhaseRunning {
			t.Errorf("a normal Sandbox loaded a kept snapshot: %+v %v", got, err)
		}
		_ = k8sClient.Delete(context.Background(), normal)
	}

	// The pin: the snapshot outlives its TTL.
	pin := func(v bool) {
		t.Helper()
		cur := &setecv1alpha1.Snapshot{}
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(snap), cur); err != nil {
			t.Fatalf("get kp-snap: %v", err)
		}
		p := client.MergeFrom(cur.DeepCopy())
		cur.Spec.Pinned = v
		if err := k8sClient.Patch(context.Background(), cur, p); err != nil {
			t.Fatalf("pin kp-snap = %t: %v", v, err)
		}
	}
	pin(true)
	if wait := time.Until(snap.CreationTimestamp.Add(ttl + 30*time.Second)); wait > 0 {
		time.Sleep(wait)
	}
	snapKey := client.ObjectKeyFromObject(snap)
	if err := k8sClient.Get(context.Background(), snapKey, &setecv1alpha1.Snapshot{}); err != nil {
		t.Fatalf("the pinned kp-snap expired: %v", err)
	}
	// The unpin: the snapshot expires.
	pin(false)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(snap), &setecv1alpha1.Snapshot{})
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("kp-snap did not expire after the unpin")
		}
		time.Sleep(defaultPoll)
	}
}
