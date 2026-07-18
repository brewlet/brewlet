# Multi-arch fleets & non-portable JARs — design & implementation

> **Status.** Multi-arch operations are *largely real*. **Phase A (the optional
> `arch` constraint for non-portable JARs) is now implemented** — launch config +
> CRD `arch` field, `kubernetes.io/arch` nodeAffinity injection with a
> `NoCompatibleArch` denial, and CLI/Maven auto-detection of bundled natives. See
> [jdk-management → architecture mapping](jdk-management.md#architecture-mapping-multi-arch)
> and [observability → multi-arch fleets](observability.md#day2-multi-arch-fleets).
> This note documents what is delivered, the remaining gaps, and — the real research
> content — how to handle JARs that are **not** architecture-neutral. Phase B
> (coverage observability + accelerator guardrails) remains *intended* behavior.

---

## 1. TL;DR

- **Multi-arch is Brewlet's model working *for* you.** A JAR is JVM bytecode —
  **architecture-neutral**. The JDK root is installed per node architecture
  (`amd64`/`arm64`), so the *same* Brewlet artifact runs unchanged on any
  provisioned arch. Container images need a multi-arch manifest list per image;
  Brewlet gets cross-arch portability *for free* for pure-bytecode apps. This is a
  genuine advantage worth stating loudly.
- **Most of it is already implemented / documented.** The provisioner's
  copy-from-image installs the node-arch JDK automatically (`ctr` selects the
  matching platform); the component images (operator, provisioner, admission) have
  multi-arch `*-image-push` buildx targets in `Makefile`.
- **The real Phase 3 work is the exceptions:**
  1. **Non-portable JARs** — those bundling **JNI native libraries** or
     arch-specific dependencies (e.g. `netty-tcnative`, RocksDB, some crypto libs)
     are *not* arch-neutral. Brewlet's optional `arch` constraint (§3.2, now
     shipped) lets such an artifact declare **"this artifact needs amd64"** so it is
     kept off incompatible nodes; without it the JAR could be scheduled onto an
     arm64 node and fail at runtime with an `UnsatisfiedLinkError`.
  2. **Arch-coupled accelerators** — AppCDS archives are
     arch-specific and quietly break the "one artifact, any arch" property
     ([appcds.md](appcds.md)).
- **Shipped in Phase A:** an optional `arch` constraint in the launch config /
  CRD that steers scheduling via the standard `kubernetes.io/arch` node label
  (reusing the existing admission-webhook nodeAffinity mechanism), plus CI/build
  guidance for multi-arch component images. Everything else stays transparent.

---

## 2. What already works

| Concern | Status | Where |
|---|---|---|
| **Artifact portability** | ✅ Bytecode JAR is arch-neutral; same artifact any arch | §12, [jdk-management](jdk-management.md#architecture-mapping-multi-arch) |
| **Per-arch JDK install** | ✅ `ctr` copy-from-image auto-selects the node platform; `uname -m` → vendor token | `provisioner/entrypoint.sh` (`host_arch_oci`, `jdk_from_image`) |
| **Per-node capability advertising** | ✅ JDK/launcher labels are per node, so arch is implicit in which node has which root | `labels.go`, provisioner `label_node` |
| **Multi-arch component images** | ✅ buildx `provisioner-image-push` / `operator-image-push` / `admission-image-push` build `linux/amd64,linux/arm64` | `Makefile` |
| **Shim/binary per arch** | ✅ compiled per-arch (`GOARCH`), baked into the per-arch provisioner image | `Makefile`, `provisioner/Dockerfile` |

So for the **common case — a pure-bytecode JAR** — multi-arch is already done and
transparent. The gaps below are about correctness for the *uncommon* cases and
about not letting Phase 3 accelerators silently regress portability.

---

## 3. Gap 1 — non-portable JARs (the core research problem)

### 3.1 The problem

Not every JAR is arch-neutral. A fat JAR may bundle **native `.so`/`.dll`
libraries** loaded via JNI, or pull arch-specific classifiers at build time. Such
an app runs only on the arch(es) whose natives were bundled. Under Brewlet today:

- The artifact carries no arch metadata.
- The `brewlet` RuntimeClass nodeSelector only requires `brewlet.sh/runtime=ready`
  (§7), which spans all arches.
- The admission webhook steers on JDK/launcher capability, **not** arch.

→ A non-portable artifact can land on the wrong arch and fail at runtime
(`UnsatisfiedLinkError`) rather than being kept off incompatible nodes. This is the
one place multi-arch is *not* safe by default.

### 3.2 The fix — an optional `arch` constraint (implemented)

Add an **optional** architecture constraint that developers set only when their JAR
is not portable. Default (unset) = "runs anywhere", preserving today's behavior.

Launch config (SPECIFICATION §4.2):

```json
{
  "schemaVersion": 1,
  "mainJar": "app.jar",
  "entry": { "mode": "jar" },
  "arch": ["amd64"]        // optional; omit = arch-neutral (default)
}
```

CRD (§9) mirror: `spec.arch: [amd64]` (or `spec.nodeSelector` passthrough for the
fully manual escape hatch).

**Enforcement reuses existing machinery.** The admission webhook already injects
`nodeAffinity` for JDK/launcher capabilities (`injectNodeAffinity`,
`requiredCapabilityLabels` in `mutate.go`). An `arch` constraint maps directly to
the **standard, kubelet-provided** `kubernetes.io/arch` label — no new provisioner
label needed:

```yaml
# injected onto a non-portable pod:
nodeAffinity:
  requiredDuringSchedulingIgnoredDuringExecution:
    nodeSelectorTerms:
      - matchExpressions:
          - { key: kubernetes.io/arch, operator: In, values: [amd64] }
```

If no ready node of the required arch exists, deny with a `NoCompatibleArch`
reason, joining the §14 failure-mode table alongside `NoCompatibleJDK`.

### 3.3 Tooling can auto-detect it

The `brewlet` CLI (`push`) and the Maven plugin's `JarInspector` can **scan the JAR
for bundled natives** (entries like `*.so`, `*.dll`, `*.dylib`, or Maven native
classifiers) and default `arch` accordingly — the same inference pattern already
used to pick `jar` vs `classpath` entry mode. A portable JAR yields no `arch`
(runs anywhere); a JAR with only `linux-x86_64` natives yields `["amd64"]`.

---

## 4. Gap 2 — accelerators that break portability

Two Phase 3 features re-introduce arch coupling and must not silently regress the
"one artifact, any arch" guarantee:

| Feature | Why it's arch-coupled | Mitigation |
|---|---|---|
| **AppCDS** ([appcds.md](appcds.md)) | A `.jsa` archive is arch- and JDK-build-specific | Prefer **node-side regeneration** so the base artifact stays arch-neutral; if shipping an archive, ship one per arch or accept `-Xshare:auto` fallback (which still runs, just cold). |

**Design rule:** an accelerator layer must never turn a portable artifact into a
non-portable one *silently*. If an artifact ships an arch-specific accelerator, its
`arch` constraint should reflect that; if the accelerator is generated node-side,
portability is preserved and no `arch` constraint is added.

---

## 5. Gap 3 — operator/build ergonomics

- **Document the buildx flow** for cutting multi-arch component images
  (`make provisioner-image-push operator-image-push admission-image-push`) and
  recommend digest-pinned manifest-list refs in `values.yaml images:`.
- **Heterogeneous JDK inventory across pools.** Multi-arch is orthogonal to *which*
  JDK feature is installed, but operators should keep the JDK feature set
  consistent across arches so a `jdk-feature.21` request is satisfiable on both —
  otherwise a feature request narrows scheduling to one arch by accident. Surface
  arch×JDK coverage in fleet observability ([metrics-exporter.md](metrics-exporter.md)).

---

## 6. What existing features this touches

| Area | Interaction |
|---|---|
| **Launch config (§4.2)** | New optional `arch` array; `Validate()`/`DecodeConfig` must accept it (currently `DisallowUnknownFields`). |
| **`JavaApplication` CRD (§9)** | Optional `spec.arch`; controller stamps it for the webhook (mirrors how it stamps `brewlet.sh/jdk`). |
| **Admission webhook (§8.3)** | Extend `requiredCapabilityLabels` to add a `kubernetes.io/arch In [...]` requirement; new `NoCompatibleArch` denial. |
| **Provisioner (§5)** | No change for the arch *label* (kubelet sets `kubernetes.io/arch`); already arch-aware for JDK install. |
| **CLI / Maven plugin** | Auto-detect bundled natives → default `arch`. |
| **AppCDS** | Keep accelerators node-side to preserve portability; otherwise reflect arch coupling in `arch`. |
| **Build / CI (`Makefile`)** | Multi-arch component images via buildx; documented. |
| **Docs** | Promote the "same artifact, any arch" advantage in [concepts](concepts.md); add the non-portable-JAR caveat + `arch` field to [deploying-workloads](deploying-workloads.md) and [reference](reference.md). |

---

## 7. Recommendation & phasing

1. **Phase A0 — document the win + the build flow.** State the arch-neutral-artifact
   advantage and the buildx multi-arch component-image targets. Zero code.
2. **Phase A — optional `arch` constraint. ✅ Implemented.** Adds `arch` to the
   launch config + CRD; injects `kubernetes.io/arch` nodeAffinity in the webhook;
   `NoCompatibleArch` denial; auto-detects bundled natives in the CLI/Maven plugin.
   This makes non-portable JARs *safe by construction* while leaving portable JARs
   untouched.
3. **Phase B — coverage observability + accelerator guardrails.** Expose arch×JDK
   fleet coverage as metrics; enforce the "accelerators don't silently break
   portability" rule when AppCDS lands.

Multi-arch is largely an **advantage Brewlet already has**; the Phase 3 work is a
small, optional scheduling constraint for the minority of non-portable JARs, plus
discipline so the new startup accelerators don't undermine portability.

---

## 8. References

- [Kubernetes well-known labels: `kubernetes.io/arch`](https://kubernetes.io/docs/reference/labels-annotations-taints/#kubernetes-io-arch).
- [docker buildx multi-platform builds](https://docs.docker.com/build/building/multi-platform/).
- JNI / native libraries in JARs; native classifiers (e.g. netty-tcnative).
- Brewlet: [SPECIFICATION §12 (multi-arch)](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#12-networking-observability-day-2),
  [§7 (RuntimeClass)](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#7-runtimeclass),
  [jdk-management](jdk-management.md#architecture-mapping-multi-arch),
  [observability](observability.md#day2-multi-arch-fleets),
  [appcds.md](appcds.md);
  core `Makefile` and `provisioner/entrypoint.sh`,
  Kubernetes `internal/admission/mutate.go`.
