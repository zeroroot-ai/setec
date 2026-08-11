# Observability

Setec Phase 2 exports Prometheus metrics and OpenTelemetry traces for
every sandbox lifecycle event. Both are opt-in through the Helm chart.

## Prometheus metrics

Scrape the operator on `--metrics-bind-address` (default `:8080`) and
the node-agent on port `9090`.

### Operator metrics

All metric names are prefixed with `setec_sandbox_` and every series
carries the label set documented below. The `tenant` label is always
present — an empty-string value means "no tenant / single-tenant mode".

| Metric | Type | Labels | Notes |
| --- | --- | --- | --- |
| `setec_sandbox_total` | Counter | `phase`, `tenant`, `sandbox_class` | Increments once per observed phase transition. |
| `setec_sandbox_duration_seconds` | Histogram | `phase`, `tenant`, `sandbox_class` | Phase durations. Default Prometheus buckets. |
| `setec_sandbox_cold_start_seconds` | Histogram | `vmm`, `sandbox_class` | Time from Sandbox creation to Pod Running. Exponential buckets (0.1s–204.8s). |
| `setec_sandbox_active` | Gauge | `tenant`, `sandbox_class` | Current active Sandbox count. |

Cardinality notes: the label set is bounded by (tenants × classes ×
phases) plus the `vmm` axis on cold-start. A large deployment with
50 tenants and 10 classes yields roughly (50 × 10 × 4) = 2000 series
per metric — well within Prometheus's comfort zone.

### Node-agent metrics

Each node-agent instance exports:

| Metric | Type | Notes |
| --- | --- | --- |
| `setec_node_thinpool_used_bytes` | Gauge | Allocated bytes in the devicemapper thin-pool. |
| `setec_node_thinpool_total_bytes` | Gauge | Total bytes in the thin-pool. |
| `setec_node_kata_runtime_ready` | Gauge | 1 if `/dev/kvm` is present, else 0. |

The agent exposes `/metrics` and `/healthz` on port 9090.

## OpenTelemetry traces

Enable tracing by passing `--otlp-endpoint=<collector-addr>:4317` (or
setting `observability.otlpEndpoint` in the chart). An empty endpoint
disables tracing completely — no spans are created and the SDK is a
no-op tracer, so disabled deployments pay near-zero runtime cost.

The tracer reports the service name `setec-operator` and uses the
standard OpenTelemetry semantic conventions for Kubernetes resources.

### Span hierarchy

A typical reconcile emits a single `Reconcile.Sandbox` root span with
attributes:

- `k8s.namespace.name` — Sandbox namespace.
- `k8s.object.name` — Sandbox name.
- `setec.sandbox.class` — resolved SandboxClass name (empty when none).
- `setec.sandbox.phase` — final phase this reconcile observed.

Failed reconciles set the span status to `Error` with a message
describing the cause (tenant label missing, class not found, constraint
violation, etc.).

### Collector authentication

The OTLP channel has two shapes and never both. Configuring both is a
startup error naming the conflict.

**One-way TLS (default).** The collector is authenticated and the
operator presents no identity. `--otel-ca-file` points at a PEM bundle;
empty uses the host root store. A bundle that is set but unreadable or
unparseable is fatal — silently widening back to the host roots would
be the opposite of what the operator asked for. The floor on this hop is
TLS 1.2 rather than setec's usual 1.3, because the peer is frequently a
vendor gateway or a terminating proxy the operator does not control.

Be precise about what this proves: the collector cannot use this channel
to establish who is talking to it. It is not mTLS.

**Mutual TLS over SPIFFE.** `--otel-spiffe-socket` points at the SPIFFE
Workload API, and one or more `--otel-spiffe-server-id` flags name the
full SPIFFE IDs the collector may present. The operator presents its own
X509-SVID, and the collector's identity is checked rather than merely
its certificate chain. An empty allow-list is a startup error: there is
no accept-any-collector setting.

**Plaintext.** `--otel-insecure` is an explicit dev-cluster opt-out and
logs a loud warning. The chart disables it by default.

### Recommended dashboards

The Setec team has published a reference Grafana dashboard on
[grafana.com Community Dashboards]. Import by ID; the dashboard
imports the Prometheus data source variable and renders tiles for
cold-start P50/P95, active-sandbox gauge, per-tenant rate, and
thin-pool fill across nodes.

Community dashboards are OSS and vendor-neutral. Setec does not ship
any cloud-specific dashboard.

[grafana.com Community Dashboards]: https://grafana.com/grafana/dashboards

## Grafana Dashboard

The chart ships a ready-to-import dashboard JSON at
[`charts/setec/grafana/setec-operator.json`](../charts/setec/grafana/setec-operator.json).
UID is `setec-operator`; the dashboard auto-binds to a Prometheus
datasource via the `DS_PROMETHEUS` variable and exposes a `namespace`
filter so multi-install deployments can scope to a single operator.

### Panels

| Panel                                 | What it shows                                                           |
|---------------------------------------|-------------------------------------------------------------------------|
| Active sandboxes                       | Single-stat over `setec_sandbox_active`.                                |
| Sandbox phase transitions              | Per-phase rate of `setec_sandbox_total`.                                |
| Cold-start latency heatmap             | `setec_sandbox_cold_start_seconds` bucket distribution over time.       |
| Cold-start P50 / P95 / P99             | Histogram quantiles over the same bucket series.                        |
| Snapshot duration by operation         | P95 of `setec_snapshot_duration_seconds` split by create/restore/etc.   |
| Pre-warm pool entries per node/class   | `setec_prewarm_pool_entries` gauge.                                     |
| Operator and node-agent up-ness        | `up` metric filtered to Setec pods.                                     |
| gRPC frontend request rate             | `grpc_server_handled_total` rate split by method.                       |

### Import

Import via the Grafana UI (`Dashboards` -> `New` -> `Import`, upload
JSON), or via the grafana-operator with a `GrafanaDashboard` resource
that points at the JSON file in-cluster. Screenshots will land once
the v0.1.0 smoke-test run produces representative time-series.

## Prometheus Alerts

Two `PrometheusRule` manifests ship alongside the chart:

- [`charts/setec/prometheus/recording-rules.yaml`](../charts/setec/prometheus/recording-rules.yaml)
  &mdash; roll-up series (`setec:sandbox_cold_start_p50`, `p95`, `p99`,
  `setec:sandbox_error_rate`, `setec:snapshot_duration_p95`) used by
  the alerts and the dashboard.
- [`charts/setec/prometheus/alerts.yaml`](../charts/setec/prometheus/alerts.yaml)
  &mdash; the default alert set.

Apply both when the Prometheus Operator CRDs are installed:

```bash
kubectl apply -f charts/setec/prometheus/recording-rules.yaml
kubectl apply -f charts/setec/prometheus/alerts.yaml
```

### Alerts

| Alert                          | Fires when                                                             | Severity  |
|--------------------------------|------------------------------------------------------------------------|-----------|
| `SetecOperatorDown`            | No operator pod has reported `up=1` for 5 minutes.                     | critical  |
| `SetecNodeAgentDown`           | A node-agent has been absent on any node for 10 minutes.               | warning   |
| `SetecColdStartSLOBreach`      | Cold-start P95 above 1s for 10 minutes for any sandbox class.          | warning   |
| `SetecSnapshotFailureHigh`     | Snapshot create or restore failure rate above 5% for 10 minutes.       | critical  |
| `SetecPoolUnderfilled`         | Actual pool entry count below target for 10 minutes.                   | warning   |

Each alert carries a `runbook_url` annotation pointing into this doc.
Treat these as starting points; tune the thresholds to your cluster's
shape and the SLOs you want to hold the operator to.

