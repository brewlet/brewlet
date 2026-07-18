# Configuration

Every knob Brewlet exposes, in one place: Helm chart values, node-provisioner
environment variables, operator/admission flags, the `RuntimeClass`, and how the
different layers relate. For *how* to install, see [Installation](installation.md);
for JDKs and launchers specifically, see [JDK management](jdk-management.md) and
[Launchers](launchers.md).

---

## Configuration layers (how they connect)

There is **one source of truth** for the JDK/launcher inventory: you set it once,
and it flows down.

```
Helm values (provisioner.jdks / .launchers)
        │  become operator flags
        ▼
Operator flags (--jdks / --launchers)
        │  flow into the DaemonSet container env
        ▼
Provisioner env (JDKS / LAUNCHERS)
        │  drive what gets installed on each node
        ▼
Node state: /opt/brewlet/jdks/<dist>-<feature>/ + labels/annotations
```

If you use Helm, set values. If you run the operator directly, set flags. If you
hand-wire the DaemonSet, set env vars. Don't mix — the operator overwrites the
DaemonSet it manages.

> **Note on defaults.** The "Default" columns below are per-layer: they apply only
> when that layer is invoked directly. The Helm chart ships richer defaults than the
> bare binaries (e.g. `provisioner.jdks=temurin-21,microsoft-25` and
> `provisioner.launchers=jaz`), and passes them down explicitly, so a direct
> operator/DaemonSet invocation without those flags/env falls back to the leaner
> binary defaults (`--jdks=temurin-21`, `--launchers`/`LAUNCHERS` empty).

---

## Helm chart values

From [`charts/brewlet/values.yaml`](https://github.com/brewlet/kubernetes/blob/main/charts/brewlet/values.yaml). Override
with `--set key=value` or a values file.

| Key | Default | Meaning |
|---|---|---|
| `namespace` | `brewlet` | Namespace all components install into (created by the chart). |
| `images.operator` | `ghcr.io/brewlet/operator:0.1.0` | Operator image. |
| `images.provisioner` | `ghcr.io/brewlet/node-provisioner:0.1.0` | Provisioner image the operator runs. |
| `images.admission` | `ghcr.io/brewlet/admission:0.1.0` | Admission webhook image. |
| `images.pullPolicy` | `IfNotPresent` | Image pull policy for all components. |
| `provisioner.jdks` | `temurin-21,microsoft-25` | Comma-separated `<dist>-<feature>` JDK roots to install on every opted-in node ([§JDK management](jdk-management.md)). |
| `provisioner.launchers` | `jaz` | Comma-separated launcher layers ([§Launchers](launchers.md)). Empty = vanilla `java` only. |
| `provisioner.rollout.maxUnavailable` | `null` | Bounds the default profile's provisioner DaemonSet rolling update. `null` keeps the DaemonSet default (proposal 0002). |
| `provisioner.rollout.validate` | `true` | Gate node readiness on the post-install JDK smoke test (`java -version` per root). Renders the provisioner `BREWLET_VALIDATE` env. |
| `provisioner.rollout.containerdRestart` | `validated` | When/whether to reload containerd after writing its config: `validated`, `sighup`, or `none`. Renders `BREWLET_CONTAINERD_RESTART` ([§5.5](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md)). |
| `provisioner.registry.mirrors` | `{}` | `<upstream-host>: <mirror-host>` map applied to every copy-from-image pull for air-gapped clusters. Renders `MIRRORS`. |
| `defaultProfile.enabled` | `true` | Render the chart-managed **default** `NodeProfile` from `provisioner.*`. Disable to manage the default profile yourself, e.g. via GitOps ([§5.6](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md)). |
| `profiles` | `[]` | Additional per-pool `NodeProfile` CRs, each binding node pool(s) to their own JDK/launcher inventory plus rollout/registry policy ([§5.6](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md)). |
| `operator.replicas` | `1` | Operator replica count. |
| `operator.leaderElect` | `true` | Enable leader election for HA. |
| `operator.resources` | requests `50m/64Mi`, limits `200m/128Mi` | Operator pod resources. |
| `admission.enabled` | `true` | Deploy the admission/scheduling webhook. Set `false` to skip it (the shim keeps its runtime JDK check). |
| `admission.replicas` | `1` | Webhook replica count. |
| `admission.failurePolicy` | `Ignore` | Webhook failure policy. `Ignore` ensures a webhook outage never blocks workloads. |
| `admission.port` | `9443` | Webhook server port. |
| `admission.resources` | requests `50m/64Mi`, limits `200m/128Mi` | Webhook pod resources. |

Example production install (own registry, no `jaz`):

```bash
helm install brewlet ./charts/brewlet \
  --set images.operator=registry.example.com/brewlet/operator@sha256:… \
  --set images.provisioner=registry.example.com/brewlet/node-provisioner@sha256:… \
  --set images.admission=registry.example.com/brewlet/admission@sha256:… \
  --set provisioner.jdks="temurin-21,temurin-25" \
  --set provisioner.launchers=""
```

> JDKs and launchers are always obtained **copy-from-image** (the vendor's
> official image, pulled through the host containerd). Mirror those images into
> your own registry for air-gapped clusters.

---

## Operator flags

From [`operator/README.md`](https://github.com/brewlet/kubernetes/blob/main/operator/README.md). When you install via
Helm, the chart populates these for you.

| Flag | Default | Meaning |
|---|---|---|
| `--namespace` | `brewlet` | Namespace the provisioner DaemonSet is managed in. |
| `--provisioner-image` | `ghcr.io/brewlet/node-provisioner:0.1.0` | Image the DaemonSet runs. |
| `--jdks` | `temurin-21` | Comma-separated `<dist>-<feature>` inventory (flows to the provisioner `JDKS` env). |
| `--launchers` | *(empty)* | Comma-separated launcher inventory (`LAUNCHERS` env). |
| `--leader-elect` | `false` | Enable leader election for HA. |
| `--metrics-bind-address` | `:8080` | Metrics endpoint. |
| `--health-probe-bind-address` | `:8081` | Health/readiness endpoint. |

```bash
./operator/bin/manager --namespace=brewlet \
  --provisioner-image=ghcr.io/brewlet/node-provisioner:0.1.0 \
  --jdks=temurin-21,microsoft-25 --launchers=jaz
```

---

## Node-provisioner environment variables

From [`provisioner/README.md`](https://github.com/brewlet/kubernetes/blob/main/provisioner/README.md). The operator sets
these on the DaemonSet it manages; you only touch them directly if you hand-wire the
DaemonSet.

| Env var | Default | Meaning |
|---|---|---|
| `JDKS` | `temurin-21` | Comma-separated `<distribution>-<feature>` roots to install. Curated distributions: `temurin`, `microsoft`. |
| `LAUNCHERS` | *(empty)* | Comma-separated launcher layers to stage (e.g. `jaz`). `java` is implicit. |
| `NODE_NAME` | (downward API) | The node to label; injected from `spec.nodeName`. |
| `BREWLET_PREFIX` | `/opt/brewlet` | Host install prefix (`bin/`, `jdks/`, `launchers/`). |
| `CONTAINERD_CONFIG` | `/etc/containerd/config.toml` | containerd config to patch. |
| `CONTAINERD_ADDRESS` | `/run/containerd/containerd.sock` | Host containerd socket (used for copy-from-image). |
| `CONTAINERD_NAMESPACE` | `k8s.io` | containerd namespace for image pulls. |
| `BREWLET_MODE` | `provision` | `provision` installs the runtime; `cleanup` reverses it (restores the containerd config backup, removes the shim, drops the runtime + capability labels) for a deleted `NodeProfile`. The operator sets it on the short-lived `brewlet-cleanup-<profile>` DaemonSet (§5.6). |
| `BREWLET_CONTAINERD_RESTART` | `validated` | When/whether to reload containerd after writing its config: `validated` (smoke-test the JDK roots first, then SIGHUP), `sighup` (SIGHUP unconditionally), or `none` (never signal; a rollout/human restarts it). Rendered from `spec.rollout.containerdRestart`. |
| `BREWLET_VALIDATE` | `true` | Run the post-install JDK smoke test (`java -version` per root) before flipping the node ready. `false` skips it. Rendered from `spec.rollout.validate`. |
| `MIRRORS` | *(empty)* | Comma-separated `<upstream-host>=<mirror-host>` pairs; every copy-from-image pull rewrites its registry host through this map for air-gapped clusters. Rendered from `spec.registry.mirrors`. |

---

## Admission webhook

The [`brewlet-admission`](https://github.com/brewlet/kubernetes/tree/main/operator/cmd/admission/) webhook is
mutating+validating. For every pod on CREATE with `runtimeClassName: brewlet` it:

- **stamps** `brewlet.sh/artifact-ref` (and `brewlet.sh/artifact-digest` when the
  ref is digest-pinned) so the shim can resolve the JAR from the content store;
- **matches** any requested JDK/launcher against the ready fleet, denying with
  `NoCompatibleJDK` / `NoCompatibleLauncher`;
- **steers** scheduling via `nodeAffinity` onto per-capability node labels.

Non-brewlet pods pass through untouched. With `admission.failurePolicy: Ignore`
(default) a webhook outage never blocks workloads.

**Serving certificate.** By default Helm generates a self-signed serving cert at
render time and injects the CA as the `caBundle`. Because Helm regenerates it on
each `helm upgrade`, the Secret and `caBundle` rotate together and a checksum
annotation rolls the webhook pods. **For production, swap in cert-manager** (a
`Certificate` + the `cert-manager.io/inject-ca-from` annotation).

Pod-side annotations the webhook reads (developer-facing) — see
[Deploying workloads](deploying-workloads.md):

| Annotation | Example | Meaning |
|---|---|---|
| `brewlet.sh/jdk` | `21` or `temurin-21` | Request a specific JDK feature (any distro) or exact `<dist>-<feature>`. |
| `brewlet.sh/launcher` | `jaz` | Request a launcher. Empty / `java` = vanilla OpenJDK launcher. |
| `brewlet.sh/artifact-container` | `app` | Which container's `image` is the OCI artifact (defaults to the brewlet container). |

---

## RuntimeClass

The operator manages the `brewlet` `RuntimeClass`; this is what it generates (mirrors
[`deploy/runtimeclass.yaml`](https://github.com/brewlet/kubernetes/blob/main/deploy/runtimeclass.yaml)):

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: brewlet
handler: brewlet                 # matches the containerd runtime name
scheduling:
  nodeSelector:
    brewlet.sh/runtime: "ready"  # only land on provisioned nodes
overhead:
  podFixed:
    memory: "64Mi"               # JVM/runtime baseline overhead accounting
    cpu: "50m"
```

`overhead.podFixed` is how the scheduler and `LimitRange`/quotas account for the
JVM/runtime baseline. Adjust it if your JVMs have a materially different fixed
footprint.

---

## Precedence & defaults you should know

- **JVM launch flags:** artifact structured knobs carry app-intrinsic correctness
  flags; deployment-descriptor `jvm.args` carries heap/GC/agent tuning and comes
  after those knobs. The descriptor's `jvm.launcher` / `brewlet.sh/launcher`
  selects `java` or a node-installed launcher. Brewlet injects **no** `-XX` flags
  itself. See [Resource tuning](resource-tuning.md).
- **JDK selection:** the deployment descriptor is authoritative:
  `spec.jvm.version` (plus optional `spec.jvm.distribution`) on `JavaApplication`,
  or `brewlet.sh/jdk` on raw pods, drives validation, scheduling, and shim launch
  selection. A bare feature matches any distribution; `<distribution>-<feature>`
  pins one.
- **cgroup v2 is mandatory** on nodes; the provisioner refuses cgroup v1-only nodes.
- **Digest-pinned artifact refs are recommended** (`repo@sha256:…`) so the shim can
  resolve straight from the content store and so supply-chain policy can apply.

## Next steps

- **[JDK management](jdk-management.md)** — the copy-from-image mechanics and the
  curated distribution → image matrix.
- **[Launchers](launchers.md)** — installing and choosing `jaz`.
- **[Deploying workloads](deploying-workloads.md)** — put these knobs to use.
