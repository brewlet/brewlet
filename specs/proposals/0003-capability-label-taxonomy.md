# Proposal 0003 — Stable capability-label taxonomy for autoscaling

- **Status:** Draft
- **Target spec sections:** amend **§5.2**, **§8.3** (document the labels as a public,
  versioned contract)
- **Related code:** [`brewlet/kubernetes`](https://github.com/brewlet/kubernetes):
  `internal/brewlet/labels.go`; [`brewlet/brewlet`](https://github.com/brewlet/brewlet):
  `provisioner/entrypoint.sh` (`label_node`)
- **Split from:** the original [0001 (node profiles)](0001-node-profiles.md) draft (the
  autoscaling-label contract 0001 depends on, §3 / §10). This is a
  documentation/stability commitment, not a mechanism change; it stands on its own and
  makes 0001's heterogeneous fleets autoscaler-friendly.

> Enhancement proposal (KEP-style). The labels already exist; this proposal promotes
> them from an implementation detail to a supported public taxonomy.

---

## 1. Summary

The provisioner already emits per-capability node labels that the admission webhook
matches `nodeAffinity` against (`labels.go`, `label_node`):

| Label | Meaning | Example |
|---|---|---|
| `brewlet.sh/jdk.<dist>-<feature>` | exact JDK root installed | `brewlet.sh/jdk.temurin-21=true` |
| `brewlet.sh/jdk-feature.<feature>` | some JDK of that feature (distribution-agnostic) | `brewlet.sh/jdk-feature.21=true` |
| `brewlet.sh/launcher.<name>` | launcher layer installed (incl. built-in `java`) | `brewlet.sh/launcher.jaz=true` |
| `brewlet.sh/runtime=ready` | shim + JDK installed, runtime registered | — |
| `kubernetes.io/arch` (reused) | node architecture for non-portable artifacts | `amd64` |

This proposal **documents them as a stable, versioned taxonomy** so that:

1. **Cluster Autoscaler / Karpenter** can be configured to grow JDK-capable node
   groups keyed on these labels (e.g. a NodePool that provisions nodes labelled for
   `brewlet.sh/jdk-feature.21`), and
2. the admission webhook's `nodeAffinity` injection (§8.3) is guaranteed stable across
   Brewlet versions.

## 2. Motivation

Autoscalers scale node groups by matching pod scheduling constraints (nodeAffinity /
nodeSelector) to node-group labels. Brewlet already injects that nodeAffinity, but the
label keys are currently only described in code comments and the SPECIFICATION's prose,
with no stability promise. Anyone wiring an autoscaler against them is depending on an
undocumented contract that could change.

## 3. Design

Purely documentation + a stability guarantee — no code change required:

- Add a **"Capability labels"** reference section to the docs (jdk-management /
  reference) enumerating the taxonomy above, its value semantics (boolean-presence,
  matched with `Operator: Exists`), and the `<dist>`/`<feature>`/`<name>` token grammar.
- Declare the keys **stable within the `brewlet.sh/` v1 label namespace**: additions
  are allowed; renames/removals are breaking changes gated on a major version.
- Document an **autoscaler recipe**: a Karpenter `NodePool` / Cluster Autoscaler node
  group that carries `brewlet.sh/provision=true` on new nodes so the operator
  provisions them, and the capability labels the workloads request.
- Cross-reference from §8.3 (the webhook injects affinity on exactly these keys).

## 4. Non-goals

- No new labels or renames. If profiles (0001) later add a `brewlet.sh/profile`
  assignment label, that is documented there, not here.
- No auto-discovery of what JDKs a workload needs (still platform-team-declared).

## Appendix — spec integration points

- **§5.2:** point the "emits per-capability scheduling labels" sentence at the new
  taxonomy reference.
- **§8.3:** note the injected `nodeAffinity` keys are the stable taxonomy defined here.
