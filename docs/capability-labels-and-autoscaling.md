# Capability labels and autoscaling

Brewlet publishes node labels that describe which runtime capabilities are
actually ready, then admission adds matching required node affinity to workloads.
This page explains how to connect that behavior to node pools and autoscalers.

!!! info "Canonical capability-label contract"

    The complete key catalog, token grammar, matching semantics, and compatibility
    guarantees are maintained in the
    [Brewlet capability-label contract](https://github.com/brewlet/brewlet/blob/main/specs/CAPABILITY_LABELS.md).
    Treat that document as normative; this page focuses on operator workflows.

Related: [Installation](installation.md) · [Configuration](configuration.md) ·
[JDK management](jdk-management.md).

---

## How capabilities reach the scheduler

1. A `NodeProfile` selects one or more node pools with `spec.nodePool` and
   declares the JDKs and launchers Brewlet must install.
2. The operator places the provisioner on matching nodes.
3. The provisioner installs and validates the inventory, registers the runtime,
   and only then publishes `brewlet.sh/runtime=ready` and the corresponding
   capability labels.
4. For an explicit workload request, admission verifies the current ready fleet
   and injects required affinity for the requested JDK, launcher, and
   architecture.

For example, this profile prepares an EKS managed node group:

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: jdk21
spec:
  nodePool:
    key: eks.amazonaws.com/nodegroup
    names: ["jdk21-workers"]
  jdks:
    - distribution: temurin
      feature: 21
  launchers:
    - jaz
```

After preparation succeeds, the node advertises the runtime and inventory:

```text
brewlet.sh/runtime=ready
brewlet.sh/jdk.temurin-21=true
brewlet.sh/jdk-feature.21=true
brewlet.sh/launcher.java=true
brewlet.sh/launcher.jaz=true
```

The capability-label values are not the scheduling contract. JDK and launcher
capabilities are matched by **key presence**, using `Operator: Exists`; do not
write policies that require `=true`. The exact `runtime=ready` value is
value-sensitive and is selected by the `brewlet` `RuntimeClass`.

`brewlet.sh/provision=true` is not required here. It is the opt-in label for the
legacy standalone provisioner manifest. Operator-managed installations select
nodes through `NodeProfile.spec.nodePool`.

---

## Admission-injected affinity

A distribution-agnostic JDK 21 request with the `jaz` launcher produces
requirements equivalent to:

```yaml
spec:
  runtimeClassName: brewlet
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - key: brewlet.sh/jdk-feature.21
                operator: Exists
              - key: brewlet.sh/launcher.jaz
                operator: Exists
```

The `brewlet` `RuntimeClass` additionally selects:

```yaml
nodeSelector:
  brewlet.sh/runtime: ready
```

An exact distribution request uses a key such as
`brewlet.sh/jdk.temurin-21`. A non-portable artifact also receives a
`kubernetes.io/arch In [...]` requirement. If the pod already has required node
affinity, Brewlet adds its requirements to every existing selector term so the
author's alternatives remain intact while every alternative still requires the
requested Brewlet capabilities.

Admission validates explicit requests against the **current ready fleet** before
the scheduler sees the pod. Keep at least one compatible node ready: current
Brewlet admission cannot use a request to wake a completely zero-sized
capability pool.

---

## Cluster Autoscaler

Cluster Autoscaler simulates whether a future node from a group could schedule a
pending pod. Its node-group template must therefore advertise the same runtime,
JDK, and launcher labels that the group's `NodeProfile` will install.

On EKS, add synthetic node-template labels to the backing Auto Scaling group:

```bash
aws autoscaling create-or-update-tags --tags \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/runtime,Value=ready,PropagateAtLaunch=false" \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/jdk.temurin-21,Value=true,PropagateAtLaunch=false" \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/jdk-feature.21,Value=true,PropagateAtLaunch=false" \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/launcher.java,Value=true,PropagateAtLaunch=false" \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/launcher.jaz,Value=true,PropagateAtLaunch=false"
```

These tags are simulation hints, not real node readiness claims.
`PropagateAtLaunch=false` prevents them from being copied onto a joining node.
The node joins with its provider pool label, the matching `NodeProfile` prepares
it, and the Brewlet provisioner publishes the real labels only after validation.

Keep the template labels synchronized with the `NodeProfile`. Advertising a
capability the profile does not install can trigger an unnecessary scale-up,
although the scheduler still waits for the real provisioner-owned labels before
placing the workload.

---

## Karpenter

Karpenter handles template labels differently: labels under
`NodePool.spec.template.metadata.labels` are copied onto real NodeClaims and
Nodes. Do **not** put `brewlet.sh/runtime=ready` or Brewlet capability labels on a
Karpenter `NodePool` when Brewlet will be installed after the node registers.
That creates a window where the scheduler can place a pod before the runtime is
ready.

A `NodeProfile` can safely target Karpenter nodes that are created for other
demand:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: brewlet-jdk21
spec:
  template:
    spec:
      requirements:
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64", "arm64"]
---
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: brewlet-jdk21
spec:
  nodePool:
    key: karpenter.sh/nodepool
    names: ["brewlet-jdk21"]
  jdks:
    - distribution: temurin
      feature: 21
  launchers:
    - jaz
```

This ensures created nodes receive the inventory, but it is not
capability-driven scale-from-zero: Karpenter cannot infer labels that another
DaemonSet will add after registration.

Making a Karpenter pool eligible for Brewlet capability requests requires an
immutable node image or a pre-registration bootstrap that installs and validates
Brewlet before kubelet registers the node. Only then may the Karpenter template
truthfully publish the complete ready label set:

```yaml
spec:
  template:
    metadata:
      labels:
        brewlet.sh/runtime: ready
        brewlet.sh/jdk.temurin-21: "true"
        brewlet.sh/jdk-feature.21: "true"
        brewlet.sh/launcher.java: "true"
        brewlet.sh/launcher.jaz: "true"
```

The image inventory and labels must move together. Publishing
`brewlet.sh/runtime=ready` before bootstrap has made that claim true is
unsupported.

---

## Choose an integration pattern

| Provisioning model | Autoscaler configuration | Scale-from-zero for capability requests |
|---|---|---|
| Cluster Autoscaler + `NodeProfile` | Synthetic node-template labels matching the profile; provisioner publishes the real labels | No; keep at least one compatible ready node for admission |
| Karpenter + post-registration `NodeProfile` | Select the Karpenter pool in `NodeProfile.spec.nodePool`; do not template Brewlet readiness labels | No; suitable for nodes created for other demand |
| Karpenter + pre-baked or pre-registration Brewlet bootstrap | Template the complete labels only after the image/bootstrap makes them true | Not end-to-end with a zero-sized pool while current admission requires a compatible ready node |

## Next steps

- **[Brewlet capability-label contract](https://github.com/brewlet/brewlet/blob/main/specs/CAPABILITY_LABELS.md)** —
  normative keys, grammar, matching semantics, and compatibility guarantees.
- **[Configuration](configuration.md)** — define `NodeProfile` inventories and
  pool selection.
- **[JDK management](jdk-management.md)** — install and validate JDK roots.
