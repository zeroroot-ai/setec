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

package frontend

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Lease-pool gauges. They are frontend-scoped (the lease layer runs in
// the frontend process, distinct from the node-agent's node-local
// prewarm pool gauge) and registered lazily on the default registry so
// the package has no init-time side effects. registerOnce guards against
// a duplicate-registration panic if the frontend is constructed twice in
// one process (e.g. across tests sharing the default registry).
var (
	registerOnce      sync.Once
	leasePoolReady    *prometheus.GaugeVec
	leasePoolLeasedG  *prometheus.GaugeVec
	leasePoolRegister = func() {
		leasePoolReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "setec_lease_pool_ready",
			Help: "Number of pre-warmed Sandboxes currently ready to lease, per namespace and class.",
		}, []string{"namespace", "sandbox_class"})
		leasePoolLeasedG = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "setec_lease_pool_leased",
			Help: "Number of Sandboxes currently leased (checked out) per namespace and class.",
		}, []string{"namespace", "sandbox_class"})
		// Best-effort registration: a duplicate registration (already
		// registered) is ignored so tests that build several services do
		// not panic.
		_ = prometheus.Register(leasePoolReady)
		_ = prometheus.Register(leasePoolLeasedG)
	}
)

// poolFillGauge records the current ready/leased counts for a pool. Safe
// to call before any explicit init; registration happens on first use.
func poolFillGauge(namespace, sandboxClass string, ready, leased int) {
	registerOnce.Do(leasePoolRegister)
	if leasePoolReady != nil {
		leasePoolReady.WithLabelValues(namespace, sandboxClass).Set(float64(ready))
	}
	if leasePoolLeasedG != nil {
		leasePoolLeasedG.WithLabelValues(namespace, sandboxClass).Set(float64(leased))
	}
}
