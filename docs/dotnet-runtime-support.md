# Multi-runtime support: .NET as the second runtime family

> **Status.** Design proposal / research note. Nothing here is implemented yet.
> Brewlet today runs the JVM only; [SPECIFICATION §2.2](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md) lists
> multi-language runtimes (Python, Node) as a v1 non-goal. This note argues for
> **relaxing that non-goal for .NET first**, and sketches how to generalize the
> existing design (§4–§9) to a second runtime family without forking the Java path.

## The question

Can Brewlet support runtimes other than the JVM — .NET, Node.js, Python — and which
one makes the most sense to support first, besides Java?

## The answer, up front

**Yes, and .NET (framework-dependent deployment) is the clear first target.** It is
the closest conceptual twin of the fat-JAR model Brewlet is built around, so it
reuses the existing artifact/provisioning/shim/admission machinery with the least
distortion. Node.js and Python remain viable later but are a worse fit for the
current design's assumptions.

## What Brewlet's model actually requires

Brewlet is not really "a way to run Java" — it is a way to run a **portable
application payload against a shared, platform-owned runtime**, with no base image.
Concretely, the model rests on four properties (see §1, §4, §5, §6):

1. **A shared, read-only, separately-patchable runtime root on the node** — today a
   JDK under `/opt/brewlet/jdks/<dist>-<feature>/`, installed via copy-from-image and
   upgraded independently of apps (§5.3, [`jdk-management.md`](jdk-management.md)).
2. **A portable, arch-neutral app payload** shipped as an OCI *artifact* with no OS
   layer — today a fat JAR + a small launch-config JSON (§4).
3. **A canonical launch invocation** the shim rewrites the OCI spec into — today
   `java -jar /app/app.jar` (§6.1).
4. **A container/cgroup-aware runtime** that self-sizes from the sandbox limits, so
   Brewlet injects **no tuning flags** (§4.2, [`resource-tuning.md`](resource-tuning.md)).

Whichever candidate matches those four properties most cleanly should go first.

## Fit analysis

| Property | Java (baseline) | **.NET** | Node.js | Python |
|---|---|---|---|---|
| Shared, patchable node runtime root | JDK | .NET shared runtime (`dotnet`) — official images, copy-from-image works | `node` binary — works | interpreter — works, but ABI-fragmented |
| Portable, arch-neutral app payload | fat JAR (bytecode) | **framework-dependent `.dll` set (IL) — 1:1 analog of the fat JAR** | JS files, but `node_modules` may carry arch-specific native `.node` addons | `.py` + site-packages; C-extension wheels are arch + CPython-ABI specific |
| Canonical launch | `java -jar app.jar` | `dotnet app.dll` | `node app.js` / `node .` | `python -m app` / `python app.py` |
| Cgroup/container-aware self-tuning | strong | strong (`DOTNET_*`; GC honors cgroup limits) | weak (heap needs `--max-old-space-size`) | weak |
| Clean runtime/app separation | strong | strong (framework-dependent = runtime lives on the node) | medium (deps bundled with the app) | weak (venv + native deps entangle with the interpreter) |

### Why .NET first

- **Framework-dependent deployment is the exact conceptual twin of the fat JAR:** a
  portable IL payload that runs against a runtime already present on the node. The
  "ship just your app, the platform owns and patches the runtime" story (§1, G1/G5)
  is identical.
- The runtime is **container-aware**, so the "inject no tuning flags" promise (§4.2)
  holds unchanged.
- Official runtime images support the same **copy-from-image** provisioning already
  used for JDKs (§5.3), and the `brewlet.sh/` label/annotation scheme (§5.2, §14)
  generalizes directly.
- `dotnet <app.dll>` is a single canonical invocation, mirroring `java -jar`, so the
  shim's arg-building and the launcher abstraction (`java`/`jaz`, [`launchers.md`](launchers.md))
  map over naturally.

### Why not Node.js or Python first

- **Node.js:** JavaScript itself is portable, but native addons in `node_modules` are
  architecture-specific, which breaks the clean arch-neutral single-artifact model
  (contrast [`multi-arch.md`](multi-arch.md)); and the runtime does not self-tune to
  cgroups, weakening the "no tuning flags" promise.
- **Python:** the worst fit for a first cut — C-extension wheels are architecture-
  *and* CPython-ABI-specific, dependencies frequently need system libraries or
  compilation, and venv/interpreter coupling blurs the runtime/app separation the
  whole model depends on.

Both remain plausible later; they are simply further from the current design's
assumptions and would demand more new machinery (native-dep handling, per-arch
artifacts) up front.

## Proposed approach — generalize, don't fork

The goal is to **generalize the runtime abstraction into a "runtime family"**
(`jvm`, `dotnet`) rather than clone the Java path, so that a third or fourth runtime
later is incremental. This threads through the spec as follows.

- **§2.2 / §4 — Artifact & family.** Introduce a runtime-family discriminator on the
  artifact and a .NET payload media type (e.g.
  `application/vnd.brewlet.dotnet.layer.v1+*`) alongside the existing
  `jvm.config.v1+json`. JVM artifacts stay byte-for-byte backward-compatible; a
  missing family defaults to `jvm`. The launch-config `entry.mode` set gains a
  `dll` mode for .NET.
- **§5 — Provisioning.** Extend the provisioner to install .NET runtime roots via
  copy-from-image under `/opt/brewlet/runtimes/dotnet-<version>/` and advertise
  per-capability scheduling labels (`brewlet.sh/runtime-family.dotnet`,
  `brewlet.sh/dotnet.<version>`), exactly mirroring the JDK inventory mechanism.
- **§6 — Shim.** In the `Create()` decorator, select the .NET runtime root by family/
  version and rewrite the OCI spec to `dotnet /app/app.dll`. Overlay assembly (shared
  RO runtime lower + per-container upper + app layer at `/app`) is unchanged.
- **§8 — Admission.** Match the requested runtime family/version against the ready
  fleet and inject nodeAffinity onto the new capability labels, mirroring
  `NoCompatibleJDK` with a generalized event (e.g. `NoCompatibleRuntime`).
- **§9 — CRD.** Decide between generalizing `JavaApplication` into a runtime-agnostic
  `Application` (with a `spec.runtime` discriminator) versus adding a sibling
  `DotnetApplication`. Recommendation: a generic `Application` kind, keeping
  `JavaApplication` as a deprecated alias to avoid a hard breaking change.
- **CLI & tooling.** Extend `brewlet push` to publish .NET artifacts; a `dotnet
  publish`/MSBuild helper would be the .NET analog of the Maven plugin (future).

## Roadmap (incremental todos)

1. Generalize the spec: introduce the runtime-family concept and relax §2.2 for .NET.
2. Add the family field + .NET media type to `internal/artifact` (JVM-compatible).
3. Generalize the launch/arg-building core to emit `dotnet <app.dll>` (`entry.mode: dll`).
4. Provisioner: install .NET runtime roots (copy-from-image) + capability labels.
5. Shim `Create()`: select the .NET runtime root and rewrite the OCI spec.
6. Admission: match family/version and inject nodeAffinity (`NoCompatibleRuntime`).
7. CRD: generic `Application` with `spec.runtime` (keep `JavaApplication` alias).
8. CLI: `brewlet push` for .NET artifacts; MSBuild/`dotnet publish` helper (future).
9. A demo .NET app + an e2e path analogous to `make demo` / `make e2e-linux`.

## Open decisions & non-goals

- **CRD naming** is the biggest API decision: generalize `JavaApplication`
  (cleaner long-term, but a breaking change) vs. a parallel kind (non-breaking, more
  surface area). Recommended: generic `Application` + `spec.runtime`, with
  `JavaApplication` retained as a deprecated alias.
- **Self-contained .NET deployments** (the runtime bundled into the app) are
  explicitly out of scope — they defeat the shared-runtime model, exactly as shading
  a JVM into a JAR would.
- Framework-dependent .NET still targets a RID for some native bits; the supported
  shape is the standard **portable** framework-dependent app.
