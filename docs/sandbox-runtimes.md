# Sandbox runtimes: gVisor & Kata — research

> **Status.** Research / design note. Nothing here is implemented yet — it
> fleshes out the Phase 4 roadmap item *"gVisor/Kata option"* and the
> multi-tenancy guidance in
> [SPECIFICATION §11](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#11-security-model) /
> [security.md](security.md#multi-tenancy-guidance) (Open Question #5). It
> documents *intended* behavior and the architectural tradeoffs, not shipped code.

---

## 1. TL;DR

- **The problem is real.** Brewlet execution is **runc-backed** (§6.3), so a
  Brewlet workload has exactly a container's trust boundary — no more. For hostile
  multi-tenancy (running *untrusted* JARs from different tenants on shared nodes),
  runc's shared-kernel isolation may be insufficient.
- **gVisor is a clean, low-effort fit.** gVisor's `runsc` is an
  **OCI-runtime-compatible binary**. Brewlet's shim wraps containerd's runc *task*
  service, which accepts a `BinaryName` option — pointing it at `runsc` keeps
  Brewlet's entire artifact-assembly `Create()` hook and just changes the runtime
  that executes the bundle. Offer it as an **isolation tier** (a second
  RuntimeClass / node pool), not a per-developer knob.
- **Kata is architecturally harder and partly at odds with the model.** Kata is
  **not** a runc-CLI binary; it is its *own* containerd shim
  (`containerd-shim-kata-v2`) that boots a microVM. Brewlet's shim *is* the
  runc-task decorator, so "Brewlet artifact assembly + Kata VM" cannot be a simple
  binary swap. Worse, a microVM has its **own kernel and page cache**, which
  erodes Brewlet's "one shared, pre-warmed, page-cache-shared node JDK" advantage.
- **Isolation trades against startup.** gVisor adds syscall-interception overhead;
  Kata adds VM boot. Both push *against* the Phase 3 startup goals (AppCDS).
  Document the tension; let operators choose per node pool.
- **This is a platform/operator decision surfaced as RuntimeClass tiers**, kept
  transparent to developers.

---

## 2. Background

### 2.1 How Brewlet executes today

The provisioner registers a containerd runtime handler `brewlet` pointing at
`runtime_type = "io.containerd.brewlet.v2"` (`provisioner/entrypoint.sh`,
`patch_containerd`). The shim
(`shim/cmd/containerd-shim-brewlet-v2/service_linux.go`) **embeds containerd's
own runc task service** (`runtime/v2/runc/task`) and decorates only `Create()` to
disassemble the OCI artifact into a `java -jar` bundle (overlay rootfs from the
shared node JDK, JAR/JDK bind mounts). Everything else — start/kill/exec/wait —
delegates to runc. So the *isolation primitive is runc*.

### 2.2 The two candidates

| | **gVisor (`runsc`)** | **Kata Containers** |
|---|---|---|
| Boundary | User-space kernel intercepting syscalls (ptrace/KVM platform) | Hardware-virtualized **microVM** (real guest kernel) |
| Packaging | An **OCI runtime binary** (`runsc`), CLI-compatible with `runc` | A **containerd shim** (`containerd-shim-kata-v2`) + hypervisor |
| Node needs | `runsc` binary; KVM for the fast platform | Nested virt/bare-metal KVM, a hypervisor (QEMU/Cloud-Hypervisor/Firecracker), a guest kernel+rootfs |
| Overhead | Low-moderate CPU on syscall-heavy paths | VM boot + per-VM memory; own page cache |
| Brewlet integration | **Swap the OCI runtime binary** the runc task drives | **Replace the shim** — incompatible with the runc-task decorator |

---

## 3. Alignment with the Brewlet model

- **runc (today) — parity with containers.** Correct default for trusted tenants
  and your own services; documented as such in [security.md](security.md).
- **gVisor — aligns well.** It swaps only the syscall boundary. The shared node
  JDK, overlay rootfs, bind mounts, cgroup limits and CNI netns are unchanged
  because Brewlet's `Create()` still runs and `runsc` consumes the same OCI bundle.
  The JDK-on-node advantage survives.
- **Kata — partially anti-aligned.** Two frictions:
  1. **Shim topology.** Kata is a separate v2 shim; Brewlet's artifact-assembly is
     baked into a runc-task decorator. Combining them means either (a) porting
     Brewlet's `Create()` disassembly into a Kata-fronting shim, or (b) letting
     Kata mount a prepared bundle — neither is a drop-in.
  2. **Shared-JDK economics.** Brewlet's headline efficiency is *one* JDK userland
     on the node, shared read-only across sandboxes (overlay lower layer,
     `setupOverlayRootfs`), warm in the host page cache. A microVM has its own
     kernel and page cache; the JDK must be surfaced into the guest (virtio-fs /
     block image) and is **not** page-cache-shared across VMs. You keep "no image
     build" but lose much of the "shared, pre-warmed JDK" win, and pay VM memory
     overhead per pod.

**Conclusion:** implement **gVisor** as a first-class optional isolation tier.
Treat **Kata** as *documented interop* / later research, being honest that it
partly trades away Brewlet's core efficiency and needs deeper shim work.

---

## 4. How to surface it — isolation tiers, not a developer knob

Isolation strength is a **platform** decision (which node pools exist, what a
tenant is allowed to land on). Developers should not hand-pick a hypervisor.
Surface it as **RuntimeClass tiers** mapped to containerd runtime handlers:

| RuntimeClass | Handler | Isolation | Typical use |
|---|---|---|---|
| `brewlet` | runc-backed brewlet shim | container-grade | trusted / first-party |
| `brewlet-gvisor` | brewlet shim with `runsc` | user-space kernel | untrusted JARs, shared nodes |
| `brewlet-kata` *(future)* | Kata-fronting integration | microVM | strong multi-tenant isolation |

Configuration knob (Helm), operator-owned:

```yaml
provisioner:
  # Underlying OCI runtime for the brewlet sandbox.
  sandboxRuntime: runc        # runc | gvisor
  # Or, to offer multiple tiers simultaneously:
  # sandboxRuntimes: [runc, gvisor]
```

The `JavaApplication` controller renders `runtimeClassName: brewlet` today
(`buildDeployment`). If multiple tiers coexist, add an optional
`spec.isolation: standard|sandboxed` that maps to the right RuntimeClass — but
default to standard and keep it optional, so the common case is unchanged.

---

## 5. Implementing the gVisor tier

### 5.1 Shim: drive `runsc` instead of `runc`

Containerd's runc task service selects the OCI runtime via
`options.Options{BinaryName: …}` (the `runc`/`runsc`/`crun` binary). Because
`runsc` is CLI-compatible, the brewlet shim can request it. Two placements:

- **Per-handler default:** register a distinct containerd runtime
  (`io.containerd.brewlet-gvisor.v2`, or the `brewlet` handler with runtime
  options) whose `BinaryName = runsc`. The provisioner writes this into
  `/etc/containerd/config.toml`.
- **Env/annotation-driven** (dev/testing): the shim reads `BREWLET_OCI_RUNTIME`.

`Create()` (artifact disassembly) is unchanged; only the runtime that executes the
resulting bundle differs.

### 5.2 Provisioner: install `runsc` + register the runtime

Extend `entrypoint.sh`:
- Install `runsc` (copy-from-image via `ctr`, mirroring the JDK/launcher install
  in `jdk_from_image`/`install_launcher`, or a pinned download) onto the host PATH.
- Preflight the platform: KVM availability (fast path) vs ptrace fallback; refuse
  or warn if unsupported.
- In `patch_containerd`, emit the gVisor runtime block, e.g.:
  ```toml
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.brewlet-gvisor]
    runtime_type = "io.containerd.brewlet.v2"
    [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.brewlet-gvisor.options]
      BinaryName = "/usr/local/bin/runsc"
  ```
- Advertise the tier as a node capability label (new
  `brewlet.sh/sandbox.gvisor=true`), so admission/scheduling can steer
  `brewlet-gvisor` pods to gVisor-capable nodes — reusing the exact per-capability
  label + nodeAffinity mechanism already built for JDKs/launchers
  (`labels.go`, `injectNodeAffinity`).

### 5.3 Operator: reconcile the extra RuntimeClass

The node controller (§8.1) already reconciles the `brewlet` RuntimeClass as a
cluster singleton. Add reconciliation for `brewlet-gvisor` (its own
`scheduling.nodeSelector` on the gVisor capability label and its own, higher
`overhead.podFixed`).

### 5.4 Admission steering

The webhook already steers pods to nodes advertising a requested capability.
Extend the capability vocabulary with a sandbox tier so a `brewlet-gvisor` pod
only lands on gVisor-ready nodes (`NoCompatibleSandbox`, joining the §14 table).

---

## 6. What existing features this touches

| Area | Interaction |
|---|---|
| **Shim (§6)** | Add a `BinaryName`/runtime-options path for the runc task; `Create()` disassembly unchanged. |
| **Provisioner (§5)** | Install `runsc`; platform preflight (KVM); emit the gVisor runtime handler + capability label. |
| **RuntimeClass (§7)** | A second class `brewlet-gvisor` with its own nodeSelector and larger `overhead`. |
| **Operator node controller (§8.1)** | Reconcile the extra RuntimeClass singleton. |
| **Admission webhook (§8.3)** | New sandbox-tier capability + nodeAffinity steering; `NoCompatibleSandbox` denial. |
| **`JavaApplication` CRD (§8.2/§9)** | Optional `spec.isolation` → RuntimeClass selection (default unchanged). |
| **Resource↔JVM mapping (§10)** | gVisor presents cgroup limits to the guest, so the container-aware JVM still sizes heap/CPU correctly — **verify** `runsc`'s cgroupfs presentation. Kata sizes via *guest RAM = limit*, a different mechanism to validate. |
| **Startup (§13)** | gVisor adds syscall overhead; Kata adds VM boot. Interacts with AppCDS (page-cache sharing weaker under Kata). |
| **Security (§11)** | This is the mitigation the security page already points to for untrusted JARs; promote from "roadmap option" to a configured tier. |

---

## 7. Kata: why it's harder (and the honest recommendation)

- **Not a binary swap.** Kata replaces the shim, so Brewlet's `Create()` artifact
  disassembly would have to be re-hosted in a Kata-integrating shim, or Brewlet
  would have to hand Kata a fully-prepared bundle via a different seam. Either is
  real engineering, not configuration.
- **Erodes the shared-JDK win.** Per-VM kernels/page-caches mean the node JDK isn't
  shared the way it is under runc/gVisor; you pay per-pod VM memory and lose warm
  page-cache reuse. The "no image, shared node JDK" value is diluted to "no image".

**Recommendation for Kata:** document it as *possible* for teams that need VM-grade
isolation and accept the tradeoffs, but **prioritize gVisor** for Phase 4. If Kata
is pursued, frame it as a distinct integration project, and be explicit that such
workloads may be better served by ordinary Kata-backed containers (Brewlet stays
additive — it only intercepts `runtimeClassName: brewlet*`).

---

## 8. Recommendation & phasing

1. **Phase A — gVisor tier (recommended).** Shim `BinaryName=runsc`; provisioner
   installs `runsc`, preflights KVM, registers `brewlet-gvisor` + capability label;
   operator reconciles the RuntimeClass; webhook steers + denies
   `NoCompatibleSandbox`; optional CRD `spec.isolation`. Document the startup
   tradeoff.
2. **Phase B — Kata interop (research/optional).** Prototype hosting Brewlet's
   `Create()` disassembly in front of Kata (or a prepared-bundle handoff);
   measure the shared-JDK/startup cost; decide whether it earns its place or stays
   a documented "use Kata containers instead" pointer.

gVisor changes the *runtime binary*, not the artifact or launch model, so it fits
Brewlet with minimal surface area. Kata changes the *shim topology and the
shared-JDK economics*, so it deserves a separate, evidence-driven decision.

---

## 9. References

- [gVisor](https://gvisor.dev/docs/) — `runsc`, OCI compatibility, containerd
  `runtimeHandler` integration.
- [Kata Containers](https://katacontainers.io/) — `containerd-shim-kata-v2`,
  microVM architecture.
- [Kubernetes RuntimeClass](https://kubernetes.io/docs/concepts/containers/runtime-class/)
  and Pod Overhead.
- containerd runc `options.Options{BinaryName}` (runtime v2 runc task service).
- Brewlet: [SPECIFICATION §6 (shim)](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#6-the-containerd-shim-containerd-shim-brewlet-v2),
  [§7 (RuntimeClass)](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#7-runtimeclass),
  [§11 (security)](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#11-security-model),
  [security.md](security.md), [appcds.md](appcds.md);
  `shim/cmd/containerd-shim-brewlet-v2/service_linux.go`,
  `provisioner/entrypoint.sh`.
