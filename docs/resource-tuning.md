# Resource requests, limits & JVM tuning

Brewlet maps Kubernetes resource requests and limits to the workload and lets the
**container-aware JVM** react — it injects **no** `-XX` flags of its own. This page
explains their distinct effects and how to tune the JVM correctly.

Related: [Launchers](launchers.md) · [Deploying workloads](deploying-workloads.md).

---

## Requests versus limits

**Requests** influence scheduling, CPU shares, capacity planning, and the
utilization denominator used by the HPA. **Limits** define the cgroup ceilings
observed by the container-aware JVM: the CPU quota and maximum memory available
to the container. A memory request is not a JVM heap-sizing input; heap sizing
follows the memory limit (for example, through `MaxRAMPercentage`).

## The mapping

The deployment descriptor's requests influence Kubernetes scheduling and resource
accounting, while limits drive the **sandbox cgroup ceilings**. Modern JDKs are
cgroup-v2 aware and read those limits directly.

| Descriptor field | Kubernetes/cgroup effect | JVM effect |
|---|---|---|
| `resources.requests.memory` | Scheduling and capacity planning reservation. | No direct heap-sizing effect. |
| `resources.requests.cpu` | Scheduling reservation and CPU shares; CPU HPA utilization is calculated relative to this request. | No fixed processor count; without a CPU limit, the JVM sees the node's available CPU count. |
| `resources.limits.memory` | `memory.max` | `-XX:+UseContainerSupport` (default on) reads the cgroup and sizes the heap. |
| `resources.limits.cpu` | `cpu.max` (quota/period) | The cgroup-aware JDK auto-detects available processors from the quota; GC/JIT thread counts scale accordingly. |

You can see this for real in integration-test tier 3. Its Linux/runc harness pins
`--cpus=1 --memory=384m`, and the app reports `availableProcessors = 1` and
`memory.max = 384Mi` from *inside* the JVM. See
[Getting started](getting-started.md).

> **cgroup v2 is required.** The provisioner refuses cgroup v1-only nodes, so the
> container-awareness above always holds on a ready node.

---

## Who tunes the JVM

**Brewlet never injects tuning flags.** Tuning is either yours or your launcher's:

- **Vanilla `java`** → *you* tune heap/GC/agents/container flags via descriptor
  `jvm.args`. The artifact carries only app-intrinsic correctness knobs such as
  preview features, module access, and system properties.
- **`jaz`** → the launcher derives sensible ergonomics (heap/GC/CPU) from the cgroup
  limits, so you usually pass no tuning flags. See [Launchers](launchers.md).

Precedence for the vanilla path: artifact structured launch knobs
(`--enable-preview`, `--add-modules`, `--add-opens`, `--add-exports`, sorted
`-D` flags) → descriptor `jvm.args` (append/override where the JVM honors
last-wins).

---

## Recommended tuning (vanilla `java`)

### Memory — leave headroom for non-heap

The container memory limit must cover **more than the heap**: Metaspace, thread
stacks, JIT code cache, direct/`mmap` buffers, and GC structures all live outside
`-Xmx`. If you size the heap to 100% of the limit, the JVM (or the kernel OOM killer)
will kill the pod.

Set a percentage that reserves headroom (commonly ~25%):

```yaml
jvm:
  args:
    - "-XX:MaxRAMPercentage=75.0"     # heap ≈ 75% of memory.max; ~25% for non-heap
```

`MaxRAMPercentage` is preferred over a fixed `-Xmx` because it tracks whatever
`limits.memory` you set — resize the pod and the heap follows.

### Fail fast on OOM

Let a memory-exhausted JVM exit cleanly so the kubelet restarts it:

```yaml
jvm:
  args:
    - "-XX:+ExitOnOutOfMemoryError"
```

### GC selection

Pick a collector suited to your workload; the JVM will size its GC threads from the
CPU quota:

```yaml
jvm:
  args:
    - "-XX:+UseZGC"        # low-latency; or -XX:+UseG1GC (default), -XX:+UseParallelGC
```

### A solid default set

```yaml
jvm:
  version: 21
  launcher: java
  args:
    - "-XX:MaxRAMPercentage=75.0"
    - "-XX:+UseZGC"
    - "-XX:+ExitOnOutOfMemoryError"
```

Or hand it all to `jaz` and pass nothing:

```yaml
jvm:
  version: 21
  launcher: jaz
```

---

## CPU limits and threads

- With a CPU **limit**, `cpu.max` sets a quota; the JVM computes
  `availableProcessors()` from it and scales GC, JIT compiler, and common
  `ForkJoinPool` thread counts.
- With only a CPU **request** (no limit), the JVM sees the node's full CPU count —
  which may over-provision internal thread pools on a busy node. Set a limit if you
  want deterministic sizing.
- Fractional limits (e.g. `cpu: "1500m"`) are honored via the quota; the JVM rounds
  processor counts as it sees fit.

---

## Ports

`ports` is a **deployment concern, not an artifact field**. It lives in the
descriptor (CRD `spec.ports` or the Maven `manifest` goal's `<ports>`), where the
operator uses it to wire the Service and probes. The artifact's launch config
carries no ports.

Brewlet does **not** translate a port into any JVM system property — the listen
port is a framework concern (e.g. Spring Boot's `server.port`, Quarkus's
`quarkus.http.port`), so configure it the way your framework expects, via `env`
or extra args (`-- -Dserver.port=8080`) on a local `run`. The local `run` path
does not touch ports at all.

---

## Startup performance

Cold start is the JVM's classic weakness vs. Wasm. Brewlet mitigates it with:

- **Shared, pre-warmed JDK** on the node → no per-pod JDK pull/unpack.
- **Artifact caching:** containerd's content store caches the JAR layer; only the
  (small) JAR moves over the network, not a full image.
- **AppCDS / dynamic CDS:** ships a class-data archive as a dedicated
  `cds.layer` — build-time via the Maven `brewlet:appcds` goal / `brewlet push
  --appcds-archive`, plus opt-in node-side regeneration — to cut startup. See
  [AppCDS](appcds.md).

See [SPECIFICATION §13](https://github.com/brewlet/brewlet/blob/main/specs/SPECIFICATION.md).

---

## Checklist

- [ ] Set resource **requests** for schedulability and capacity planning.
- [ ] Set resource **limits** for deterministic JVM sizing and cgroup ceilings.
- [ ] Vanilla `java`: set `-XX:MaxRAMPercentage` (reserve non-heap headroom) and
      `-XX:+ExitOnOutOfMemoryError`; pick a GC. **Or** use `jaz` and set nothing.
- [ ] Don't expect Brewlet to add flags — it doesn't.
- [ ] Remember the `RuntimeClass` `overhead.podFixed` accounts for baseline JVM
      overhead in scheduling/quotas ([Configuration](configuration.md#runtimeclass)).
