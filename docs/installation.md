# Installation

This page enables Brewlet on a Kubernetes cluster: the operator, the node
provisioner, and the admission webhook. After this, any pod with
`runtimeClassName: brewlet` runs a Java application (packaged as an OCI artifact) directly on a node JDK.

There are two paths:

- **[Helm (recommended)](#helm-recommended)** — the KWasm-style single-command
  activation.
- **[Manual](#manual-without-helm)** — apply the raw manifests yourself.

> ⚠️ **Node provisioning is privileged and mutates the host** (installs a shim,
> JDK roots, and edits `/etc/containerd/config.toml`). Provision only nodes your
> platform team controls; on mixed clusters scope with named `NodeProfile`s (§5.6)
> rather than the all-nodes default profile. See [Security](security.md).

---

## Prerequisites

| Requirement | Why |
|---|---|
| Kubernetes with **containerd** as the CRI runtime | The shim is a containerd Runtime v2 shim. |
| **cgroup v2** on nodes | Brewlet requires it; the provisioner refuses cgroup v1-only nodes. |
| Nodes you control | Provisioning is privileged and host-mutating. |
| `kubectl` + `helm` (for the Helm path) | To install and manage node provisioning. |
| A reachable **OCI registry** | Where developers push OCI artifacts (and where component + vendor JDK images live). |
| Node access to **JDK images** | Vendor JDK/launcher images (Temurin, MS OpenJDK) pulled copy-from-image via the host containerd; mirror them for air-gapped clusters. |

### Component images

Brewlet ships three images. The defaults point at `ghcr.io/brewlet/*`; pin to your
own registry/digests in production. Build them from source with the
[Kubernetes repository Makefile](https://github.com/brewlet/kubernetes/blob/main/Makefile):

```bash
git clone https://github.com/brewlet/kubernetes.git
cd kubernetes
make operator-image-push      OPERATOR_IMAGE=<registry>/operator:<tag>
make provisioner-image-push   PROVISIONER_IMAGE=<registry>/node-provisioner:<tag>
make admission-image-push     ADMISSION_IMAGE=<registry>/admission:<tag>
```

`*-image-push` targets build multi-arch (`linux/amd64,linux/arm64`) via `buildx`
and require a logged-in registry. The provisioner image compiles the shim **inside**
the build for each target arch, so the installed shim always matches the node.

---

## Helm (recommended)

The [`charts/brewlet`](https://github.com/brewlet/kubernetes/tree/main/charts/brewlet) chart installs the operator, the
provisioner RBAC, and the admission webhook. The operator then creates and
reconciles the provisioner DaemonSet and the `brewlet` RuntimeClass from the chart's
values — so there is a single runtime source of truth for the JDK/launcher inventory.

```bash
cd kubernetes

helm install brewlet ./charts/brewlet \
  --set provisioner.jdks="temurin-21,microsoft-25" \
  --set provisioner.launchers="jaz"

# The chart renders a default NodeProfile that provisions EVERY node (§5.6) —
# there is no per-node opt-in step. The operator provisions each node and the
# provisioner marks it ready once the shim + JDK + runtime are installed. Watch:
kubectl get nodes -L brewlet.sh/runtime -w
```

> To limit provisioning to platform-owned pools instead of every node, disable the
> chart's default profile (`--set defaultProfile.enabled=false`) and define named
> `NodeProfile`s scoped to those pools — see [Configuration](configuration.md#helm-chart-values)
> (`profiles` / `defaultProfile`) and [SPECIFICATION §5.6](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md).

Point the chart at your own images if you built them:

```bash
helm install brewlet ./charts/brewlet \
  --set images.operator=<registry>/operator:<tag> \
  --set images.provisioner=<registry>/node-provisioner:<tag> \
  --set images.admission=<registry>/admission:<tag> \
  --set provisioner.jdks="temurin-21"
```

Every value is documented in [Configuration](configuration.md#helm-chart-values).
Lint / preview the rendered manifests before installing:

```bash
make helm-lint        # helm lint charts/brewlet
make helm-template    # render to stdout
```

### What the chart deploys vs. what the operator creates

| Deployed by the chart | Created/reconciled by the operator at runtime |
|---|---|
| `brewlet-operator` Deployment + RBAC | `brewlet-node-provisioner` DaemonSet |
| Node-provisioner `ServiceAccount` + `ClusterRole` | The `brewlet` `RuntimeClass` |
| `brewlet-admission` webhook + serving cert | (tracks node readiness, emits events) |

---

## Manual (without Helm)

If you'd rather not use Helm, apply the raw manifests and run the operator directly.

```bash
cd kubernetes

# 1. Namespace + provisioner ServiceAccount/RBAC (and, if you want to hand-wire it,
#    the provisioner DaemonSet):
kubectl apply -f deploy/node-provisioner.yaml

# 2. The operator ServiceAccount + RBAC + Deployment:
kubectl apply -f operator/config/operator.yaml

# 3. Opt nodes in. The standalone provisioner DaemonSet schedules onto nodes
#    carrying this LABEL — it drives nodeAffinity, so it must be a label, not an
#    annotation:
kubectl label node --all brewlet.sh/provision=true
```

You can also run the operator locally against your current kubeconfig (useful for
debugging), passing the same inventory the chart would set:

```bash
make operator-build
./operator/bin/manager \
  --namespace=brewlet \
  --provisioner-image=<registry>/node-provisioner:<tag> \
  --jdks=temurin-21,microsoft-25 \
  --launchers=jaz
```

The RuntimeClass and provisioner DaemonSet the operator generates mirror
[`deploy/runtimeclass.yaml`](https://github.com/brewlet/kubernetes/blob/main/deploy/runtimeclass.yaml) and
[`deploy/node-provisioner.yaml`](https://github.com/brewlet/kubernetes/blob/main/deploy/node-provisioner.yaml). All operator
and admission flags are in [Configuration](configuration.md#operator-flags).

> The operator itself does **not** need to be privileged — it only talks to the API
> server. The privileged, host-mutating work is done by the DaemonSet it manages.

---

## Verify the installation

```bash
# 1. Components are running:
kubectl get pods -n brewlet

# 2. Nodes are being provisioned → ready:
kubectl get nodes -L brewlet.sh/runtime
#   NAME     STATUS   RUNTIME
#   node-1   Ready    ready        ← provisioned

# 3. Inspect what a node advertises:
kubectl get node node-1 -o jsonpath='{.metadata.annotations.brewlet\.sh/jdks}{"\n"}'
#   temurin-21,microsoft-25
kubectl get node node-1 -o jsonpath='{.metadata.annotations.brewlet\.sh/launchers}{"\n"}'
#   java,jaz

# 4. The RuntimeClass exists:
kubectl get runtimeclass brewlet

# 5. The operator's view of each node:
kubectl get node node-1 -o jsonpath='{.metadata.annotations.brewlet\.sh/provision-state}{"\n"}'
#   Ready
```

Watch provisioning events if a node isn't going ready:

```bash
kubectl get events --field-selector reason=NodeReady
kubectl get events --field-selector reason=ProvisionFailed
```

See [Troubleshooting](troubleshooting.md) if a node stays `Provisioning`/`Failed`.

---

## Smoke test with a workload

```bash
kubectl apply -f deploy/raw-deployment.yaml
kubectl get pods -l app=hello -w
kubectl logs -l app=hello
```

For the full deploy story (raw Deployment, `JavaApplication` CRD, requesting a
specific JDK/launcher), see [Deploying workloads](deploying-workloads.md).

---

## Uninstall

```bash
helm uninstall brewlet
```

> Uninstalling deletes the control-plane components **and** the chart's `NodeProfile`
> objects. Each profile carries a `node.brewlet.sh/cleanup` finalizer, so the operator
> holds the object while a short-lived `brewlet-cleanup-<profile>` DaemonSet
> (`BREWLET_MODE=cleanup`) restores the `config.toml` backup, removes the shim + JDK
> roots, and drops the runtime + capability labels on every assigned node — reversing
> host state automatically before the object is garbage-collected (§5.6). Watch it with
> `kubectl get daemonset -n brewlet -w`. If a cluster was provisioned the older way (a
> bare `brewlet.sh/provision=true` node **label** with no profile), drain and clean those
> nodes (or replace them) to fully reverse provisioning.

## Next steps

- **[Configuration](configuration.md)** — tune every knob.
- **[JDK management](jdk-management.md)** — add/patch JDK roots (copy-from-image),
  go multi-arch.
- **[Launchers](launchers.md)** — install and use `jaz`.
