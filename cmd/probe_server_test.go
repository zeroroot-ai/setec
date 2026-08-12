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

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// The probe server must run on every replica, not just the leader.
//
// setec#225: it was a bare manager.RunnableFunc, and controller-runtime puts
// a Runnable in the leader-election group unless it implements
// LeaderElectionRunnable and says no. So the standby replica never bound the
// probe port, the kubelet's liveness probe got "connection refused", and it
// was killed on a ~60s loop forever — the Deployment never reached 2/2 at the
// chart's default replica count.
func TestProbeServerDoesNotWaitForLeadership(t *testing.T) {
	var r manager.Runnable = newProbeServer("127.0.0.1:0", &readyzState{})

	ler, ok := r.(manager.LeaderElectionRunnable)
	if !ok {
		t.Fatalf("probe server does not implement manager.LeaderElectionRunnable, "+
			"so controller-runtime defers it until leadership is acquired "+
			"and a standby replica serves no probes at all (setec#225); got %T", r)
	}
	if ler.NeedLeaderElection() {
		t.Fatal("probe server reports NeedLeaderElection() = true; health is a " +
			"property of the process, not of leadership, and a standby that " +
			"cannot answer /healthz is killed by the kubelet (setec#225)")
	}
}

// And it must actually serve while no lease is held — the interface assertion
// above is necessary but not sufficient.
func TestProbeServerServesBeforeLeadershipIsAcquired(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := newProbeServer(addr, &readyzState{})
	errCh := make(chan error, 1)
	// Started directly, with nothing standing in for leader election: this is
	// the standby's situation.
	go func() { errCh <- srv.Start(ctx) }()

	for _, path := range []string{"/healthz", "/readyz"} {
		if err := waitForOK(fmt.Sprintf("http://%s%s", addr, path)); err != nil {
			t.Fatalf("%s never answered without leadership: %v", path, err)
		}
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("probe server returned %v on shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("probe server did not shut down when its context was cancelled")
	}
}

func waitForOK(url string) error {
	deadline := time.Now().Add(10 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			last = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err := resp.Body.Close(); err != nil {
			return err
		}
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		last = fmt.Errorf("status %d", resp.StatusCode)
		time.Sleep(50 * time.Millisecond)
	}
	return last
}
