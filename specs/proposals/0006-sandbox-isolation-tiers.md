# Proposal 0006 — Sandbox isolation tiers

- **Status:** research complete; runtime implementation is gated on a successful
  compatibility and performance prototype
- **Related roadmap item:** stronger sandbox options
- **Related issue:** [#1](https://github.com/brewlet/brewlet/issues/1)
- **Current execution path:** [`core/shim`](../../core/shim),
  [`provisioner/entrypoint.sh`](../../provisioner/entrypoint.sh)
- **Control plane:** [`NodeProfile`](0001-node-profiles.md), capability labels
  ([0003](0003-capability-label-taxonomy.md)), and Kubernetes `RuntimeClass`

## 1. Decision

Stronger isolation is still a useful direction for Brewlet, but the original
assumption that gVisor is a low-effort `BinaryName=runsc` substitution is not a
supported design conclusion.

The current recommendation is:

1. Retain **gVisor as the preferred candidate** for a stronger isolation tier.
   It preserves a container-like operating model, tests Java as a supported
   language runtime, and is used for untrusted workloads in production services.
2. Require a dedicated **compatibility and performance prototype** before adding
   any API, provisioner, or RuntimeClass implementation. The prototype must prove
   that Brewlet's Runtime v2 shim and host-backed JDK model can execute through
   gVisor without weakening the intended isolation boundary.
3. **Defer Kata Containers** as an equivalent Brewlet tier. Kata's per-pod VM,
   guest kernel, guest page cache, and host-to-guest filesystem transport conflict
   with Brewlet's primary optimization: sharing node-resident JDK and dependency
   bytes across JVMs.
4. If the gVisor prototype succeeds, build the eventual control plane on
   `NodeProfile`, capability labels, and distinct RuntimeClasses. Isolation
   selection must be owned by the platform and must not silently downgrade to
   runc.

This proposal records the research direction and the gates for a later
implementation. It does not change shipped behavior, so
[`SPECIFICATION.md`](../SPECIFICATION.md) remains unchanged.

## 2. Why revisit the original issue

Issue #1 predates several changes that materially affect the design:

- Brewlet now has a production containerd Runtime v2 shim that decorates
  containerd's runc task service. Its `Create()` hook rewrites the OCI bundle,
  resolves the artifact from containerd, selects a node JDK and launcher, creates
  the overlay rootfs, and mounts application layers before delegating the
  lifecycle to runc.
- `NodeProfile` now provides platform-owned, per-pool node preparation,
  validated rollout, cleanup, immutable-image operation, and status.
- Capability labels and admission-injected node affinity now steer workloads by
  JDK, launcher, and architecture.
- Runnable-image delivery is the default, so kubelet/containerd pulls and unpacks
  standard OCI layers before the Brewlet shim resolves them.
- Managed dependency bundles can supply a shared, governed classpath layer.
- Node-side AppCDS regeneration maintains a writable per-artifact, per-JDK-build
  cache on the node.
- Supply-chain admission can verify managed-dependency evidence before a Brewlet
  workload is admitted.

These features improve the control plane and artifact pipeline, but they also
create more compatibility surfaces for an alternative sandbox runtime.

## 3. Threat model

An isolation tier is useful only when tied to a concrete trust boundary.

| Deployment model | Runtime recommendation | Reason |
|---|---|---|
| Dedicated or single-tenant nodes running trusted applications | runc | The existing container boundary is operationally simple and preserves maximum startup, density, and compatibility. |
| Governed multi-tenant nodes where application artifacts are built and admitted through trusted pipelines | runc plus supply-chain policy, or gVisor after validation | Provenance controls reduce artifact risk but do not prevent a compromised application from attacking the host kernel. |
| Hostile multi-tenancy with independently controlled, untrusted JARs sharing nodes | gVisor candidate | A userspace application kernel reduces direct exposure to the host kernel while retaining a container-oriented model. |
| Workloads requiring a hardware-virtualized kernel boundary or regulatory VM isolation | Separate Kata-oriented architecture | The stronger boundary may justify the cost, but it should not be presented as preserving Brewlet's node-shared runtime economics. |

Artifact verification and runtime isolation are complementary:

- admission answers **which artifact may run**;
- the sandbox answers **what a running or compromised artifact can reach**.

Neither control replaces the other.

## 4. Current runtime composition

The current Brewlet shim is not a thin executable alias.

1. containerd starts `containerd-shim-brewlet-v2`.
2. Brewlet registers a decorated containerd runc task service.
3. On workload `Create()`, Brewlet:
   - normalizes CRI runtime options into runc options;
   - resolves the runnable image or native artifact;
   - chooses the requested node JDK and launcher;
   - rewrites the OCI process, mounts, environment, and rootfs;
   - stages classpath, module-path, and AppCDS data; and
   - delegates the completed task lifecycle to containerd's runc service.
4. The runc task service invokes the configured OCI runtime and manages
   lifecycle operations such as start, exec, kill, wait, and delete.

This architecture creates a possible seam for another OCI-compatible runtime,
but it does not prove the seam is supported.

Modern gVisor containerd documentation configures the dedicated
`containerd-shim-runsc-v1` and `runtime_type = "io.containerd.runsc.v1"`.
Kata provides its own Runtime v2 shim. containerd has no built-in mechanism for
stacking two independent task-service shims so that Brewlet owns `Create()`
transformation while another shim owns the sandbox lifecycle.

The existing runc options include `BinaryName`, and the Brewlet shim preserves
that field when normalizing generic CRI options. That makes a `runsc` experiment
technically plausible, but not an implementation contract. It must be tested
against the exact containerd and gVisor versions Brewlet supports, including all
task lifecycle methods and CRI pod-sandbox behavior.

## 5. Runtime comparison

| Dimension | runc (current) | gVisor candidate | Kata Containers |
|---|---|---|---|
| Isolation boundary | Linux namespaces, cgroups, capabilities, seccomp; shared host kernel | Userspace application kernel intercepts application syscalls; reduced host-kernel attack surface | Hardware-virtualized guest with its own kernel |
| containerd integration | Brewlet's decorated runc Runtime v2 task service | Official integration uses the dedicated runsc shim; Brewlet composition is unproven | Dedicated Kata Runtime v2 shim |
| Java compatibility | Native Linux behavior | gVisor reports regression testing for Java, but application compatibility still requires testing | Broad Linux compatibility inside the guest |
| CPU execution | Native | Native application instructions; syscall and networking paths add overhead | Virtualized execution with VM and guest overhead |
| Filesystem path | Host overlay and bind mounts | Host files are mediated by gVisor filesystem mechanisms; semantics and cache behavior require measurement | Rootfs is shared or attached into the VM, commonly through virtio-fs or block devices |
| Node JDK sharing | Direct host page cache and one installed root per node | JDK bytes can potentially remain host-backed, but per-sandbox filesystem/page-cache behavior must be measured | Guest page caches and per-VM filesystem transport erode cross-pod sharing |
| Startup | Lowest baseline | Additional sandbox initialization and syscall/filesystem overhead | VM boot and guest initialization |
| Runtime overhead | Existing RuntimeClass JVM baseline | Sentry and filesystem/network components require measured CPU/memory overhead | VMM, guest kernel, agent, and VM memory require materially larger overhead |
| Hardware requirements | Standard Linux | `systrap` works without nested virtualization; KVM platform has environment-specific requirements | Hardware virtualization and a supported hypervisor stack |
| Operational fit | Current Brewlet model | Plausible per-pool stronger tier | Separate infrastructure tier with different economics |
| Recommendation | Default | Prototype, then consider support | Defer |

## 6. gVisor assessment

### 6.1 Why it remains the leading candidate

gVisor is a reasonable fit for the original hostile multi-tenancy goal:

- it interposes a userspace kernel between the Java process and the host kernel;
- it retains Kubernetes/containerd/RuntimeClass deployment semantics;
- its compatibility program includes Java;
- it avoids requiring a full guest VM for every pod; and
- its CPU execution model is appropriate for JVM workloads that spend substantial
  time in generated application code rather than syscall-heavy loops.

The likely costs are extra fixed memory, filesystem and networking overhead,
and longer startup. Those costs must be measured using Brewlet workloads rather
than inferred from generic container benchmarks.

### 6.2 The prototype must answer the shim question first

The first prototype should use the smallest possible change:

1. install a version-pinned `runsc`;
2. configure a separate Brewlet runtime handler whose normalized runc options set
   `BinaryName` to `runsc`;
3. run the existing Brewlet task service unchanged; and
4. prove from sandbox-visible evidence that the workload is actually running
   under gVisor.

This path is accepted only if:

- CRI pod sandboxes and workload containers both start reliably;
- an init container, Brewlet application container, and sidecar share the
  intended pod sandbox and retain normal Kubernetes ordering and lifecycle;
- `Create`, `Start`, `Exec`, `Kill`, `Wait`, and `Delete` behave correctly;
- logs, probes, Services, DNS, and termination signals work;
- the host-backed overlay rootfs and all bind mounts retain the expected
  read-only/read-write semantics;
- cgroup limits constrain the entire gVisor sandbox as Kubernetes expects; and
- no configuration bypasses gVisor for the workload process.

If this fails, Brewlet must not silently fall back to runc. The follow-up design
must instead choose explicitly among:

- integrating Brewlet's bundle transformation into a maintained runsc shim
  extension;
- moving artifact-to-OCI-bundle preparation to a supported pre-runtime seam; or
- declining gVisor support because the maintenance or security cost is too high.

Forking a third-party shim or depending on undocumented task-service behavior is
not acceptable without an explicit long-term maintenance plan.

### 6.3 Filesystem and cache questions

Brewlet's performance model depends on host-backed files:

- the selected JDK root is the overlay lower layer;
- the application JAR is mounted read-only;
- classpath and module-path layers are staged and mounted read-only;
- a custom launcher may add another overlay lower layer; and
- AppCDS regeneration uses a writable node cache with writer election.

The prototype must determine:

- whether gVisor accepts the overlay rootfs emitted by the Brewlet shim;
- how JDK and JAR pages are cached across sandboxes;
- whether read-only bind mounts and executable mappings behave correctly;
- whether AppCDS file creation, locking, rename, and reuse behave correctly;
- whether direct filesystem modes alter the security boundary; and
- whether managed dependency layers remain deduplicated in storage and memory.

Passing functional tests is necessary but insufficient. A configuration that
works by copying a complete JDK into each sandbox would defeat the reason to
combine gVisor with Brewlet. The same applies if each additional sandbox
duplicates approximately the full hot JDK and classpath working set in its own
memory instead of adding primarily fixed sandbox overhead. That result is a
disqualifying density regression unless the final recommendation explicitly
redefines the gVisor tier as an isolation-first mode that does not preserve
Brewlet's node-sharing economics.

## 7. Kata assessment

Kata is not a drop-in OCI binary behind Brewlet's current task service. It owns
the Runtime v2 lifecycle, starts a hypervisor, boots a guest, and asks the guest
agent to create containers inside the VM.

That model offers a stronger and more familiar kernel boundary, but it changes
the core Brewlet tradeoff:

- host JDK files must cross into each VM through a shared-filesystem or block
  mechanism;
- the guest kernel maintains its own page cache;
- AppCDS cache access crosses the host/guest boundary;
- RuntimeClass overhead must include VM memory and VMM processes;
- startup includes VM boot and guest-agent readiness; and
- node requirements include hardware virtualization, hypervisor assets, guest
  images, and Kata-specific lifecycle management.

Kata may still make sense for a future product mode where isolation is more
important than node-wide JDK sharing. That mode should be evaluated as a
distinct architecture, potentially with a JDK baked into a guest image or
provided through Kata-native storage. It should not be described as an
equivalent Brewlet backend until measurements prove that its value proposition
survives.

## 8. Future control-plane direction

No API is finalized by this research proposal. If the gVisor prototype passes,
the implementation design should extend existing platform-owned concepts.

### 8.1 NodeProfile owns backend preparation

A future `NodeProfile` revision may declare the isolation backend prepared on
its selected pool. The exact field shape remains a separate API review, but the
semantics should be:

- one effective Brewlet isolation backend per profile/pool;
- provision, validate, advertise, and clean up the backend with the profile;
- support immutable node images through the existing label-only mode;
- report backend validation failures through profile status; and
- reject overlapping or ambiguous backend ownership.

The provisioner should advertise an exact node capability such as
`brewlet.sh/isolation=gvisor`. This joins the existing runtime, JDK, launcher,
and architecture capability model.

### 8.2 Distinct RuntimeClasses

The operator should eventually manage separate RuntimeClasses, for example:

- `brewlet` for the backward-compatible runc path; and
- `brewlet-gvisor` for the validated sandbox path.

Each RuntimeClass must:

- use its own containerd handler;
- select only nodes advertising the matching isolation capability;
- publish measured, backend-specific scheduling overhead; and
- remain unavailable until backend validation has completed.

Using distinct handlers avoids changing the runtime implementation underneath a
single RuntimeClass based only on which node the scheduler happened to choose.

### 8.3 Platform-controlled workload selection

The stronger tier is a security policy, not an application optimization flag.
The eventual design should support a platform-owned assignment, such as a
namespace policy that the `JavaApplication` controller resolves into the
appropriate RuntimeClass.

Requirements:

- existing applications continue to use the standard `brewlet` class by default;
- application authors cannot downgrade a namespace or workload that the platform
  requires to use gVisor;
- unavailable sandbox capacity causes denial or an unschedulable workload, never
  fallback to runc;
- raw Deployments and `JavaApplication` resources follow the same policy; and
- the enforcement path fails closed.

The current pod mutating webhook uses `failurePolicy: Ignore` for availability.
It must not be the only enforcement point for mandatory isolation. Brewlet
already ships a `failurePolicy: Fail` validating webhook for `NodeProfile`; the
future workload policy should extend that established fail-closed pattern with a
dedicated validating rule, or use an equivalent control that cannot be bypassed
during a mutating-webhook outage.

## 9. Compatibility gate

A later implementation issue must provide automated coverage for all of the
following under both runc and the candidate gVisor tier:

| Area | Required proof |
|---|---|
| CRI lifecycle | Pod sandbox, single-container and multi-container pods (init container plus sidecar), workload create/start, exec, logs, probes, signal handling, graceful termination, forced deletion |
| Artifact delivery | Default runnable image pulled by kubelet and resolved from containerd content |
| Native artifact path | Existing pre-pulled/native-artifact fallback where supported |
| Launch modes | JAR, layered classpath, JPMS module path, and mixed module/classpath |
| Managed dependencies | Governed thin JAR using the exact managed classpath layer |
| JDK inventory | Feature-only and distribution-specific selection, custom JDK source, incompatible JDK denial |
| Launchers | Vanilla `java` and an installed custom launcher |
| AppCDS | Seed archive, node-side regeneration, second-start reuse, JDK-patch cache invalidation |
| Kubernetes networking | CNI, DNS, Service routing, readiness and liveness probes |
| Resources | CPU and memory limits applied to the complete sandbox; OOM and termination behavior |
| Security identity | Positive evidence that the workload runs under gVisor; negative test proving no runc downgrade |
| Multi-architecture | amd64 and arm64 where the chosen gVisor platform and Brewlet release support them |
| Operations | NodeProfile rollout, immutable-image mode, readiness, upgrade, failure status, and cleanup |
| Admission | JDK/launcher/arch checks plus mandatory-isolation enforcement |

Compatibility exceptions must be explicit and documented. They must not be
hidden behind a success-shaped fallback.

## 10. Performance gate

Benchmarks should compare identical Brewlet applications under runc and gVisor
on the same node type and JDK build.

Measure at least:

- pod creation to JVM process start;
- pod creation to readiness for a representative Spring Boot service;
- first and second startup with node-side AppCDS regeneration;
- idle RSS and total host memory per pod, including sandbox processes;
- CPU throughput for a compute-heavy Java workload;
- request latency and throughput for a network service;
- startup with a large managed dependency classpath;
- filesystem-heavy class loading and JAR scanning;
- page-cache reuse when starting many replicas on one node; and
- pod density before memory or CPU saturation.

The proposal does not set arbitrary pass percentages. The prototype report must
publish raw results and define an operational envelope: which workload classes
are suitable for gVisor and which should remain on runc.

The density result does have a qualitative rejection threshold: if incremental
memory per replica scales with another copy of the hot JDK and dependency
working set rather than predominantly fixed sandbox overhead, the prototype
must either reject gVisor for the normal Brewlet model or explicitly propose an
isolation-first tier with different density expectations. On-disk layer
deduplication alone is not sufficient.

## 11. Rollout shape after a successful prototype

The likely implementation sequence is:

1. publish a prototype report resolving the `BinaryName=runsc` and filesystem
   questions;
2. review the NodeProfile and RuntimeClass API design;
3. add version-pinned backend installation and validation;
4. add isolation capability labels and profile status;
5. add distinct RuntimeClasses with measured overhead;
6. add platform-owned, fail-closed workload assignment;
7. add the compatibility matrix to the tiered integration suite; and
8. document supported node environments, limitations, and performance guidance.

Kata should have a separate proposal if renewed. It must define how the JDK,
application layers, and AppCDS data enter the guest and quantify the loss of
node-wide sharing.

## 12. Non-goals

- Implementing gVisor or Kata in this proposal.
- Claiming that `BinaryName=runsc` is a supported production integration.
- Adding a developer-controlled isolation field to `JavaApplication`.
- Automatically falling back from a stronger tier to runc.
- Treating supply-chain verification as sandbox isolation.
- Promising identical compatibility or performance across backends.
- Changing the shipped behavior in `SPECIFICATION.md`.

## 13. References

- [gVisor containerd quick start](https://gvisor.dev/docs/user_guide/containerd/quick_start/)
- [gVisor platform choices](https://gvisor.dev/docs/architecture_guide/platforms/)
- [gVisor resource and memory architecture](https://gvisor.dev/docs/architecture_guide/resources/)
- [gVisor compatibility](https://gvisor.dev/docs/user_guide/compatibility/)
- [gVisor performance guide](https://gvisor.dev/docs/architecture_guide/performance/)
- [Kata Containers architecture](https://github.com/kata-containers/kata-containers/blob/main/docs/design/architecture/README.md)
- [Kata Containers storage architecture](https://github.com/kata-containers/kata-containers/blob/main/docs/design/architecture/storage.md)
- [Kubernetes RuntimeClass](https://kubernetes.io/docs/concepts/containers/runtime-class/)
