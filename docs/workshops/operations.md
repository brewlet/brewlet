# Part 1: Enable Brewlet on a Kubernetes cluster

**Audience:** Kubernetes platform engineers and cluster operators.

**Goal:** install Brewlet, provision an approved node JDK, verify the runtime,
and give application developers a small, explicit platform contract.

## 1. Prerequisites

You need:

- cluster-admin access to a disposable Kubernetes cluster;
- containerd nodes using cgroup v2;
- permission to run privileged DaemonSets and modify the node runtime;
- `kubectl`, Helm, and `curl`; and
- an OCI registry repository that Dev participants can push to and cluster
  nodes can pull from.

Confirm the active cluster before changing it:

```bash
kubectl config current-context
kubectl get nodes -o custom-columns=NAME:.metadata.name,RUNTIME:.status.nodeInfo.containerRuntimeVersion
kubectl auth can-i create customresourcedefinitions.apiextensions.k8s.io
```

Do not continue on a production or shared cluster unless its platform owner has
approved host-level Brewlet provisioning.

Set the Brewlet release and application registry used by both workshop parts:

```bash
export BREWLET_VERSION="0.1.0"
export BREWLET_REGISTRY="<registry-host>/<team>"
```

Authenticate with the registry using your organization's normal mechanism.

Install the released CLI. The installer detects the operating system and
architecture and verifies the release checksum:

```bash
curl -fsSL https://brewlet.sh/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
brewlet version
```

## 2. Preview the installation

Render and inspect the chart before applying it:

```bash
helm template brewlet oci://ghcr.io/brewlet/charts/brewlet \
  --version "$BREWLET_VERSION" \
  --namespace brewlet \
  --set-string provisioner.jdks=temurin-21 \
  > /tmp/brewlet-rendered.yaml
```

This workshop uses the default `NodeProfile`, which targets every node. For a
real shared cluster, disable it and create named profiles scoped to
platform-owned node pools.

## 3. Install Brewlet

```bash
helm upgrade --install brewlet oci://ghcr.io/brewlet/charts/brewlet \
  --version "$BREWLET_VERSION" \
  --namespace brewlet \
  --create-namespace \
  --set-string provisioner.jdks=temurin-21
```

The chart installs the operator and admission components. The operator creates
the `brewlet` RuntimeClass and a privileged provisioner DaemonSet that installs
the shim and JDK on each selected node.

## 4. Prepare the developer namespace

```bash
export BREWLET_CONTEXT="$(kubectl config current-context)"
export BREWLET_NAMESPACE="brewlet-workshop"
export BREWLET_JDK="21"

kubectl create namespace "$BREWLET_NAMESPACE" \
  --dry-run=client -o yaml | kubectl apply -f -
```

Apply your normal developer RBAC before the handoff. Registry credentials and
organization-specific RBAC remain part of the platform's existing security
model.

## 5. Wait for and diagnose the platform

```bash
kubectl rollout status deployment/brewlet-operator -n brewlet --timeout=5m
kubectl rollout status deployment/brewlet-admission -n brewlet --timeout=5m
kubectl rollout status daemonset -n brewlet \
  -l app=brewlet-node-provisioner --timeout=10m
kubectl get pods -n brewlet
kubectl get runtimeclass brewlet
kubectl get nodes -L brewlet.sh/runtime
brewlet doctor \
  --context "$BREWLET_CONTEXT" \
  --namespace "$BREWLET_NAMESPACE"
```

Every node selected by the profile must report `brewlet.sh/runtime=ready`.
Inspect the advertised runtime inventory:

```bash
kubectl get nodes -o custom-columns=\
NAME:.metadata.name,\
STATE:.metadata.annotations.brewlet\\.sh/provision-state,\
JDKS:.metadata.annotations.brewlet\\.sh/jdks,\
LAUNCHERS:.metadata.annotations.brewlet\\.sh/launchers
```

If a node does not become ready:

```bash
kubectl get events -A --sort-by=.lastTimestamp | tail -30
kubectl logs -n brewlet -l app=brewlet-node-provisioner \
  --all-containers --tail=100
```

Do not hand the cluster to developers until the RuntimeClass exists and at least
one schedulable node is ready.

## 6. Define the developer handoff

Give the developer:

| Value | Meaning |
|---|---|
| Kubernetes context | Cluster containing the Brewlet runtime |
| Namespace | Namespace where the developer may deploy |
| RuntimeClass | `brewlet` |
| Supported JDK | `21` in this workshop |
| Brewlet version | `0.1.0` |
| Registry prefix | Repository where the developer can push OCI images |
| Pull secret | Required only when the registry is private |

```bash
printf '%s\n' \
  "export BREWLET_CONTEXT=\"$BREWLET_CONTEXT\"" \
  "export BREWLET_NAMESPACE=\"$BREWLET_NAMESPACE\"" \
  "export BREWLET_JDK=\"$BREWLET_JDK\"" \
  "export BREWLET_VERSION=\"$BREWLET_VERSION\"" \
  "export BREWLET_REGISTRY=\"$BREWLET_REGISTRY\""
```

Continue with [Part 2: Build and deploy a workload](developers.md).

## 7. Optional platform exercises

- Configure named `NodeProfile`s for different node pools or JDK inventories.
- Add the `jaz` launcher.
- Mirror component and JDK images into an internal registry.
- Run `./integration-tests/e2e/run.sh --tier 13` on a disposable test cluster to
  exercise the complete `NodeProfile` lifecycle.

## Cleanup

Complete cleanup only after the Dev workshop:

```bash
helm uninstall brewlet -n brewlet
kubectl get nodeprofiles -w
```

Wait for profile cleanup finalizers to restore node state before deleting the
cluster. See [Installation](../installation.md) for production installation,
scoping, upgrades, and uninstall behavior.
