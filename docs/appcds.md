# AppCDS (Application Class Data Sharing) — implementation guide

> **Status.** **AppCDS is fully implemented and ships today.** Brewlet supports
> both build-time archive delivery (`application/vnd.brewlet.cds.layer.v1+jsa`,
> `brewlet push --appcds-archive`, and Maven `brewlet:appcds`) and node-side
> regeneration (`spec.jvm.cds.regenerate`, `brewlet run/bundle --appcds-regenerate`).
> The launch path uses `-Xshare:auto -XX:SharedArchiveFile` across local `run`,
> `bundle`, and the production shim, so mismatches safely fall back while preserving
> correctness. This document describes the shipped behavior, constraints, and design
> rationale.

---

## 1. TL;DR

- **Startup is the JVM's weakness vs. Wasm (§13), and CDS is the cheapest win.**
  Class-data sharing memory-maps a pre-parsed archive of loaded classes, cutting
  class-load/verify time (typically ~10–40% off startup) with no code change.
- **Brewlet already gets the *base* JDK CDS for free.** The JDK's default archive
  (`classes.jsa`) ships inside the node JDK root, which Brewlet mounts read-only as
  the overlay lower layer (`setupOverlayRootfs`). It's shared across every sandbox
  on the node and warm in the page cache — a genuine, already-realized advantage.
- **What's missing is *application* CDS** — an archive covering the app's own and
  its libraries' classes. That needs a **training run** at build time and an
  archive shipped alongside the JAR.
- **The sharp constraint: an AppCDS archive is bound to the exact JDK build (and
  classpath layout).** The binding is to the JVM's *full build identity* — not the
  feature release, not even the minor/patch version, but the exact
  version+build+date+compiler string. **Even a minor patch update (e.g.
  21.0.1 → 21.0.2) invalidates the archive**, and this applies equally to *static*
  AppCDS and *dynamic* CDS — there is no patch-tolerant mode (verified, see §2.1).
  Brewlet's whole point is patching the node JDK centrally and independently — which
  therefore *invalidates* a shipped archive on every patch. CDS fails **safe**
  (ignores a stale archive and runs normally), so it's a *performance* optimization
  that degrades gracefully, never a correctness risk — but the win **silently
  evaporates after any JDK patch**. This tension deserves to be designed around
  (§4.3, §7).
- **Recommended surfacing:** an optional artifact **archive layer** + a launch-config
  flag, generated turnkey by the Maven plugin / CLI. Mostly an implementation
  detail exposed through tooling, not a new developer concept.

---

## 2. Background: the CDS ladder

| Level | What it covers | How produced | Brewlet status |
|---|---|---|---|
| **Base / default CDS** | JDK (`java.base` etc.) classes | Ships in the JDK (`lib/server/classes.jsa`) | **Already shared** via the node JDK root overlay. |
| **AppCDS (static)** | App + library classes | Training run: record a class list, then `-Xshare:dump` (or `-XX:ArchiveClassesAtExit` for dynamic) → `app.jsa` | **Shippable** as a `cds.layer.v1+jsa` layer (Phase A). |
| **Dynamic CDS (JDK 13+)** | App + library classes | Single run with `-XX:ArchiveClassesAtExit=app.jsa` | Easiest to generate; **shippable** (Phase A). |
| **Auto-create archive (JDK 19+)** | App + library classes | `-XX:+AutoCreateSharedArchive -XX:SharedArchiveFile=app.jsa` — used if valid, (re)created at exit if missing/JDK-stale | Basis for node-side regen (§4.3); needs JDK 21 floor. |
| **Auto-generation / AOT cache (JDK 24, JEP 483 Leyden)** | Classes (+ AOT-linked) | `-XX:AOTCacheOutput` / auto | Future; same shipping mechanics as AppCDS. |

Launch consumes an app archive with:

```bash
java -XX:SharedArchiveFile=/app/app.jsa -jar /app/app.jar
# dynamic CDS falls back to the base archive automatically if app.jsa is stale
```

Key JVM behavior: if the archive **doesn't validate** against the running JDK
(version/build mismatch) or the classpath differs, the JVM **logs a warning and
runs without it** — no failure. This safe-fallback is what makes shipping an
archive acceptable in Brewlet's centrally-patched-JDK world.

### 2.1 Verified: archives are bound to the exact build, not the minor version

A natural assumption is that an app archive survives a *minor patch* update
(e.g. 21.0.1 → 21.0.2). **It does not.** The archive header records
`VM_Version::internal_vm_info_string()` as `_jvm_ident` and the JVM compares it
verbatim on load. That string encodes version + build number + build date +
compiler, so any patch bump (or even a rebuild of the same version) mismatches.

Measured on two same-vendor patch builds (Temurin `25.0.1+8` → `25.0.3+9`), for
**both** static AppCDS (`-Xshare:dump`) and dynamic CDS (`-XX:ArchiveClassesAtExit`):

```
_jvm_ident expected: OpenJDK 64-Bit Server VM (25.0.3+9-LTS) ... built on 2026-04-21 ... with clang ...
             actual: OpenJDK 64-Bit Server VM (25.0.1+8-LTS) ... built on 2025-10-21 ...
warning: The shared archive file was created by a different version or build of HotSpot
```

- `-Xshare:auto` (Brewlet's default) → archive rejected, JVM falls back to base CDS
  and runs normally. You lose the *app*-archive win, nothing breaks.
- `-Xshare:on` → **fatal** (`Unable to use shared archive … Failed to initialize`).

There is no "patch-tolerant" application-CDS mode: the same `_jvm_ident` check
governs base, static-AppCDS, and dynamic archives alike. This is exactly why the
build-time archive must be treated as best-effort seed data and paired with
`-Xshare:auto` and/or node-side regeneration (§4.3, §7).

### 2.2 Minimum JDK version

Set the AppCDS feature floor at **JDK 21**:

- It is the LTS version Brewlet already defaults to when no pod annotation is
  present (`temurin-21`), so it introduces no new fragmentation.
- It is the lowest LTS that ships **`-XX:+AutoCreateSharedArchive`** (added in
  JDK 19) — the primitive the node-side regeneration design (§4.3) relies on to
  create/refresh the archive automatically when it is missing or JDK-stale.
- Everything below it in the ladder (static AppCDS since 10, dynamic CDS since 13)
  is available too, so build-time generation (§4.1–4.2) works unconditionally at 21.

JDK 17 works for **build-time-only** archives (static + dynamic CDS) but lacks
`AutoCreateSharedArchive`, so it cannot self-heal after a patch — reason enough to
prefer 21 as the gate.

---

## 3. Alignment with the Brewlet model

- **Base CDS is a Brewlet freebie worth advertising.** Because the JDK root is
  shared read-only and page-cache-warm, every sandbox on a node reuses the same
  mapped base archive. Container images that each carry their own JVM don't share
  this across pods on a node. Call it out in [resource-tuning](resource-tuning.md)
  / [observability](observability.md).
- **AppCDS fits the artifact model as an extra layer.** Brewlet already supports
  optional extra layers (the `classpath.layer.v1+tar`, `artifact.go`) that the shim
  stages/mounts (`mountClasspathLayers`). An archive layer is the same shape: an
  extra blob, bind-mounted into `/app`, referenced by a jvm arg.
- **The friction is JDK coupling.** Brewlet *deliberately* decouples the artifact
  from the JDK (the JDK lives on the node, patched centrally). An AppCDS archive
  re-introduces a coupling to the *exact* JDK build. This is the core design
  problem (§7), and it's why AppCDS should lean on safe-fallback and/or node-side
  regeneration rather than a hard artifact↔JDK pin.

**Conclusion:** in scope and valuable, but design it as a *best-effort accelerator*
that never becomes a correctness or scheduling constraint.

---

## 4. How to surface it

### 4.1 Build-time archive layer (recommended primary)

Add an **optional archive layer** to the artifact, e.g. media type
`application/vnd.brewlet.cds.layer.v1+jsa`, carrying `app.jsa`. Extend the launch
config with a small, backward-compatible hint:

```json
{
  "schemaVersion": 1,
  "mainJar": "app.jar",
  "entry": { "mode": "jar" },
  "cds": { "archive": "app.jsa", "mode": "dynamic" }   // optional (shipped in Phase A)
}
```

The shim mounts the archive read-only at `/app/<archive>` and `BuildJVMArgs`
prepends `-XX:SharedArchiveFile=/app/<archive>` (plus `-Xshare:auto`, the safe
default, so a mismatch falls back rather than fails). Ship a prebuilt archive with
`brewlet push --appcds-archive app.jsa` (the config `cds.archive` defaults from the
file's basename); the same layer flows through `run`, `bundle`, and the production
containerd shim.

**This is mostly an implementation detail surfaced through tooling** — the
developer points `--appcds-archive` at a prebuilt `.jsa`; the launch-config field is
machine-authored.

### 4.2 Turnkey generation in the Maven plugin / CLI

**Implemented (Maven).** The training run is the ergonomic hurdle, so the Maven
plugin automates it with a dedicated goal:

- **Maven plugin — `brewlet:appcds`:** stages a copy of the built JAR with a
  canonical mtime (§4.4), then runs a training JVM with
  `-XX:ArchiveClassesAtExit=<target>/brewlet/app.jsa` (dynamic CDS — one run, no
  class-list file) using a JDK 21+ runtime (the AppCDS floor, §2.2). App-intrinsic
  launch knobs (`enablePreview`, `addModules`, `addOpens`, `addExports`,
  `systemProperties`) are replayed so the training flags match production. Extra
  warmup arguments go through `-Dbrewlet.appcds.trainingArgs`, and
  `-Dbrewlet.appcds.timeoutSeconds` bounds the run. Feed the result into
  `brewlet:push`/`brewlet:build` with `-Dbrewlet.cdsArchive=target/brewlet/app.jsa`,
  which attaches it as the CDS layer. Bind the goal to `pre-integration-test` when
  the build already has a short startup/warmup path.

  The training layout mirrors how the artifact is pushed, so the recorded archive
  matches the shim's layout (verified: HotSpot validates each classpath/module-path
  entry by basename+size+mtime, not absolute path, and expands `lib/*` in sorted
  order — see §4.4):
  - **fat JAR** (`entry.mode=jar`): `-jar <mainJar>`.
  - **layered class-path** (`-Dbrewlet.layered=true`, non-modular): the resolved
    runtime dependencies are staged into `lib/` and trained with
    `-cp <mainJar>:lib/* <mainClass>`, matching `/app/lib`.
  - **JPMS module** (`-Dbrewlet.layered=true`, modular): dependencies staged into
    `mods/`, trained with `-p <mainJar>:mods -m <module>[/<mainClass>]`, matching
    `/app/mods`.

  Run the goal with the **same** `layered`/module settings you push with; every
  staged file (app JAR + deps) is mtime-pinned. The training JAR bytes must be
  identical to the pushed JAR (same resolved artifact, no rebuild between `appcds`
  and `push`).

- **CLI (`brewlet push --appcds`):** turnkey for a **fat JAR** — runs a
  self-terminating training JVM (`-XX:ArchiveClassesAtExit`) against a
  canonical-mtime copy of the JAR, then ships the resulting archive exactly like
  `--appcds-archive`. The training `java` is `--appcds-java` (a binary or a
  `JAVA_HOME` dir), else `$JAVA_HOME/bin/java`, else `java` on `PATH`; drive
  startup with repeatable `--appcds-arg` and bound the run with `--appcds-timeout`
  (seconds). It is mutually exclusive with `--appcds-archive`, `--classpath-layer`,
  and `--module-layer` — for layered class-path / JPMS module training use the
  Maven `brewlet:appcds` goal (which stages `lib/`, `mods/`), or ship a prebuilt
  archive with `--appcds-archive`.

Emit clear guidance that the training run should exercise startup paths (Spring
context refresh, etc.) for the archive to be worthwhile. Because the training JVM
must exit cleanly for dynamic CDS to write the archive, the default `exit` mode
requires a self-terminating app; for long-running servers, `brewlet:appcds
-Dbrewlet.appcds.mode=signal` starts the app, waits for a readiness signal
(`readyLog` regex / `readyHttp` poll / `readyDelaySeconds`), then sends `SIGTERM`
so the shutdown hook runs and the archive flushes (verified: a `SIGTERM`'d JVM with
`-XX:ArchiveClassesAtExit` produces an archive that maps cleanly under
`-Xshare:on`). Signal mode is Unix-oriented and needs the app to shut down
gracefully on `SIGTERM`.

### 4.3 Node-side regeneration (the durable answer for a patched fleet)

> **Status: implemented (Phase B).** Opt in per-**deployment** with the
> `spec.jvm.cds.regenerate` field on the `JavaApplication` CRD — the controller
> stamps the `brewlet.sh/cds-regenerate` pod annotation, which the shim reads. For
> local dev the `brewlet run` / `brewlet bundle` commands take an equivalent
> `--appcds-regenerate` flag (with or without a seed archive). Regeneration is a
> fleet/operational choice (it depends on your JDK patch cadence), so it lives in the
> deployment descriptor, not baked into the artifact digest. The decision engine
> lives in `internal/runtime/cds_regen.go`
> (`DecideCDSRegen`) and is wired into local `run`, `bundle`/e2e harness, and the
> production shim (`applyBrewletLaunch`). It is entirely best-effort: any failure
> degrades to base CDS and never fails a launch.

The verified patch-invalidation (§2.1) makes build-time generation structurally at
odds with Brewlet's core promise — *patch the node JDK once, patch everything*. A
build-time archive goes stale on the **next** central patch, and a `.jsa` is also
**arch-specific**, so shipping one breaks the "same artifact runs on any arch"
property. Node-side regeneration removes both problems by decoupling the archive
from the shipped artifact entirely.

The node generates/refreshes a per-`(artifact-digest, jdk-build)` archive lazily
and caches it under `<cacheDir>/<key>.jsa`, where `key = sha256(artifactKey|jdkBuild)`
(first 32 hex) and `cacheDir` defaults to `/opt/brewlet/cds` (`DefaultCDSCacheDir`,
overridable via the `BREWLET_CDS_CACHE` env var). Inside the sandbox the cache is
bind-mounted at `/run/brewlet/cds` (`InSandboxCDSDir`) — read-write for the elected
writer, read-only for everyone else. On a JDK patch the `<jdkBuild>` component of the
key changes, the old entry is ignored, and a fresh archive is produced on the next
launch — always matched to the running JVM and to the node's architecture. The
shipped archive (§4.1) becomes optional *seed* data (copied into the cache slot when
present and the slot is empty; `AutoCreateSharedArchive` revalidates and recreates it
if JDK-stale, so seeding is always safe).

**Built on `-XX:+AutoCreateSharedArchive` (JDK 19+, hence the JDK 21 floor).**
Launching with `-XX:+AutoCreateSharedArchive -XX:SharedArchiveFile=<cache>.jsa`
makes the JVM *use* the archive when it is valid and *(re)create* it at exit when
it is missing or JDK-stale — so regeneration is automatic and JDK-build-keyed with
no bespoke training tooling. **This flag is a *fatal* unrecognized-option error on
JDK < 19**, so the engine reads the JDK's build identity from `<jdkRoot>/release`
(falling back to `java -Xinternalversion`) and **gates regeneration on feature ≥ 19**
(`minRegenFeature`); an older JDK falls back to base CDS rather than emitting a flag
that would break the app.

The engine resolves one of four roles per launch (`RegenDecision.Role`):

- **consume** — a valid cached archive exists → `-Xshare:auto -XX:SharedArchiveFile=<cache>`.
- **write** — no archive yet and this launch wins the writer election → seed if a
  shipped archive is present, then `-XX:+AutoCreateSharedArchive -XX:SharedArchiveFile=<cache>`.
- **defer** — no archive and another writer is in flight → base CDS this time.
- **skip** — JDK unsupported or regeneration disabled → base CDS.

Two hard problems this design handles:

1. **Long-running servers don't exit.** `AutoCreateSharedArchive` only *writes* the
   archive at JVM exit; a service that runs for weeks never produces one during
   normal operation. In practice the archive is emitted on the first pod's **graceful
   shutdown / rollout** and is then warm for subsequent replicas — i.e. the win lands
   on the *second* rollout, not the first boot. (A dedicated short "training"
   invocation is the alternative, at the cost of extra plumbing and knowing when
   startup is "done".)
2. **Thundering herd.** A Deployment scaling to N cold replicas all miss the cache
   at once. The engine elects exactly one writer per key via an `O_EXCL` marker file
   (`<archive>.writer`); the rest take the **defer** role — start on base CDS and pick
   up the app archive on a later restart. A stale marker (older than `DefaultWriterTTL`
   = 10m) is reclaimable, so a crashed writer never wedges the key permanently.

The engine also evicts cache entries untouched for longer than `DefaultEvictTTL`
(14d) on a best-effort pass, and emits a best-effort node-local metric
(`brewlet_cds_archive_mapped{key,role}` as a textfile under `BREWLET_METRICS_DIR`)
so operators can watch archive hits/rebuilds — the minimal Option A hook described in
[metrics-exporter.md](metrics-exporter.md). Running app code to produce archives on
the node is inherent to this mechanism; it runs inside the same sandbox as the app.

**Verified end to end in a real cluster.** [e2e Tier 8](https://github.com/brewlet/integration-tests/blob/main/README.md)
provisions a `kind` node for real (shim + full-userland `temurin-21` JDK root +
`brewlet` containerd runtime), deploys a genuine `runtimeClassName: brewlet` pod
with `brewlet.sh/cds-regenerate: true`, and asserts the full lifecycle: rollout 1
elects a **writer** (`-XX:+AutoCreateSharedArchive`), a graceful delete dumps the
`.jsa` into the node cache, and rollout 2 **consumes** it (`-Xshare:auto
-XX:SharedArchiveFile`, no AutoCreate) with the archive confirmed *mmap'd into the
JVM* via `/proc/1/maps` — a real CDS hit, not a silent fallback. Standing this up
also hardened the shim's CRI path (it now registers the `runtimeoptions` proto,
translates the generic options CRI hands a non-`runc` handler into `runc` options
preserving the cgroup driver, and skips the pod sandbox container) and added the
`pod_annotations = ["brewlet.sh/*"]` passthrough to the node provisioner so the
shim actually receives the artifact digest and the `cds-regenerate` toggle.

### 4.4 Deterministic JAR mtime — why a shipped archive maps on the node

**This is the subtle piece that makes §4.1/§4.2 actually work** (guarded by an
automated JDK integration test — `TestAppCDSTrainThenMapIntegration` in
`internal/runtime/cds_train_integration_test.go`, runnable via
`make appcds-verify`; it trains a real archive through the `push --appcds` code
path, then asserts it maps under `-Xshare:on` from a *different* directory with the
canonical mtime and is *refused* when the mtime drifts):

- CDS validates each classpath entry by **basename + file size + mtime** — *not* by
  directory or absolute path. An archive trained at `/x/traindir/app.jar` maps
  cleanly when consumed at `/y/appdir/app.jar` (or relative `app.jar`) **iff the
  JAR's size and mtime are unchanged**. So production's absolute `-jar /app/app.jar`
  launch needs no change; only the timestamp matters.
- Under Brewlet's default `-Xshare:auto`, an mtime/size mismatch is **silent**: the
  JVM drops the app archive and falls back to base CDS with no warning and no
  benefit (under `-Xshare:on` it is fatal). A naive `cp` (fresh mtime) is enough to
  make the archive inert.
- **The shim bind-mounts the app JAR straight from the content-store blob**, so the
  in-container `/app/app.jar` mtime is the node's *pull* time — never the build
  machine's. Without intervention, every build-time archive would be silently
  rejected on the node.

**Fix: normalize the JAR mtime to a canonical fixed value on both sides.** Both the
training run and the node stamp the JAR (and any staged `lib/`/`mods/` entries) with
`2000-01-01T00:00:00Z` = Unix second **946684800** (ns=0). This is a shared
invariant that must stay in lockstep:

- Go: `runtime.CDSModTime = time.Unix(946684800, 0).UTC()`
  (`internal/runtime/cds_mtime.go`).
- Java: `FileTime.from(Instant.ofEpochSecond(946684800L))`
  (`AppCdsMojo.CANONICAL_APP_MTIME`).

Normalization happens **only when the artifact ships a CDS archive**
(`cfg.CDS != nil`), so the common no-CDS path is byte-for-byte unchanged. On the
node side the JAR can't be re-timestamped on the shared read-only blob, so when CDS
is present the runtime copies the JAR into per-container staging, `chtimes` it to
the canonical value, and bind-mounts that copy; extracted `lib/`/`mods/` JARs are
pinned in place. All three JAR-materialization paths implement this identically:
`AssembleSandboxWithCDS` (`run`), `GenerateBundleWithCDS` (`bundle` + harness), and
the production shim's `applyBrewletLaunch`.

---

## 5. Resolved launch argv

| Config | Emitted (illustrative) |
|---|---|
| no `cds` | `java -jar /app/app.jar` (still uses base CDS from the JDK root) |
| `cds.archive: app.jsa` | `java -Xshare:auto -XX:SharedArchiveFile=/app/app.jsa -jar /app/app.jar` |
| dynamic + layered classpath | archive generated against the *same* `-cp /app/app.jar:/app/lib/*` layout it will run with |

`-Xshare:auto` (not `:on`) is deliberate: `on` would make a stale archive fatal;
`auto` preserves Brewlet's safe-fallback posture.

---

## 6. What existing features this touches

| Area | Interaction |
|---|---|
| **Artifact format (§4)** | New optional layer media type; append to `Manifest.Layers` beside the JAR/classpath layers (mirror `ClasspathLayers()`), dedup by digest in the registry/content store. |
| **Launch core (`BuildJVMArgs`)** | New optional `cds` block → prepend `-XX:SharedArchiveFile`; strict `Validate()`/`DecodeConfig` must learn the field (it currently `DisallowUnknownFields`). |
| **Shim (`service_linux.go`)** | Stage + bind-mount the archive at `/app/app.jsa` (reuse `StageClasspathLayers`/`mountClasspathLayers`). |
| **Layered classpath ([doc](layered-classpath-deployment.md))** | The archive is **classpath-sensitive**: it must be trained against the exact classpath order/wildcards it runs with. Document the pairing; `lib/*` wildcard has CDS rules. |
| **JDK matching (§5.3/§8.3)** | The archive is JDK-*build* specific, but Brewlet only matches on JDK *feature* and resolves the distribution per node. Do **not** turn CDS into a scheduling constraint — rely on `-Xshare:auto` fallback, or node-side regen (§4.3). |
| **CLI / Maven plugin (Phase 2)** | Add the training-run + attach flow. |
| **Multi-arch ([doc](multi-arch.md))** | A `.jsa` is arch-specific → shipping one archive breaks the "same artifact runs on any arch" property unless per-arch archives are shipped or archives are generated node-side. Prefer node-side/optional so the base artifact stays arch-independent. |

---

## 7. The JDK-coupling problem (design core)

Brewlet's value: *patch the node JDK once, patch everything.* An AppCDS archive is
validated against the JDK's exact build identity (not the feature or patch
version); after **any** patch it no longer applies (verified, §2.1).

Mitigations, in order of preference:

1. **Always `-Xshare:auto` + dynamic CDS.** A mismatch silently falls back to base
   CDS. Worst case: you lose the *app* archive benefit until the archive is
   regenerated; you never break the app. This alone makes shipping an archive safe.
2. **Node-side regeneration keyed on JDK build (§4.3), built on
   `-XX:+AutoCreateSharedArchive` (JDK 19+).** *(Implemented — opt in with
   `spec.jvm.cds.regenerate`.)* Fully decouples the artifact from the JDK build — the shipped
   archive is optional seed data, and the node self-heals the archive on the next
   patch. This is the durable fix; see §4.3 for the server-doesn't-exit and
   thundering-herd handling.
3. **Advertise archive staleness as a metric** (see [metrics-exporter.md](metrics-exporter.md)):
   count launches where the app archive failed to map, so operators can see the
   win eroding after a patch and trigger regeneration.

Explicitly **reject** turning the archive into a hard artifact↔JDK-build pin (e.g.
denying scheduling unless an exact JDK build is present) — that would resurrect the
per-image-JVM coupling Brewlet exists to remove.

---

## 8. Recommendation & phasing

1. **Phase A0 — document the freebie.** State clearly that base JDK CDS is already
   shared across sandboxes via the node JDK root. Zero code. *(Done.)*
2. **Phase A — build-time archive layer.** *(Done.)* The optional `cds.layer`
   (`application/vnd.brewlet.cds.layer.v1+jsa`) + `cds` launch-config block, the
   `/app/<archive>` read-only mount, and the `-Xshare:auto -XX:SharedArchiveFile=…`
   launch wiring ship across `run`, `bundle`, and the production shim; attach a
   prebuilt archive with `brewlet push --appcds-archive`. Deterministic JAR-mtime
   normalization (§4.4) makes the shipped archive actually map on the node — without
   it a build-time archive is silently inert under `-Xshare:auto`.
3. **Phase A′ — turnkey generation.** *(Done, Maven.)* The `brewlet:appcds` goal
   runs a training JVM (`-XX:ArchiveClassesAtExit`) with canonical-mtime inputs
   against the production layout — fat-JAR (`-jar`), layered class-path
   (`-cp app.jar:lib/*`), or JPMS module (`-p app.jar:mods -m …`) — on a JDK 21+
   runtime, and writes `target/brewlet/app.jsa` for
   `brewlet:push -Dbrewlet.cdsArchive=…` (§4.2). Long-running servers use
   `-Dbrewlet.appcds.mode=signal` (readiness → `SIGTERM`). A fat-JAR turnkey CLI
   equivalent, `brewlet push --appcds`, is also available (§4.2).
4. **Phase B — node-side regeneration.** *(Done.)* Opt in with the
   `spec.jvm.cds.regenerate` deployment field (or `brewlet run/bundle
   --appcds-regenerate` locally). The node caches
   per-`(artifact-digest, JDK-build)` archives under `/opt/brewlet/cds` using
   `-XX:+AutoCreateSharedArchive` (JDK 19+; hence the JDK 21 floor), regenerates on
   JDK patch (the build token is part of the cache key), elects a single writer per
   key to tame the thundering herd, evicts stale entries, and exposes an
   archive-hit/rebuild metric. This is what makes AppCDS robust in a
   centrally-patched-JDK fleet. **Caveat:** because `AutoCreateSharedArchive` only
   writes at JVM exit, the app-archive win lands on the *second* rollout of a
   long-running server, not the first boot (§4.3).

**Minimum JDK:** gate the `cds` feature on **JDK 21** (Brewlet's default LTS and the
lowest LTS with `AutoCreateSharedArchive`); JDK 17 is build-time-only, without
auto-regeneration (§2.2).

AppCDS is an **argv + optional-layer** concern — exactly the seam Brewlet already
owns for the classpath layer — plus a build-time training step. It delivers most of
the practical startup win at low risk, and is the recommended primary startup
accelerator for Brewlet.

---

## 9. References

- [JEP 310: Application Class-Data Sharing](https://openjdk.org/jeps/310),
  [JEP 350: Dynamic CDS Archives](https://openjdk.org/jeps/350).
- [JEP 483: Ahead-of-Time Class Loading & Linking (Leyden, JDK 24)](https://openjdk.org/jeps/483).
- `java` flags: `-XX:SharedArchiveFile`, `-XX:ArchiveClassesAtExit`, `-Xshare:auto|on|off`.
- Brewlet: [SPECIFICATION §13 (startup)](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#13-performance--startup),
  [§4 (artifact)](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#4-the-oci-application-artifact),
  [layered-classpath-deployment](layered-classpath-deployment.md),
  [multi-arch.md](multi-arch.md);
  `internal/runtime/launch.go`, `internal/artifact/artifact.go`,
  `shim/cmd/containerd-shim-brewlet-v2/service_linux.go`.
