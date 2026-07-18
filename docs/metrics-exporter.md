# Brewlet metrics exporter — research

> **Status.** Research / design note — the **exporter itself is unbuilt** (no
> provisioner `/metrics` server, no operator/admission collectors, no chart
> wiring), so it still fleshes out the Phase 4 roadmap item *"metrics exporter"*
> and the "the shim can export node-level per-sandbox JVM metrics *(roadmap)*" note
> in [SPECIFICATION §12](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#12-networking-observability-day-2) /
> [observability.md](observability.md#metrics--tracing). **One slice already
> ships:** the shim-side emission half of Option A — a best-effort
> `brewlet_cds_archive_mapped` textfile written under `BREWLET_METRICS_DIR` on every
> launch ([appcds.md §4.3](appcds.md), `internal/runtime/cds_metric.go`). Everything
> else documents *intended* behavior, not shipped code.

---

## 1. TL;DR

- **The app's own metrics already work — that's not what this is.** Micrometer, JMX,
  OpenTelemetry and JFR run unchanged inside the sandbox
  ([observability.md](observability.md#metrics--tracing)); the app owns those. A
  Brewlet metrics exporter is for the **runtime-level telemetry only Brewlet can
  see** — per-sandbox launch internals, node JDK inventory/patch age, and
  admission/scheduling outcomes.
- **The valuable, otherwise-invisible signals** are things like: cold-start
  breakdown (artifact-resolve → bundle-prepare → overlay setup → process start),
  `NoCompatibleJDK`/`NoCompatibleLauncher`/`NoCompatibleArch` denials,
  JDK/launcher inventory per node, **JDK patch age** (the centralized-CVE lever),
  content-store cache hit/miss for the JAR, and AppCDS archive-map success rates.
- **Topology matters:** a Runtime v2 shim is a **short-lived, per-container**
  process — a poor Prometheus target. Prefer emitting per-task stats to a
  **node-local sink** scraped by the **already-present, long-lived provisioner
  DaemonSet**, which exposes `/metrics`. Operator/CRD metrics ride the operator's
  **existing** controller-runtime metrics endpoint (`:8080`).
- **Surface it as standard Prometheus**: a `brewlet_*` metric namespace, a
  chart-gated exporter + `Service`/`ServiceMonitor`, and a starter Grafana
  dashboard. Operator concern, transparent to developers.

---

## 2. What to export (and what not to)

### 2.1 In scope — Brewlet-specific signals

| Layer | Example metrics | Why only Brewlet sees it |
|---|---|---|
| **Shim / per-sandbox** | `brewlet_launch_duration_seconds{phase="resolve\|prepare\|overlay\|classpath\|start"}`, `brewlet_artifact_resolve_errors_total`, `brewlet_selected_jdk{dist,feature}`, `brewlet_selected_launcher` | Cold-start breakdown and JDK/launcher selection happen inside `Create()` (`service_linux.go`), invisible to the app or kubelet. |
| **Content store** | `brewlet_jar_contentstore_hits_total` / `_misses_total` | Whether the JAR was already cached on the node (§6.4, §13) — a startup driver. |
| **Node inventory** | `brewlet_node_jdks{dist,feature}`, `brewlet_node_launchers{name}`, `brewlet_node_ready`, `brewlet_jdk_patch_age_days{dist,feature}` | The provisioner owns the inventory (`entrypoint.sh label_node`); patch age is the CVE-management lever (§11). |
| **Admission** | `brewlet_admission_denied_total{reason}`, `brewlet_admission_steered_total` | Only the webhook (`mutate.go`) knows about `NoCompatibleJDK`/`Launcher` outcomes. |
| **Operator/CRD** | `brewlet_javaapplication_ready_replicas`, `brewlet_javaapplication_selected_jdk`, reconcile latency/errors | Reconcile state (§8.2), atop the existing controller-runtime metrics. |
| **Phase 3 tie-ins** | `brewlet_cds_archive_mapped_total` / `_stale_total` ([appcds.md](appcds.md)) | Tells operators whether accelerators are actually helping (e.g. an AppCDS archive going stale after a JDK patch). |

### 2.2 Out of scope

- Application business/JVM metrics (heap, GC, request rates) — the app exports
  these via Micrometer/JMX/OTel already. Brewlet should **not** duplicate them; at
  most it may expose coarse per-sandbox RSS the node already tracks.
- Anything requiring instrumenting user code.

---

## 3. Why the shim can't be the scrape target (topology)

A containerd Runtime v2 shim process exists roughly for the lifetime of a task and
is not addressable as a stable `/metrics` endpoint. Three viable emission paths:

| Option | Mechanism | Assessment |
|---|---|---|
| **A. Shim → node-local sink; provisioner exports** *(recommended)* | The shim writes structured per-task stats (JSON/textfile) to a host path (e.g. `/run/brewlet/metrics/<id>.prom`); the **provisioner DaemonSet** (already long-lived + privileged + per-node) runs a Prometheus textfile-collector-style `/metrics` server that aggregates them + node inventory. | Reuses the one component that's already node-resident and long-lived; clean separation; survives shim exit. |
| **B. Shim pushes to a gateway/OTel collector** | Shim pushes to a node OTel collector or Prometheus pushgateway on exit. | Adds a dependency; pushgateway semantics for short-lived jobs are awkward; keep as alternative. |
| **C. Scrape containerd** | Read containerd's own metrics + task list. | Gives cgroup stats but not Brewlet's launch-phase internals; complementary, not sufficient. |

**Recommendation: A.** The provisioner already runs as a per-node DaemonSet pod
that idles after setup (`exec sleep infinity`); give it a metrics HTTP server and a
node-local aggregation directory the shim writes to. Operator/CRD metrics stay on
the operator's existing endpoint (B/C-free, no new component).

---

## 4. How to surface it

### 4.1 Prometheus, chart-gated

```yaml
# values.yaml (new block)
metrics:
  enabled: false              # opt-in
  port: 9095
  serviceMonitor:
    enabled: false            # create a Prometheus-Operator ServiceMonitor
  # operator metrics endpoint already exists (:8080); optionally scrape it too
```

When enabled, the chart creates a `Service` (and optional `ServiceMonitor`) for the
provisioner exporter and, if desired, for the operator's `:8080`. Ship a starter
Grafana dashboard JSON (fleet readiness, cold-start percentiles, denial rates, JDK
patch age).

### 4.2 Metric namespace + labels

Standardize on `brewlet_` prefix and consistent labels (`node`, `dist`, `feature`,
`launcher`, `reason`, `phase`) drawn from the existing vocabulary in
`operator/internal/brewlet/labels.go` so metrics line up with the annotations
operators already query in [observability.md](observability.md#watching-the-fleet).

---

## 5. Implementation sketch

- **Shim (`service_linux.go`):** time the `Create()` phases already present
  (`resolveArtifact`, `assembleBrewletBundle`, `setupOverlayRootfs`,
  `mountClasspathLayers`) and write a small textfile record per task to
  `/run/brewlet/metrics/`; record selected JDK/launcher and content-store hit/miss.
  Keep it best-effort (never fail a launch because metrics couldn't be written).
  *(Partly shipped: the shim already writes a best-effort `brewlet_cds_archive_mapped`
  textfile under `BREWLET_METRICS_DIR` — `internal/runtime/cds_metric.go`. Phase
  timing and content-store hit/miss are the remaining additions.)*
- **Provisioner (`entrypoint.sh` + a small companion binary):** run an HTTP
  `/metrics` server that (a) reads/aggregates the shim textfiles, (b) emits node
  inventory from what it installed (`JDKS`/`LAUNCHERS`) and JDK patch age
  (from the JDK build date / `release` file), and (c) reflects readiness. A tiny Go
  exporter is cleaner than bash for this.
- **Operator/admission:** register custom collectors on the existing
  controller-runtime metrics registry — reconcile results, `readyReplicas`,
  `selectedJdk` (already on status), and `brewlet_admission_denied_total{reason}`
  incremented in the webhook's deny path (`webhook.go`/`mutate.go`).
- **Chart:** `Service`/`ServiceMonitor`, RBAC if needed, dashboard ConfigMap.

---

## 6. What existing features this touches

| Area | Interaction |
|---|---|
| **Shim (§6)** | Add best-effort phase timing + a node-local textfile write; no behavior change to launch. |
| **Provisioner (§5)** | Gains a metrics HTTP server + inventory/patch-age exporter (companion binary alongside `entrypoint.sh`). |
| **Operator/admission (§8)** | Custom collectors on the **existing** `:8080` metrics endpoint; increment denial counters in the webhook. |
| **Helm chart / values** | New opt-in `metrics` block; `Service`/`ServiceMonitor`; dashboard. |
| **Labels vocabulary** | Reuse `labels.go` keys for metric label consistency. |
| **JDK management (§ day-2)** | `brewlet_jdk_patch_age_days` operationalizes the "patch the node JDK, patch everything" story — a headline security metric ([security.md](security.md)). |
| **AppCDS** | Provide the archive-map metrics that make that accelerator's payoff observable. |
| **Docs** | Promote the "shim can export per-sandbox metrics *(roadmap)*" line in [observability.md](observability.md) to a configured feature. |

---

## 7. Recommendation & phasing

1. **Phase A — operator/admission metrics (cheapest).** Add custom collectors to
   the operator's existing metrics endpoint: reconcile stats, ready replicas,
   `brewlet_admission_denied_total{reason}`. No new component. Immediately useful
   for spotting `NoCompatibleJDK` storms and reconcile errors.
2. **Phase B — node exporter via the provisioner.** Node inventory + JDK patch age
   + readiness on a provisioner `/metrics` server; chart `Service`/`ServiceMonitor`;
   Grafana dashboard. Delivers the fleet + CVE-lever visibility.
3. **Phase C — per-sandbox launch metrics from the shim.** Best-effort phase timing
   + content-store hit/miss written node-local and aggregated by the Phase-B
   exporter. The AppCDS accelerator slice already landed — the shim emits
   `brewlet_cds_archive_mapped` textfiles ([appcds.md §4.3](appcds.md)); the Phase-B
   exporter still needs to aggregate them.

Scoping the exporter to *Brewlet-specific* runtime signals (not re-exporting app
JVM metrics) keeps it small, avoids overlap with the app's own observability, and
directly serves the operator questions Brewlet uniquely creates: *is the fleet
compatible, are cold starts healthy, and how stale is my centrally-patched JDK?*

---

## 8. References

- [Prometheus exposition / textfile collector](https://prometheus.io/docs/instrumenting/exposition_formats/),
  [Prometheus Operator ServiceMonitor](https://prometheus-operator.dev/docs/operator/design/#servicemonitor).
- [controller-runtime metrics](https://book.kubebuilder.io/reference/metrics.html)
  (the operator already serves `--metrics-bind-address`, default `:8080`).
- [containerd metrics](https://github.com/containerd/containerd/blob/main/docs/man/containerd-config.toml.5.md).
- Brewlet: [SPECIFICATION §12](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#12-networking-observability-day-2),
  [observability.md](observability.md), [security.md](security.md),
  [appcds.md](appcds.md);
  `shim/cmd/containerd-shim-brewlet-v2/service_linux.go`,
  `provisioner/entrypoint.sh`,
  `operator/internal/admission/webhook.go`,
  `operator/internal/brewlet/labels.go`.
