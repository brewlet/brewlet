# Proposal 0001 — Node profiles: per-pool cluster preparation

- **Status:** Implemented (Phases A–D)
- **Target spec sections:** new **§5.6** (Node profiles) + amendments to **§8.1**,
  **§8.3**, **§14** — landed.
- **Related code:** [`brewlet/kubernetes`](https://github.com/brewlet/kubernetes):
  `api/nodeprofile/v1alpha1`, `internal/controller/nodeprofile_*.go`,
  `internal/admission/nodeprofile_webhook.go`, the `internal/brewlet` vocabulary,
  `charts/brewlet` (CRD, `nodeprofiles.yaml`, profile values, and validating
  webhook), `deploy/nodeprofile-crd.yaml`, and
  `deploy/sample-nodeprofile.yaml`; [`brewlet/brewlet`](https://github.com/brewlet/brewlet):
  `provisioner/entrypoint.sh`
- **Related proposals (split out of the original draft):**
  - [0002 — Validated containerd reconfiguration & readiness smoke gate](0002-validated-node-reconfig.md)
    (consumed here via `rollout.containerdRestart` / `rollout.validate`).
  - [0003 — Stable capability-label taxonomy for autoscaling](0003-capability-label-taxonomy.md).
  - [0004 — cert-manager integration for the admission webhook](0004-cert-manager-admission.md).

> **Implemented.** The `NodeProfile` CRD (`node.brewlet.sh/v1alpha1`),
> `NodeProfileReconciler` (per-profile DaemonSet, catch-all default, pool-key
> auto-detection, finalizer-driven cleanup, registry mirrors), the validating
> webhook, the provisioner cleanup/mirror/restart modes, and the Helm surface are
> all in tree with unit + envtest coverage. The CRD/reconcile sketches below are
> the original design notes; the shipped API is authoritative in the code.

---

## 1. Summary

Make preparing a Kubernetes cluster for Brewlet **configurable for heterogeneous
production fleets** by introducing a cluster-scoped **`NodeProfile`** CRD. A profile
binds a **node pool** to a **JDK/launcher inventory** (plus rollout policy and an
optional registry override). The operator reconciles **one provisioner DaemonSet per
profile**, so GPU pools, arm64 pools, and team-specific JDK sets are first-class
instead of a single global string.

The unit of configuration is the **node pool**, not the individual node. The user says
"provision Brewlet on the `batch` pool"; every node in that pool — including nodes the
autoscaler adds later — is provisioned automatically. Selecting a pool **is** the opt-in,
so there is no separate per-node `kubectl annotate` step. Cloud node pools are disjoint
(a node belongs to exactly one), which removes the per-node ownership/overlap machinery
an arbitrary node-selector model would need.

The zero-config path is unchanged: `helm install … --set provisioner.jdks=temurin-21`
still works and simply generates a **default profile** that provisions every node pool
(the bare-metal / no-pool fallback).

This proposal is deliberately scoped to the CRD and its reconcile semantics. The
host-hardening work it depends on (validated restart, smoke gate) lives in
[0002](0002-validated-node-reconfig.md); the autoscaling label contract in
[0003](0003-capability-label-taxonomy.md); the webhook cert change in
[0004](0004-cert-manager-admission.md). Each can ship independently.

## 2. Motivation — where the current model gets hard in production

Today the inventory is a single global string:
`provisioner.jdks` / `provisioner.launchers` (Helm) → operator `Config.JDKs`/`Launchers`
→ one `buildDaemonSet` whose `nodeAffinity` is just `brewlet.sh/provision In [true]`
(see [`resources.go`](https://github.com/brewlet/kubernetes/blob/main/internal/controller/resources.go),
[`node_controller.go`](https://github.com/brewlet/kubernetes/blob/main/internal/controller/node_controller.go)).

| Gap | Today | Why it hurts in production |
|---|---|---|
| **One global inventory** | `provisioner.jdks` is one comma list applied to every `brewlet.sh/provision=true` node (`Config.JDKs` → one DaemonSet). | Real fleets are heterogeneous (pool, team, cost). You can't say "the batch pool gets `temurin-21`, the edge pool gets `microsoft-25`". |
| **Coarse, per-node opt-in** | `kubectl annotate node --all brewlet.sh/provision=true`. | Manual, node-by-node, and untied to any inventory. Worse, it **breaks under autoscaling**: cluster-autoscaler / Karpenter nodes join un-annotated and get nothing until a human re-annotates them. |
| **No reversal** | Uninstall removes control-plane objects; shim + JDK roots + `config.toml` edits are left on the host (see [installation.md](https://github.com/brewlet/site/blob/main/docs/installation.md) "Uninstall"). | Nodes accumulate drift; no clean way to de-provision without reimaging. |

The opt-in and autoscaling gaps have the same root cause: the unit of configuration is
the **individual node**. Cloud fleets are already organized into **node pools** (GKE
node pools, AKS agent pools, EKS managed node groups, Karpenter NodePools), and that is
the unit autoscalers add/remove capacity in. Targeting the pool instead of the node makes
opt-in declarative ("provision the `batch` pool") and autoscaler-safe (new pool members
are provisioned automatically).

> **Note on host mutation / readiness.** The original draft also listed "fragile host
> mutation" and "readiness ≠ validated" here. Those are accurate but **independent of
> the CRD** and are addressed in [0002](0002-validated-node-reconfig.md). (For the
> record: `entrypoint.sh` already runs `java -version` at install time and writes a
> `config.toml.brewlet.bak` backup — the gap is that nothing *restores* the backup on
> failure and the final `runtime=ready` label isn't gated on a re-run smoke test.)

Architecture is **not** a motivating gap for the inventory: the provisioner already
installs the JDK for the node's own arch automatically (`ctr` selects the matching
image platform), so a mixed amd64/arm64 fleet works today with one inventory. Profiles
add value where the *inventory itself* should differ per pool. Where a fleet organizes
arch into distinct pools (a common arm64 pattern), pinning a profile to that pool
already pins it to the arch.

## 3. How similar projects prepare nodes (prior art)

| Project | Node-prep mechanism | Idea we adopt |
|---|---|---|
| **KWasm / runtime-class-manager** (CNCF) | Operator + node-installer DaemonSet; a **`Shim` CRD** describes what to install. | CRD-per-capability, targeted by selector. |
| **SpinKube / spin-operator** | `SpinAppExecutor` CR selects runtime. | RuntimeClass + selection as first-class CRs. |
| **Kata `kata-deploy`** | DaemonSet installs artifacts; targets nodes by label; ships a **`kata-cleanup` DaemonSet**. | Per-capability targeting + an explicit **cleanup** path for reversal. |
| **NVIDIA GPU Operator** | A single **`ClusterPolicy`** CR drives the whole config; **validator pods** gate readiness. | One CR = whole config surface; validation gate. |
| **gVisor on GKE / Bottlerocket / Talos** | Shim baked into the **node image**; pick a "sandbox node pool". | First-class **immutable-image path** — no privileged host mutation. |
| **Cluster Autoscaler / Karpenter** | Scale node groups from pre-baked images on capability labels. | Stable capability-label taxonomy ([0003](0003-capability-label-taxonomy.md)). |

The common thread: **a declarative object per capability, targeted by node selector,
with a reversal path.** `NodeProfile` closes the remaining gap in Brewlet.

## 4. Goals / non-goals

**Goals**
- Per-pool JDK/launcher inventories via a cluster-scoped CRD.
- **Node-pool-level targeting**: name a pool, provision every node in it (present and
  future) — autoscaler-safe, no per-node annotation.
- Keep the one-command install trivially simple (default profile from Helm values).
- Deterministic behaviour when a node matches no pool profile.
- A reversal (cleanup) path driven by a finalizer.
- A per-profile hook for the label-only / immutable-image path.

**Non-goals**
- **Sub-pool (individual-node) targeting.** Pool is the smallest unit by design; "these
  3 nodes of 20" is explicitly out of scope. Split the pool if you need that boundary.
- Changing the artifact format, the shim, or the `JavaApplication` CRD.
- Auto-discovering which JDKs a workload needs (still platform-team-declared).
- Replacing the capability-label / admission mechanism — admission reads per-node
  `brewlet.sh/jdks` annotations and needs **no change** to work across heterogeneous
  pools (§8.3). We extend, not replace it.
- The validated-restart/smoke-gate host hardening ([0002](0002-validated-node-reconfig.md)),
  the label taxonomy ([0003](0003-capability-label-taxonomy.md)), and cert-manager
  ([0004](0004-cert-manager-admission.md)) — referenced, not defined here.

## 5. Design

### 5.1 The `NodeProfile` CRD (new group `node.brewlet.sh/v1alpha1`, cluster-scoped)

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: general-pool
spec:
  # Which node pool(s) this profile provisions. Selecting a pool IS the opt-in:
  # every node in the pool (now and as the autoscaler grows it) is provisioned.
  nodePool:
    # One or more pool names. On a recognized cloud the label KEY is auto-detected
    # (see below); `key` overrides it for non-standard / bare-metal clusters.
    names: [general, general-spot]
    # key: cloud.google.com/gke-nodepool   # optional explicit override
  # Declarative inventory (replaces the global JDKS/LAUNCHERS strings).
  jdks:
    - distribution: temurin
      feature: 21
    - distribution: zulu
      feature: 21
      source:
        image: docker.io/library/azul-zulu:21
        javaHome: /usr/lib/jvm/zulu21
  launchers:
    - jaz
  # Optional per-profile registry override for air-gapped / mirrored clusters.
  # NOTE: copy-from-image pulls via `ctr` against the host containerd, so mirroring
  # is expressed as a containerd registry mirror the provisioner configures for the
  # curated image hosts — NOT a naive host-swap and NOT a Kubernetes imagePullSecret
  # (which ctr does not consume). See §5.10.
  registry:
    mirrors:
      "mcr.microsoft.com":   registry.internal.example.com/mcr
      "docker.io":           registry.internal.example.com/dockerhub
  # Rollout policy for the managed DaemonSet + host reconfig. The reconfig/validate
  # behaviour itself is defined in proposal 0002.
  rollout:
    maxUnavailable: 1
    validate: true                 # 0002 smoke gate before marking ready
    containerdRestart: validated   # validated | sighup | none  (0002)
status:
  observedGeneration: 3
  resolvedPoolKey: cloud.google.com/gke-nodepool  # key the operator matched on
  assignedNodes: 12                # nodes in the selected pool(s)
  readyNodes: 12
  conditions:
    - type: Ready
      status: "True"
      reason: AllNodesProvisioned
```

**Pool identification (provider portability).** A "node pool" is a provider-standard
node label. The operator auto-detects the key by probing known ones on the fleet:

| Provider | Pool label key |
|---|---|
| GKE | `cloud.google.com/gke-nodepool` |
| AKS | `kubernetes.azure.com/agentpool` (legacy `agentpool`) |
| EKS (managed node groups) | `eks.amazonaws.com/nodegroup` |
| Karpenter | `karpenter.sh/nodepool` |

For bare-metal / kubeadm clusters that have no pool concept, set `spec.nodePool.key`
explicitly (e.g. to a hand-applied `brewlet.sh/pool` label), or omit `nodePool`
entirely to mean "every node" (the default-profile fallback, §5.3).

**Semantics**
- **Cluster-scoped** (nodes are cluster-scoped; profiles describe them).
- A node is provisioned if and only if it belongs to one of the profile's `nodePool.names`
  (matched on the resolved pool key). **Pool membership is the opt-in** — there is no
  separate `brewlet.sh/provision=true` gate.
- Cloud node pools are **disjoint**: a node belongs to exactly one pool, so at most one
  profile owns a node by construction. Misconfiguration (two profiles naming the same
  pool) is rejected by the webhook (§5.8), not resolved by priority.
- `spec.registry.mirrors` configures the provisioner's copy-from-image pulls for the
  curated hosts (§5.10).
- A custom source may be a complete vendor JDK or a jlink runtime containing a
  platform-approved module set. The provisioner copies the source image's complete
  root filesystem and records `source.javaHome`; the shim uses that root as the
  sandbox userland and exposes the declared Java home at `/opt/jdk`.
- Shared jlink runtimes are node inventory, not application payloads. Applications
  continue to publish JARs and select the inventory token
  `<distribution>-<feature>`.

### 5.2 Operator reconcile: one DaemonSet per profile, selected by pool label

Because pools are disjoint, ownership needs no computation: each profile's DaemonSet
targets its pool label directly, and no node can match two of them. There is no
per-node assignment label and no priority tie-break — the earlier draft's
`brewlet.sh/profile` stamping existed only to make *overlapping arbitrary selectors*
safe, which the pool model removes.

```
NodeProfileReconciler (watches NodeProfile + Node):
  1. ensureRuntimeClass()                      # cluster singleton, unchanged
  2. resolvePoolKey(profile)                    # auto-detect provider key or use spec override
  3. ds := buildProfileDaemonSet(cfg, profile)  # nodeAffinity =
        #   <poolKey> In [<names...>]
        #   (JDKS/LAUNCHERS/mirror env come from the profile)
     SetControllerReference(profile, ds)        # cluster owner, namespaced dep — OK
     CreateOrUpdate(ds)
  4. updateStatus(profile): assignedNodes / readyNodes / Ready|Degraded
     emit NodeUnmatched only if a named pool resolves to zero nodes
```

```go
// api/v1alpha1 (node.brewlet.sh) — illustrative draft.
type NodeProfileSpec struct {
    NodePool  NodePoolRef   `json:"nodePool,omitempty"`
    JDKs      []JDKRef      `json:"jdks"`
    Launchers []string      `json:"launchers,omitempty"`
    Registry  *RegistrySpec `json:"registry,omitempty"`
    Rollout   RolloutSpec   `json:"rollout,omitempty"`
}

type NodePoolRef struct {
    // Names of the pool(s) this profile provisions. Empty means "every node"
    // (bare-metal / default-profile fallback).
    Names []string `json:"names,omitempty"`
    // Key is the node label carrying the pool name. Empty => operator auto-detects
    // the provider key (gke-nodepool / agentpool / nodegroup / karpenter).
    Key string `json:"key,omitempty"`
}

type JDKRef struct {
    Distribution string `json:"distribution"`
    Feature      int32  `json:"feature"`
    Source       *JDKSource `json:"source,omitempty"`
}

type JDKSource struct {
    Image    string `json:"image"`
    JavaHome string `json:"javaHome"`
}

type RegistrySpec struct {
    // Mirrors maps a curated upstream host to a mirror host/path the provisioner
    // configures for `ctr` pulls. See §5.10.
    Mirrors map[string]string `json:"mirrors,omitempty"`
}

type RolloutSpec struct {
    MaxUnavailable    *intstr.IntOrString `json:"maxUnavailable,omitempty"`
    Validate          *bool               `json:"validate,omitempty"`          // default true (0002)
    ContainerdRestart string              `json:"containerdRestart,omitempty"` // validated|sighup|none (0002)
}
```

Key points:
- `buildProfileDaemonSet` is the existing `buildDaemonSet` generalized: the pod
  `nodeAffinity` becomes `<resolvedPoolKey> In [<names...>]`, and
  `JDKS`/`LAUNCHERS`/mirror env come from the profile. Pool disjointness is what
  enforces single ownership — no assignment label required.
- The DaemonSet is **owned by the profile** (controller ref). A cluster-scoped owner
  with a namespaced dependent is a valid ownerReference, so deleting the profile GCs
  its DaemonSet — but see §5.7: host cleanup must run *before* that GC, via a finalizer.
- `NodeReconciler` (§8.1) is unchanged in spirit: it still reflects per-node
  `provision-state` and emits `NodeReady` / `ProvisionFailed`. Under 0002 it also reads
  `brewlet.sh/provision-error` so a validation failure surfaces as the node's (and the
  owning profile's) failure reason.

### 5.3 Backward compatibility — the default profile

`provisioner.jdks` / `provisioner.launchers` do not go away. The chart renders a single
**default `NodeProfile`** from them:

```yaml
# templates/default-nodeprofile.yaml (rendered when provisioner.jdks is set)
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: default
spec:
  nodePool: {}                     # no names => every node (bare-metal fallback)
  jdks: {{ .Values.provisioner.jdks | toNodeProfileJDKs }}
  launchers: {{ .Values.provisioner.launchers | splitList "," }}
```

The current one-liner install produces identical behavior: an empty `nodePool` means
"every node," matching today's global-inventory semantics. When a user later adds an
explicit pool-scoped profile, they narrow the default (or delete it) — because pools are
disjoint there is no priority to reason about; a node is either in a named pool or falls
through to the empty-pool default. The operator's `--jdks/--launchers` flags become the
seed for this default profile and are deprecated in favor of the CRD over one minor
release.

### 5.4 Membership, the catch-all default, and unmatched pools

- **No selector overlap to resolve.** Cloud pools are disjoint, so two *named-pool*
  profiles cannot both claim a node. The webhook (§5.8) rejects two profiles naming the
  same pool, turning what was a runtime tie-break into an up-front validation error.
- **The one precedence rule — catch-all fallthrough.** The only profile that can
  co-select a node with a named-pool profile is the empty-`nodePool` **default**
  (which nominally means "every node"). We define the default as a *catch-all*: it owns
  exactly the nodes **not** claimed by any named-pool profile. This is binary
  ("named pool beats catch-all"), not a numeric priority, and it is the whole of the
  precedence logic. Concretely, the default DaemonSet's nodeAffinity gains a
  `NotIn [<all named pools>]` term on the resolved pool key.
- **Reassignment:** if a node moves pools (rare — usually means the node was recycled),
  or a profile's `nodePool.names` changes, the operator runs cleanup for the *old*
  inventory before applying the new one (§5.7), so the node never accumulates roots from
  a pool that no longer owns it.
- **Unmatched / empty pool:** a `nodePool.name` that resolves to zero nodes (typo, pool
  not yet created, wrong provider key) is surfaced with a `NodeUnmatched` warning event
  and a `Degraded`/`reason: EmptyPool` condition, so a misnamed pool is diagnosable
  rather than silently doing nothing. Bare-metal installs that keep the empty-pool
  default never hit this.

### 5.5 Failure feedback: operator ↔ provisioner

The provisioner self-labels the node; the operator can't observe a bash-level failure
directly. Per [0002](0002-validated-node-reconfig.md) the provisioner writes
`brewlet.sh/provision-error=<reason>` on the node (and withholds `runtime=ready`) on any
reconfig/validation failure. `NodeProfileReconciler` reads it when computing
`readyNodes` and flips the owning profile's `Ready` condition to `Degraded` with that
reason, so a bad rollout is visible at `kubectl get nodeprofile`.

### 5.6 Label-only / immutable-image mode

For regulated / immutable-OS clusters, a profile with `rollout.containerdRestart: none`
(defined in [0002](0002-validated-node-reconfig.md)) runs a **label-only** provisioner:
it skips host mutation entirely (shim + JDK roots + `config.toml` are baked into the
node image), runs the smoke gate, and only advertises capability labels +
`runtime=ready`. The mechanics of baking the runtime into a Talos/Bottlerocket/AMI node
image are a how-to for the docs, not part of this design; the CRD contribution is
simply that the mode is selectable per profile.

### 5.7 Reversal — cleanup via a finalizer

Owner-ref GC alone would delete a profile's DaemonSet the instant the profile is
deleted, leaving nothing to clean host state (and racing the cleanup). So `NodeProfile`
carries a **finalizer** (`node.brewlet.sh/cleanup`):

1. On profile deletion (or pool reassignment), the operator launches a short-lived
   **`brewlet-cleanup`** DaemonSet scoped to the affected nodes (selected on the profile's
   resolved pool label). Following the kata-cleanup pattern it: removes the
   `runtimes.brewlet` block from `config.toml` (restoring the backup), restarts
   containerd, removes the shim from `/usr/local/bin`, optionally prunes
   `/opt/brewlet/jdks/*` not referenced by another profile, and removes the
   `brewlet.sh/runtime` + capability labels.
2. Only once cleanup completes does the operator remove the finalizer, letting owner-ref
   GC drop the managed DaemonSet.

This makes `helm uninstall` + `kubectl delete nodeprofile --all` fully reverse host
state, closing the "uninstall leaves drift" gap.

### 5.8 Validating webhook for `NodeProfile`

A small validating admission webhook rejects malformed profiles early: empty
`jdks`, invalid distribution identifiers, custom distributions without a fully
qualified image and clean absolute Java-home path, curated distributions with a
source override, invalid
`containerdRestart` values, and **two profiles naming the same pool** (ambiguous
ownership — the pool-model replacement for the old priority tie-break). It also flags a
`nodePool.key` that matches no known provider on a cluster where auto-detection found a
different one. This keeps the reconcile loop from having to defend against garbage and
gives users immediate feedback.

### 5.9 Status & events

- **Status:** `observedGeneration`, `resolvedPoolKey`, `assignedNodes`, `readyNodes`, and
  a `conditions` list with `Ready` (`AllNodesProvisioned`) / `Degraded` (empty pool,
  validation, or reconfig failure — reason propagated from `brewlet.sh/provision-error`).
- **Events (extend §14):** `NodeUnmatched` (a named pool resolved to zero nodes),
  plus the existing `Provisioning` / `NodeReady` / `ProvisionFailed`. The reconfig/
  validation events (`ContainerdReconfigFailed`, `ValidationFailed`) are owned by
  [0002](0002-validated-node-reconfig.md).

### 5.10 Registry override for air-gapped copy-from-image

Copy-from-image pulls the curated vendor images via `ctr` against the **host
containerd** (`entrypoint.sh`: `mcr.microsoft.com/openjdk/jdk:<f>-ubuntu`,
`docker.io/library/eclipse-temurin:<f>`). Air-gap therefore isn't a simple host-swap of
one env var, and a Kubernetes `imagePullSecret` doesn't apply (ctr doesn't read it).
`spec.registry.mirrors` maps each curated upstream host to a mirror, and the provisioner
rewrites the pull refs accordingly (equivalently, configures containerd
`hosts.toml`-style mirrors for those hosts). Auth to the mirror is a node/containerd
concern (host `hosts.toml` credentials), documented in the air-gap guide rather than
carried in the CRD.

## 6. Helm surface (progressive disclosure)

```yaml
# Simple (unchanged): one inventory for every node -> default NodeProfile.
provisioner:
  jdks: "temurin-21"
  launchers: "jaz"

# Advanced: per-pool profiles. Pool label key is auto-detected per provider;
# override with `key` on bare-metal / non-standard clusters.
profiles:
  - name: general
    nodePool: { names: [general] }
    jdks: [ { distribution: temurin, feature: 21 } ]
  - name: edge-arm
    nodePool: { names: [edge-arm64] }
    jdks: [ { distribution: microsoft, feature: 25 } ]
    registry: { mirrors: { "mcr.microsoft.com": "registry.internal/mcr" } }
```

## 7. RBAC & CRD lifecycle

- **CRD:** add the `nodeprofiles.node.brewlet.sh` CRD to `charts/brewlet/crds/` and
  `deploy/` in [`brewlet/kubernetes`](https://github.com/brewlet/kubernetes).
  **Helm does not upgrade CRDs placed in `crds/`** — the rollout plan
  must document applying CRD updates out-of-band (or moving CRDs to a templated,
  `helm.sh/hook: crd-install`-style path) so `helm upgrade` picks up schema changes.
- **Operator RBAC:** add `get/list/watch/update/patch` on `nodeprofiles` (+ `/status`,
  `/finalizers`), `get/list/watch` on Nodes (to read pool labels and readiness — no node
  writes are needed since the model no longer stamps a per-node assignment label), and
  `create/update/delete` on DaemonSets — the operator now manages **N** DaemonSets, not one.
- **Validating webhook RBAC/config:** the §5.8 webhook configuration + its serving cert
  (self-signed today, or via [0004](0004-cert-manager-admission.md)).

## 8. Test plan

The implementation is end-to-end-test driven. Additions:

- **Unit (operator):** provider pool-key auto-detection (GKE/AKS/EKS/Karpenter +
  explicit-key override), catch-all default `NotIn` exclusion of named pools, empty-pool
  `NodeUnmatched` handling, per-profile DaemonSet nodeAffinity (pool label), finalizer
  add/remove ordering, status/condition transitions.
- **Envtest:** `NodeProfileReconciler` against a fake fleet with labeled pools — create a
  named-pool profile plus the catch-all default, assert each node is owned by exactly one
  DaemonSet (named pool excluded from the default); delete a profile and assert the
  finalizer blocks GC until cleanup completes.
- **Webhook unit:** rejects missing or malformed custom JDK sources / empty jdks /
  two profiles naming the same pool / unknown pool key.
- **e2e tier:** a new tier proving two pool profiles materialize different inventories on
  their pools, that a node the autoscaler adds to a pool is provisioned without manual
  annotation, and that `kubectl delete nodeprofile` + cleanup reverses host state
  (extends the existing provisioning tiers).

## 9. Rollout plan (phased, non-breaking)

1. **Phase A — CRD + default profile.** Add `node.brewlet.sh/v1alpha1` `NodeProfile`,
   `NodeProfileReconciler` (pool-key detection + one DaemonSet per profile, catch-all
   default), the validating webhook, RBAC/CRD lifecycle, and the chart-rendered
   `default` profile. Existing installs behave identically; `--jdks/--launchers` still
   work (seed the default profile).
2. **Phase B — reversal.** Finalizer + `brewlet-cleanup` DaemonSet; pool-reassignment
   cleanup.
3. **Phase C — air-gap registry mirrors + label-only mode wiring** (mode itself from
   0002).
4. **Phase D — docs + deprecate the global-string flags.**

Host hardening (validated restart + smoke gate) and cert-manager land on their own
tracks via [0002](0002-validated-node-reconfig.md) / [0004](0004-cert-manager-admission.md).

## 10. Risks & mitigations

- **Two ways to configure (values vs. profiles).** Default-profile bridge + a single
  "simple vs. advanced" story; deprecate the flags over one minor release.
- **Ownership ambiguity.** Removed structurally: pools are disjoint, so a node belongs to
  exactly one named pool; the catch-all default is excluded from named pools via a
  `NotIn` term (§5.4). Duplicate pool names are rejected by the webhook, not raced at
  runtime.
- **Provider portability of the pool key.** Auto-detect known keys (GKE/AKS/EKS/
  Karpenter) with an explicit `spec.nodePool.key` escape hatch for bare-metal/kubeadm,
  where an empty `nodePool` still means "every node."
- **Cleanup vs. owner-ref GC race.** Finalizer runs host cleanup before GC (§5.7).
- **CRD adds API surface.** Justified: it is the mechanism NVIDIA GPU Operator /
  runtime-class-manager use for the same heterogeneous-fleet problem.

## 11. Open questions

- Should `NodeProfile` also carry **taints/tolerations** so a pool can be reserved for
  Brewlet workloads, or leave that to the platform team? (The RuntimeClass stays a
  global singleton keyed on `runtime=ready`; profiles don't currently constrain
  scheduling beyond capability labels.)
- Should the default profile be **implicit** (operator-synthesized when no profile
  exists) rather than chart-rendered, so raw-manifest installs also get the catch-all
  "every node" behavior without shipping a YAML?
- For clusters with **no pool labels at all** (hand-rolled kubeadm), is the empty-pool
  "every node" default enough, or should we also support a hand-applied `brewlet.sh/pool`
  label as the documented bare-metal pooling convention?
- Per-profile **JDK GC policy** (retire roots not referenced by any ready pod after N
  days), or keep roots purely additive as today (§5.3)?

---

## Appendix — spec integration points

- **§5.6 (new):** "Node profiles" — the CRD, node-pool targeting, disjoint-pool single
  ownership + catch-all default, empty-pool handling, finalizer cleanup, label-only
  mode, air-gap registry mirrors, and the validating webhook.
- **§8.1:** add `NodeProfileReconciler` alongside `NodeReconciler`; note the DaemonSet
  is now per-profile, owned by the profile, and selected via the resolved node-pool label.
- **§14:** add the `NodeUnmatched` event (reconfig/validation events are owned by 0002).
