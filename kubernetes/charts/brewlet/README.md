# brewlet Helm chart

Brewlet's single-install activation combines roles split across SpinKube's
Runtime Class Manager and Spin Operator. `helm install` deploys:

- **brewlet-operator** — the node lifecycle controller (§8.1). It creates and
  reconciles the `brewlet-node-provisioner` DaemonSet and the `brewlet`
  RuntimeClass from the chart's `provisioner.*` values, so there is one runtime
  source of truth for the JDK/launcher inventory.
- **node-provisioner RBAC** — the `ServiceAccount` + `ClusterRole` the DaemonSet
  the operator creates runs as (it labels/annotates the nodes it provisions).
- **brewlet-admission** — the pod admission/scheduling webhook (§8/§14): it
  stamps `brewlet.sh/artifact-ref` + `brewlet.sh/artifact-digest` onto brewlet
  pods, matches a pod's requested JDK/launcher against the ready fleet
  (`NoCompatibleJDK` / `NoCompatibleLauncher` / `NoCompatibleArch`), and injects nodeAffinity so the
  scheduler only lands pods on capable nodes. Can be disabled with
  `--set admission.enabled=false`.

The provisioner DaemonSet and RuntimeClass themselves are **not** templated here
— the operator owns them at runtime (that is what §8.1 is for). The chart only
lays down the operator, the RBAC the operator's managed objects reference, and
the webhook.

## Install

```bash
helm upgrade --install brewlet oci://ghcr.io/brewlet/charts/brewlet \
  --version 0.1.0 \
  --namespace brewlet \
  --create-namespace \
  --set provisioner.jdks="temurin-21,microsoft-25" \
  --set provisioner.launchers="jaz"

# The default NodeProfile provisions EVERY node (§5.6) — no per-node opt-in step.
# The operator provisions each node; the provisioner marks it ready.
kubectl get nodes -L brewlet.sh/runtime
```

> Provisioning is privileged and mutates the host. See the
> [Brewlet specification](../../../specs/SPECIFICATION.md).
> The default profile targets **all** nodes. To scope it to platform-owned pools,
> set `defaultProfile.enabled=false` and define named `profiles` (§5.6).

## Values

| Key | Default | Meaning |
|-----|---------|---------|
| `namespace` | `brewlet` | Namespace all components install into (created by the chart). |
| `images.registry` | `ghcr.io/brewlet` | Registry prefix used for generated component image references. |
| `images.tag` | chart `appVersion` | Shared component tag. |
| `images.operator` | generated | Explicit operator image override. |
| `images.provisioner` | generated | Explicit provisioner image override. |
| `images.admission` | generated | Explicit admission image override. |
| `images.pullPolicy` | `IfNotPresent` | Image pull policy for all components. |
| `provisioner.jdks` | `temurin-21,microsoft-25` | Comma-separated `<dist>-<feature>` JDK roots to install (§5.3). |
| `provisioner.launchers` | `jaz` | Comma-separated launcher layers (§5.4). Empty = vanilla `java` only. |
| `operator.leaderElect` | `true` | Enable operator leader election. |
| `metrics.enabled` | `false` | Enable control-plane metrics listeners and the node exporter, and expose scrape Services/ports. |
| `metrics.nodePort` | `9090` | Port served by the exporter in each provisioner pod. |
| `metrics.serviceMonitor.enabled` | `false` | Create a Prometheus Operator `ServiceMonitor`. |
| `metrics.grafanaDashboard.enabled` | `false` | Create the starter Grafana dashboard ConfigMap. |
| `admission.enabled` | `true` | Deploy the admission/scheduling webhook. |
| `admission.failurePolicy` | `Ignore` | Webhook failure policy — `Ignore` never blocks workloads on a webhook outage. |
| `admission.port` | `9443` | Webhook server port. |

For a custom JDK distribution, use the structured inventory form:

```yaml
provisioner:
  jdks:
    - distribution: zulu
      feature: 21
      source:
        image: docker.io/library/azul-zulu:21
        javaHome: /usr/lib/jvm/zulu21
```

The string form remains supported for curated inventories. A custom image may
contain a full JDK or a centrally built jlink runtime, but must include the
userland libraries required by `javaHome/bin/java`. Pin custom images by digest
in production.

> **Upgrades:** Helm installs files under `crds/` only on first install and does
> not upgrade existing CRDs. Before upgrading an existing Brewlet release to a
> version that supports custom JDK sources, apply the matching CRD explicitly:
>
> ```bash
> kubectl apply -f kubernetes/deploy/nodeprofile-crd.yaml
> helm upgrade brewlet ./kubernetes/charts/brewlet -f values.yaml
> ```

## Requesting a JDK / launcher

A pod (or the raw Deployment) opts into a specific JDK/launcher via annotations,
which the webhook validates and turns into scheduling constraints:

```yaml
metadata:
  annotations:
    brewlet.sh/jdk: "21"          # bare feature (any distribution) or "temurin-21"
    brewlet.sh/launcher: "jaz"    # optional; omit/"java" for the vanilla launcher
spec:
  runtimeClassName: brewlet
  containers:
    - image: registry.example.com/demo/hello:1.0.0
```

If no ready node provides a compatible JDK/launcher, the pod is rejected with a
`NoCompatibleJDK` / `NoCompatibleLauncher` reason surfaced on the owning
controller (§14). With no annotation, the pod is admitted (ref/digest still
stamped) and the shim keeps its runtime compatibility check.

## Runtime metrics

Runtime metrics are disabled by default. Set `metrics.enabled=true` to run the
node exporter, enable the control-plane metrics listeners, and expose three
scrape surfaces:

- `brewlet-node-metrics` discovers the exporter in every profile-managed
  provisioner pod and reports sandbox launch phases/outcomes, artifact resolution,
  AppCDS decisions, and JDK/launcher inventory.
- `brewlet-operator-metrics` exposes controller-runtime metrics plus Brewlet
  NodeProfile readiness and provisioning transitions.
- `brewlet-admission` exposes webhook metrics on its `metrics` Service port,
  including admitted, denied, errored, and fail-open outcomes.

Enable optional integrations with:

```yaml
metrics:
  enabled: true
  serviceMonitor:
    enabled: true
    interval: 30s
  grafanaDashboard:
    enabled: true
```

The node exporter intentionally reports exact JDK build/source and installation
time rather than claiming the node installation age is the upstream patch age.
It also reports artifact resolution, not containerd pull-cache hits, because the
pull occurs before the shim is invoked.

## Serving certificate

The webhook's TLS serving cert is a self-signed cert generated by Helm
(`genSignedCert`) at render time; the CA is injected as the `caBundle` on the
`MutatingWebhookConfiguration`. Because Helm regenerates it on each
`helm upgrade`, the Secret and `caBundle` rotate together and a checksum
annotation rolls the webhook pods. For production, swap in cert-manager (a
`Certificate` + the `cert-manager.io/inject-ca-from` annotation) instead.
