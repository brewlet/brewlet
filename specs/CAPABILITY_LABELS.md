# Brewlet capability labels

This document is the public reference for Brewlet's capability-label contract.
The node provisioner publishes these labels after it has installed and validated
the runtime inventory selected by a [`NodeProfile`](SPECIFICATION.md#56-node-profiles-per-pool-preparation).
The pod admission webhook then injects matching required node affinity for each
explicit workload request.

## Contract v1

| Node label | Meaning | Producer | Workload match |
|---|---|---|---|
| `brewlet.sh/runtime=ready` | The Brewlet shim, at least one JDK, and the containerd runtime registration are ready on the node. | Brewlet provisioner | `In: ["ready"]`, through the `brewlet` `RuntimeClass` |
| `brewlet.sh/jdk.<distribution>-<feature>` | The exact JDK root is installed. | Brewlet provisioner | `Exists` |
| `brewlet.sh/jdk-feature.<feature>` | At least one JDK distribution for the feature version is installed. | Brewlet provisioner | `Exists` |
| `brewlet.sh/launcher.<name>` | The launcher is available. The provisioner always publishes `brewlet.sh/launcher.java`; configured launcher layers add more keys. | Brewlet provisioner | `Exists` for non-`java` requests |
| `kubernetes.io/arch=<architecture>` | The kubelet-reported node architecture. Brewlet reuses this standard label rather than publishing an architecture label of its own. | Kubelet | `In: [...]` |

The dynamic JDK and launcher labels are **boolean-presence labels**. Brewlet
currently writes the value `true`, but the value is not part of the capability
test. Consumers MUST match these keys with Kubernetes `Operator: Exists` and
MUST NOT require `=true`. This permits Brewlet to add non-semantic diagnostic
values in a future compatible release without changing scheduling behavior.

`brewlet.sh/runtime` is not a boolean-presence label: only the exact value
`ready` means that the runtime may accept workloads. `kubernetes.io/arch` also
uses value matching because a workload requests one or more specific
architectures.

`brewlet.sh/provision` is a legacy activation label for the standalone
provisioner manifest, not a workload capability. Operator-managed installations
select nodes with `NodeProfile.spec.nodePool`; they do not require
`brewlet.sh/provision=true`.

## Token grammar

Capability keys are derived from the inventory declared by the platform team:

- `<distribution>` is the lowercase DNS-1123 label in
  `NodeProfile.spec.jdks[].distribution`: 1-48 characters, beginning and ending
  with an ASCII lowercase letter or digit, with lowercase letters, digits, and
  hyphens in between.
- `<feature>` is the canonical base-10 rendering of a positive 32-bit JDK
  feature version, without a sign or leading zeroes; for example, `17`, `21`, or
  `25`.
- `<distribution>-<feature>` is limited to 59 characters so that
  `jdk.<distribution>-<feature>` fits the 63-character Kubernetes label-name
  segment.
- `<name>` is a launcher identifier. To produce a valid capability label, names
  use lowercase DNS-1123 label syntax and are limited to 54 characters so that
  `launcher.<name>` fits the Kubernetes label-name segment. `java` is reserved
  for the built-in OpenJDK launcher. The current `NodeProfile` webhook does not
  validate launcher tokens, so an invalid name is accepted initially but causes
  node provisioning to fail when the provisioner publishes its label.
- `<architecture>` is a Kubernetes architecture token such as `amd64` or
  `arm64`.

For example, this profile:

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: batch
spec:
  nodePool:
    names: ["batch"]
    key: eks.amazonaws.com/nodegroup
  jdks:
    - distribution: temurin
      feature: 21
    - distribution: microsoft
      feature: 25
  launchers:
    - jaz
```

causes each successfully provisioned `batch` node to publish:

```text
brewlet.sh/runtime=ready
brewlet.sh/jdk.temurin-21=true
brewlet.sh/jdk-feature.21=true
brewlet.sh/jdk.microsoft-25=true
brewlet.sh/jdk-feature.25=true
brewlet.sh/launcher.java=true
brewlet.sh/launcher.jaz=true
```

The provider's kubelet separately publishes `kubernetes.io/arch`.

## Admission-injected affinity

`JavaApplication.spec.jvm.version`, `.distribution`, and `.launcher` become pod
annotations before pod admission. Equivalent raw pods can set the annotations
directly. For a distribution-agnostic JDK 21 request with the `jaz` launcher,
the Brewlet webhook injects the following requirements:

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

The `brewlet` `RuntimeClass` additionally contributes:

```yaml
nodeSelector:
  brewlet.sh/runtime: ready
```

An exact `temurin-21` request uses
`brewlet.sh/jdk.temurin-21 Exists` instead. A non-portable artifact requesting
`amd64` or `arm64` adds `kubernetes.io/arch In [...]`. A missing JDK request adds
no JDK affinity, and the built-in `java` launcher adds no launcher affinity
because every ready Brewlet node provides it.

When a pod already has required node affinity, Brewlet appends its requirements
to every existing node selector term. Expressions within a term remain ANDed
and terms remain ORed, so Brewlet preserves the pod author's existing selection
logic while requiring the requested runtime capabilities in every alternative.

Admission first verifies explicit requests against the current ready fleet. If
no ready node advertises the requested JDK, launcher, or architecture, the pod
is denied with `NoCompatibleJDK`, `NoCompatibleLauncher`, or
`NoCompatibleArch`. Consequently, a dynamically provisioned capability pool
must retain at least one ready node advertising the requested capability;
current Brewlet admission does not support waking a completely zero-sized
capability fleet.

## Autoscaler recipes

### Cluster Autoscaler with a NodeProfile-managed node group

Cluster Autoscaler must see the labels that a future node will eventually
publish when it simulates scheduling. Keep at least one node in the group for
Brewlet's admission check, and configure the group template with synthetic
labels for `runtime=ready` and every capability installed by the matching
`NodeProfile`.

For an EKS managed node group backed by Auto Scaling group `$ASG`, the Cluster
Autoscaler tag convention is:

```bash
aws autoscaling create-or-update-tags --tags \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/runtime,Value=ready,PropagateAtLaunch=false" \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/jdk.temurin-21,Value=true,PropagateAtLaunch=false" \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/jdk-feature.21,Value=true,PropagateAtLaunch=false" \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/launcher.java,Value=true,PropagateAtLaunch=false" \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/launcher.jaz,Value=true,PropagateAtLaunch=false"
```

These tags are scheduling hints to Cluster Autoscaler; with
`PropagateAtLaunch=false` they do not claim that an unprovisioned node is ready.
The EKS node joins with its normal
`eks.amazonaws.com/nodegroup=<group-name>` label, the matching `NodeProfile`
places a provisioner pod on it, and only that provisioner publishes the real
runtime and capability labels after installation succeeds.

The template labels MUST exactly match the profile inventory. A template that
advertises more capabilities than the profile installs can cause unnecessary
scale-ups, although the Kubernetes scheduler still waits for the real labels
before placing the workload.

### Karpenter

Karpenter copies `NodePool.spec.template.metadata.labels` onto real NodeClaims
and Nodes; unlike Cluster Autoscaler's synthetic template tags, they are not
simulation-only hints. Do not put `brewlet.sh/runtime=ready` or Brewlet
capability labels on a Karpenter `NodePool` whose nodes still require
post-registration `NodeProfile` provisioning. Doing so creates a window in
which the scheduler can place a Brewlet pod before the runtime exists.

`NodeProfile` can provision nodes that Karpenter creates for other workloads by
selecting the Karpenter pool:

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

This pool-targeting arrangement ensures every created node receives the declared
inventory, but it is not a capability-driven Karpenter autoscaling recipe.
Karpenter cannot infer labels that another DaemonSet will add after registration,
so a pod carrying Brewlet's injected affinity cannot select this unlabeled
`NodePool`.

Capability-driven Karpenter provisioning, including scale-from-zero, requires an
immutable node image or bootstrap process that installs and validates Brewlet
before kubelet registers the Node. Only in that model may the `NodePool`
template truthfully include the complete label set:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: brewlet-jdk21
spec:
  template:
    metadata:
      labels:
        brewlet.sh/runtime: ready
        brewlet.sh/jdk.temurin-21: "true"
        brewlet.sh/jdk-feature.21: "true"
        brewlet.sh/launcher.java: "true"
        brewlet.sh/launcher.jaz: "true"
    spec:
      requirements:
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64", "arm64"]
```

The image inventory and labels MUST move together. Publishing `runtime=ready`
from a Karpenter template while relying on the Brewlet provisioner to install
the runtime after node registration is unsupported. The current
`NodeProfile`-managed provisioner is therefore suitable for Karpenter nodes
created for other demand, but not as the bootstrap mechanism for a
capability-driven Karpenter `NodePool`.

## Compatibility and versioning

The labels in the contract-v1 table are public scheduling API:

- Brewlet may add new capability-key families or publish additional keys on a
  node without changing contract v1.
- Brewlet may add support for new distributions, feature versions, launchers,
  and architectures when their tokens follow the grammar above.
- Brewlet MUST NOT rename or remove a listed key family, change the meaning of
  `runtime=ready`, or change an `Exists` capability into value-sensitive
  matching within contract v1.
- A breaking change requires a new major capability-label contract, release
  notes, and a migration period in which old and new keys are published
  together so autoscaler templates and workload policies can move safely.
- Kubernetes-owned semantics for `kubernetes.io/arch` remain governed by
  Kubernetes. Brewlet's guarantee is that architecture requests continue to use
  that standard key rather than a Brewlet-specific replacement.

Node inventory annotations such as `brewlet.sh/jdks` and
`brewlet.sh/launchers` are used by Brewlet admission for current-fleet
validation. They are not substitutes for the scheduling labels and are not
part of this public autoscaler-template contract.
