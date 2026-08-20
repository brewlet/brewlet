# Part 1: Enable Brewlet on a Kubernetes cluster

**Audience:** Kubernetes platform engineers and cluster operators.

**Goal:** install Brewlet, provision an approved node JDK, verify the runtime,
and give application developers a small, explicit platform contract.

## 1. Prerequisites

You need:

- cluster-admin access to a disposable Kubernetes cluster;
- containerd nodes using cgroup v2;
- permission to run privileged DaemonSets and modify the node runtime;
- `kubectl`, Helm, Docker with `buildx`, and an OCI registry repository that
  workshop participants can push to and cluster nodes can pull from; and
- a clone of the Brewlet monorepo.

Confirm the active cluster before changing it:

```bash
kubectl config current-context
kubectl get nodes -o custom-columns=NAME:.metadata.name,RUNTIME:.status.nodeInfo.containerRuntimeVersion
kubectl auth can-i create customresourcedefinitions.apiextensions.k8s.io
```

Do not continue on a production or shared cluster unless its platform owner has
approved host-level Brewlet provisioning.

Set the registry location used by both workshop parts:

```bash
export BREWLET_REGISTRY="<registry-host>/<team>"
export BREWLET_TAG="workshop"
```

Authenticate with the registry using your organization's normal mechanism.

## 2. Build and publish the platform components

Publish multi-architecture operator, admission, and node-provisioner images:

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
  -t "$BREWLET_REGISTRY/brewlet-operator:$BREWLET_TAG" \
  --push kubernetes

docker buildx build --platform linux/amd64,linux/arm64 \
  --build-arg CMD=admission \
  -t "$BREWLET_REGISTRY/brewlet-admission:$BREWLET_TAG" \
  --push kubernetes

make provisioner-image-push \
  PROVISIONER_IMAGE="$BREWLET_REGISTRY/brewlet-node-provisioner:$BREWLET_TAG"
```

The component repositories must be readable by cluster nodes. Production
installations should use immutable tags or digests instead of `workshop`.

## 3. Preview the installation

Render and inspect the chart before applying it:

```bash
helm lint kubernetes/charts/brewlet
helm template brewlet kubernetes/charts/brewlet \
  --namespace brewlet \
  --set images.operator="$BREWLET_REGISTRY/brewlet-operator:$BREWLET_TAG" \
  --set images.admission="$BREWLET_REGISTRY/brewlet-admission:$BREWLET_TAG" \
  --set images.provisioner="$BREWLET_REGISTRY/brewlet-node-provisioner:$BREWLET_TAG" \
  --set-string provisioner.jdks=temurin-21 \
  > /tmp/brewlet-rendered.yaml
```

This workshop uses the default `NodeProfile`, which targets every node. For a
real shared cluster, disable it and create named profiles scoped to
platform-owned node pools.

## 4. Install Brewlet

```bash
helm upgrade --install brewlet kubernetes/charts/brewlet \
  --namespace brewlet \
  --create-namespace \
  --set images.operator="$BREWLET_REGISTRY/brewlet-operator:$BREWLET_TAG" \
  --set images.admission="$BREWLET_REGISTRY/brewlet-admission:$BREWLET_TAG" \
  --set images.provisioner="$BREWLET_REGISTRY/brewlet-node-provisioner:$BREWLET_TAG" \
  --set-string provisioner.jdks=temurin-21
```

The chart installs the operator and admission components. The operator creates
the `brewlet` RuntimeClass and a privileged provisioner DaemonSet that installs
the shim and JDK on each selected node.

## 5. Wait for the platform

```bash
kubectl rollout status deployment/brewlet-operator -n brewlet --timeout=5m
kubectl rollout status deployment/brewlet-admission -n brewlet --timeout=5m
kubectl rollout status daemonset -n brewlet \
  -l app=brewlet-node-provisioner --timeout=10m
kubectl get pods -n brewlet
kubectl get runtimeclass brewlet
kubectl get nodes -L brewlet.sh/runtime
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

Record these values for the Dev workshop:

```bash
export BREWLET_CONTEXT="$(kubectl config current-context)"
export BREWLET_NAMESPACE="brewlet-workshop"
export BREWLET_JDK="21"
```

Give the developer:

| Value | Meaning |
|---|---|
| Kubernetes context | Cluster containing the Brewlet runtime |
| Namespace | Namespace where the developer may deploy |
| RuntimeClass | `brewlet` |
| Supported JDK | `21` in this workshop |
| Registry prefix | Repository where the developer can push OCI images |
| Pull secret | Required only when the registry is private |

Create the namespace and grant your normal developer RBAC before the handoff:

```bash
kubectl create namespace "$BREWLET_NAMESPACE"
```

Registry credentials and organization-specific RBAC are intentionally left to
the platform's existing security model.

Continue with [Part 2: Build and deploy a workload](WORKSHOP-DEV.md).

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
cluster. See [Installation](docs/installation.md) for production installation,
scoping, upgrades, and uninstall behavior.
