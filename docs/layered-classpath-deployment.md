# Layered classpath deployment

> **Status.** Design + **implemented in the PoC** (Phase A). The class-path runtime
> path — `entry.classPath`, the `classpath.layer.v1+tar` layer, and unpacking it to
> `/app/lib` — now ships in the reference CLI and shim; see the
> [implementation status](#12-implementation-status). This note answers
> [SPECIFICATION §16 Open Question #3](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#16-open-questions)
> ("Classpath/modular apps") from the **class-path** angle: how Brewlet ships an
> application as **several OCI layers split along dependency-stability boundaries**
> (dependencies vs. application code) instead of one opaque fat JAR.

It is the class-path counterpart to the [JPMS support note](jpms-support.md), which
covers the same layering idea for the **module path** (`-p`). Both resolve Open
Question #3; this one targets the far more common non-modular, class-path app.

---

## 1. TL;DR

- **A fat JAR is one opaque blob.** Every rebuild — even a one-line change to your own
  code — produces a brand-new archive with a brand-new digest, so the **whole thing**
  is re-pushed and re-pulled. The 200 MB of dependencies that did not change are
  shipped again anyway.
- **Brewlet already ships JARs as OCI layers**, so it can borrow the well-known
  "layered JAR" trick (Spring Boot `layers.idx`, Jib, Cloud Native Buildpacks, Docker
  layer caching): put the **stable dependencies** in their own layer(s) and the
  **volatile application classes** in another. Registries and the containerd content
  store then dedup the heavy dependency layers across versions *and* across apps by
  digest — only the small app layer moves on a typical rebuild.
- **The only new mechanism needed is the classpath layer the spec already reserved.**
  [SPECIFICATION §4.1](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#41-media-types) and
  [reference](reference.md#oci-media-types) already list
  `application/vnd.brewlet.classpath.layer.v1+tar` as an *optional* layer "for
  classpath mode". This note designs it: a tar of JARs unpacked to `/app/lib`, driven
  by the existing `entry.mode: "classpath"` plus an optional `entry.classPath`.
- **This is additive and fully backward compatible.** A single fat JAR
  (`entry.mode: jar`) stays the default and the recommended path for most teams. Layer
  splitting is an opt-in optimization for large or frequently-rebuilt services.
- **Per-application `jlink`/`jmod` payloads remain out of scope.** Layering is
  about how application class files are packed, independent of the full JDK or
  shared jlink runtime selected from node inventory.

---

## 2. Background: the fat-JAR blob problem

### 2.1 What Brewlet ships today

Today a Brewlet artifact carries exactly **one** payload layer — the raw
self-executable JAR (`application/vnd.brewlet.jar.layer.v1+jar`,
`internal/artifact/artifact.go`). `Store.Push` writes a single `jarDesc` into
`Manifest.Layers`:

```go
m := Manifest{
    // …
    Layers: []Descriptor{jarDesc},   // one layer, the whole fat JAR
}
```

A fat JAR (Spring Boot `bootJar`, Maven Shade, Gradle Shadow) inlines the application
**and every dependency** into that one archive. Because the layer digest is the SHA-256
of the whole archive, changing a single application class changes the digest of the
entire dependency payload too.

### 2.2 Why that hurts at scale

| Symptom | Cause |
|---|---|
| **Full re-push every build** | One layer, one digest — no sub-part can be deduped. A 5 KB code change re-uploads 150–250 MB. |
| **Full re-pull on the node** | The containerd content store dedups by layer digest; a new fat-JAR digest is a cache miss, so the shim fetches the whole thing again. |
| **No cross-app sharing** | Two services on the same Spring Boot BOM still store their dependencies twice — the bytes are identical but buried in different fat-JAR digests. |
| **Slow cold pulls** | Startup latency ([§13](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md)) includes pulling the artifact; a smaller changed-layer pull is a faster cold start. |

### 2.3 Prior art: layered JARs

The container ecosystem solved this years ago by **splitting along change frequency**:

- **Spring Boot layered jars** write a `layers.idx` that groups content into
  `dependencies`, `spring-boot-loader`, `snapshot-dependencies`, and `application`,
  precisely so a Docker build caches the first three and rebuilds only the last.
- **Jib** and **Cloud Native Buildpacks** do the same automatically: a dependencies
  layer, a resources layer, and a classes layer.
- **Docker/OCI layer caching** then dedups every unchanged layer by digest.

Brewlet artifacts are *already OCI layers*. Adopting the same split gives the same
dedup for free — without a Dockerfile.

---

## 3. Alignment with the Brewlet model

Brewlet's thesis: ship only the developer's bytecode; the JDK installation lives on the node,
shared and patched centrally (see the [project landing page](/) and
[concepts](concepts.md)). Layered classpath deployment fits cleanly:

- **Still only bytecode.** The layers contain JARs — application classes and library
  JARs — and nothing else. No OS, no JVM. The node JDK still does the launching.
- **Same mount, same isolation.** Layers are unpacked into the same read-only `/app`
  tree the shim already assembles ([§6.1](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#61-per-container-lifecycle-create)).
  Nothing about the runc sandbox, cgroup mapping, RuntimeClass, or admission path
  changes.
- **Dedup is a registry/containerd property, not a runtime one.** Splitting into layers
  changes only *how the bytes are packed and addressed*, so the win is realized entirely
  by the registry and the node's content store — exactly the layers Brewlet already
  leans on.

**Conclusion:** Brewlet should support an **optional, ordered set of classpath layers**
that unpack to `/app/lib`, launched via the existing class-path entry mode. This is the
concrete design for the `classpath.layer.v1+tar` media type the spec already names.

---

## 4. Capability matrix

| Capability | Without layering | With layering | Notes |
|---|---|---|---|
| Fat JAR (`java -jar`) | ✅ | ✅ | `entry.mode: jar` (default, unchanged). |
| Class-path main, single JAR (`java -cp app.jar Main`) | ✅ | ✅ | `entry.mode: classpath` (unchanged). |
| **Thin app JAR + dependency layer(s)** (`java -cp app.jar:lib/* Main`) | ❌ | ✅ | New: optional classpath layer(s) → `/app/lib` (§5–§6). |
| **Multiple ordered layers** (deps / snapshot-deps / app) | ❌ | ✅ | Each tar is its own OCI layer; dedup per layer (§7). |
| Cross-app dependency dedup | ❌ | ✅ | Identical dependency layer digest is stored once. |
| Explicit classpath ordering | ❌ | ✅ | `entry.classPath` array (§5). |
| Module path layering (`-p /app/mods`) | ❌ | ✅ | Covered by the [JPMS note](jpms-support.md); parallel mechanism (§8). |
| Shared `NodeProfile` jlink runtime | N/A | N/A | Supported independently as node inventory. |
| Per-application `jlink` runtime | ❌ | ❌ (by design) | Duplicates the JVM in the artifact. |
| `jmod` packaging | ❌ | ❌ (by design) | Build-time only. |

---

## 5. Launch-config change: `entry.classPath` (implemented)

Extend the launch config ([SPECIFICATION §4.2](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#42-launch-config-config-blob-schema)
/ [reference](reference.md#launch-config-schema-config-blob)) with an optional
`entry.classPath` used by the existing `classpath` mode. **Fully backward compatible** —
existing `jar`/`classpath` configs omit it and behave exactly as today.

```json
{
  "schemaVersion": 1,
  "mainJar": "app.jar",
  "entry": {
    "mode": "classpath",              // "jar" | "classpath" | "module"
    "mainClass": "com.acme.orders.Main",
    "classPath": ["app.jar", "lib/*"] // optional; ordered, resolved under /app
  }
}
```

Resolution rules:

| Config | Emitted command |
|---|---|
| `mode: classpath`, no `classPath` | `java -cp /app/app.jar com.acme.orders.Main` *(today)* |
| `mode: classpath`, `classPath: ["app.jar","lib/*"]` | `java -cp /app/app.jar:/app/lib/* com.acme.orders.Main` |
| `mode: classpath`, `classPath: ["app.jar","lib/a.jar","lib/b.jar"]` | `java -cp /app/app.jar:/app/lib/a.jar:/app/lib/b.jar com.acme.orders.Main` |

- Each entry is resolved relative to `/app`; entries are joined with the platform path
  separator (`:` on Linux nodes) **in the order given** (class-path order is
  significant).
- `lib/*` uses the JVM's built-in class-path wildcard, which expands to every `*.jar`
  in `/app/lib` (non-recursive) — the same convention Spring Boot's `PropertiesLauncher`
  and ordinary `java -cp 'lib/*'` use. This keeps the config stable even as the exact
  set of dependency JARs changes.
- When `classPath` is omitted, `classpath` mode falls back to today's single-`mainJar`
  behavior.

The launch core change is confined to the `classpath` case of the existing
`switch cfg.Entry.Mode` in `BuildJVMArgs` (`internal/runtime/launch.go`):

```go
case "classpath":
    if cfg.Entry.MainClass == "" {
        return nil, fmt.Errorf("entry.mode=classpath but entry.mainClass is empty")
    }
    cp := jarPath                                   // default: just the main jar
    if len(cfg.Entry.ClassPath) > 0 {
        cp = resolveClassPath(cfg.Entry.ClassPath)  // join under /app in order
    }
    args = append(args, "-cp", cp, cfg.Entry.MainClass)
```

No shim isolation, provisioner, RuntimeClass, operator, or resource→JVM mapping change
is required — this is an **argv-and-artifact-layout** concern, the layer Brewlet already
owns.

---

## 6. Artifact format: ordered classpath layers

### 6.1 Layer media type

Give the reserved media type its meaning: an
`application/vnd.brewlet.classpath.layer.v1+tar` layer is a **tar of JAR files** that
the shim unpacks under `/app/lib`. An artifact may carry **zero or more** such layers,
in manifest order.

```go
// /internal/artifact/artifact.go
const ClasspathLayerMediaType = "application/vnd.brewlet.classpath.layer.v1+tar"
```

`Store.Push` gains an optional list of classpath-layer tars appended to
`Manifest.Layers` after the main JAR layer, each written as its own blob (its own
digest → its own dedup unit). The thin application JAR keeps the existing
`jar.layer.v1+jar` type so `Manifest.JarLayer()` still resolves the entry point.

### 6.2 Mounting & launch

```
/app/app.jar            # thin application JAR  (jar.layer.v1+jar)  — changes every build
/app/lib/               # dependency JARs        (classpath.layer.v1+tar layers)
  ├── spring-core-6.1.10.jar
  ├── jackson-databind-2.17.1.jar
  └── … (rarely change)
```

```bash
java -cp /app/app.jar:/app/lib/* com.acme.orders.Main
```

The shim's [§6.1](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#61-per-container-lifecycle-create) rootfs
assembly gains one step: after mounting the JAR layer at `/app`, unpack each
`classpath.layer.v1+tar` into `/app/lib` (read-only, shared like the JAR). Everything
else — overlayfs, cgroups, CNI, signals — is unchanged.

### 6.3 Layer split strategy

Mirror Spring Boot's `layers.idx` ordering, **stable → volatile**, so a change high in
the stack invalidates as few layers as possible:

| Layer (tar) | Typical contents | Change frequency |
|---|---|---|
| `deps` | released third-party dependencies | rarely (dependency bumps) |
| `snapshot-deps` | `-SNAPSHOT` / internal libs | occasionally |
| `app` *(the `jar.layer`)* | your compiled classes + resources | every build |

Because each tar is a separate OCI layer with its own digest, a build that only touches
application code re-pushes **only the `app` JAR layer**; the `deps` layers are already
present in the registry and the node content store.

---

## 7. Cache & dedup behavior

What actually moves over the wire on a rebuild:

| You change… | Layers with a new digest | Re-pushed / re-pulled |
|---|---|---|
| Only your own classes | `app` (jar layer) | just the small app JAR |
| Bump one released dependency | `deps` + `app` | dependency layer + app JAR |
| Bump an internal `-SNAPSHOT` | `snapshot-deps` + `app` | snapshot layer + app JAR |
| Nothing (re-tag) | none | nothing — full dedup |

Two services built on the same dependency set produce the **same `deps` layer digest**,
so the registry and every node store it once. This is the identical property that makes
Docker base-image layers cheap — Brewlet gets it without a base image.

> **Determinism caveat.** The dedup win depends on **reproducible layer tars**: stable
> file ordering and zeroed/pinned timestamps inside the tar, so unchanged inputs yield
> an identical digest. The layer builder (CLI / Maven plugin) must normalize tar
> entries — the same discipline Jib and reproducible-build Maven plugins already apply.

---

## 8. Relationship to the JPMS `mods` layer

This note and the [JPMS note](jpms-support.md) describe **the same layering mechanism
pointed at two different launch surfaces**:

| | Class path (this note) | Module path (JPMS note) |
|---|---|---|
| Layer media type | `…classpath.layer.v1+tar` (spec-reserved) | `…modulepath.layer.v1+tar` (defined in JPMS §6.1) |
| Unpack dir | `/app/lib` | `/app/mods` |
| Entry mode | `classpath` + `entry.classPath` + `entry.mainClass` | `module` + `entry.modulePath` + `entry.module` |
| Launch flag | `-cp /app/app.jar:/app/lib/*` | `-p /app/mods:/app/app.jar` |
| Applies to | non-modular / mixed apps (the common case) | JPMS modular apps |

They can share one implementation: a generic "tar-of-JARs layer that unpacks to a
well-known dir", parameterized by target dir and whether the resulting dir feeds `-cp`
or `-p`. Implementing either first makes the other nearly free.

### 8.1 Mixed class path + module path (implemented)

A modular (JPMS) app frequently needs **both** a module path (`-p`) and a
supplementary class path (`-cp`) — e.g. a JPMS application that also depends on
automatic-module or non-modular libraries that are cleanest to carry on the class
path. Brewlet supports this **mixed form** by keeping `entry.mode: module` and
additionally permitting `entry.classPath`:

```jsonc
{
  "schemaVersion": 1,
  "mainJar": "orders.jar",
  "entry": {
    "mode": "module",
    "module": "com.acme.orders",
    "modulePath": ["orders.jar", "mods"],   // -> /app/orders.jar:/app/mods  (-p)
    "classPath": ["lib/*"]                    // -> /app/lib/*                (-cp)
  }
}
```

The launcher emits the supplementary class path **before** the module path so the
terminal `-m <module>` (after which everything is a program argument) stays last:

```
java -cp /app/lib/* -p /app/orders.jar:/app/mods -m com.acme.orders[/<mainClass>]
```

Both a `classpath.layer.v1+tar` layer (→ `/app/lib`) and a
`modulepath.layer.v1+tar` layer (→ `/app/mods`) may be shipped simultaneously.
With the CLI this is `brewlet push app.jar ref --classpath-layer deps.tar
--module-layer mods.tar` on a modular JAR (auto-detected `entry.classPath=["lib/*"]`),
or any config supplied via `--config`. This closes SPECIFICATION §16 Open
Question #3 for the mixed case.

---

## 9. Tooling implications

- **`brewlet` CLI (`push`).** *Implemented:* `--classpath-layer TAR` (repeatable)
  attaches pre-built dependency tars as `classpath.layer.v1+tar` layers next to the
  thin `jar.layer`; `brewlet inspect` lists every layer with its media type and digest
  so dedup is visible. *Still to add:* an opt-in split that builds those tars for you
  (e.g. `--layer deps=./lib --thin`). Parsing a framework layering manifest such as
  Spring Boot's `layers.idx` is a **non-goal** (see [§10 Non-goals](#non-goals)); the
  generic classes/deps split does not need it — see the [PetClinic interop
  walkthrough](spring-petclinic.md#mapping-any-frameworks-layered-output).
- **Maven plugin.** *Implemented:* setting `<layered>true</layered>` (or
  `-Dbrewlet.layered=true`) packs the project's resolved transitive dependency tree
  (`project.getArtifacts()`, compile+runtime scope) into reproducible
  `classpath.layer.v1+tar` layers next to a thin app JAR, split `deps` /
  `snapshot-deps` (`brewlet.splitSnapshotLayers`, default on), and emits
  `entry.mode=classpath` with `entry.classPath=["<mainJar>","lib/*"]` and the derived
  `entry.mainClass` (`Start-Class`/`Main-Class` or `<mainClass>`). Works for both
  `brewlet:build` (local OCI layout) and `brewlet:push`. `JarInspector`
  (`brewlet-maven-plugin/.../util/JarInspector.java`) supplies the main class.
  For a **modular** project the same `<layered>true</layered>` flag instead packs
  the resolved runtime module dependencies into a single `modulepath.layer.v1+tar`
  layer (`/app/mods`) and emits `entry.mode=module` with
  `entry.modulePath=["<mainJar>","mods"]` — see [JPMS support](jpms-support.md).
  (The plugin's split is driven by the resolved POM dependency tree, not by any
  framework layering manifest; consuming a Spring Boot `layers.idx` is a non-goal.)
- **ORAS (manual).** The multi-layer form is a plain multi-layer OCI push:

  ```bash
  oras push registry.example.com/team/app:1.4.2 \
    --artifact-type application/vnd.brewlet.app.v1+json \
    --config   jvm-config.json:application/vnd.brewlet.jvm.config.v1+json \
    app.jar:application/vnd.brewlet.jar.layer.v1+jar \
    deps.tar:application/vnd.brewlet.classpath.layer.v1+tar \
    snapshot-deps.tar:application/vnd.brewlet.classpath.layer.v1+tar
  ```

- **Docs.** The "layered (thin JAR) apps" example lives in
  [building & publishing](building-and-publishing.md#layered-thin-jar-apps) and the
  `entry.mode` / media-type rows in [reference](reference.md).

---

## 10. Recommendation & suggested phasing

1. **Phase A — `entry.classPath` on the existing `classpath` mode (small). ✅ Implemented.**
   `BuildJVMArgs` builds a multi-entry `-cp` from `entry.classPath`, and any
   `classpath.layer.v1+tar` is unpacked to `/app/lib` in the sandbox/bundle assembly.
   This enables thin-JAR + dependency-layer deployment today (see
   [§12](#12-implementation-status)).
2. **Phase B — multi-layer authoring in the tooling.** *Partly done:* the CLI accepts
   pre-built dependency tars via `--classpath-layer` (repeatable), and the Maven plugin
   auto-splits the resolved dependency tree into reproducible, normalized tars. Still to
   do: an opt-in CLI split from a plain `lib/` directory. Automatic splitting is driven by
   generic inputs (a resolved dependency set / a `dependency:copy-dependencies` output),
   **not** by parsing a framework layering manifest like Spring Boot's `layers.idx` — that
   remains a non-goal (below).
3. **Phase C — unify with the JPMS `mods` layer (§8).** Share one tar-layer
   implementation across `/app/lib` (`-cp`) and `/app/mods` (`-p`), closing Open
   Question #3 for both class-path and module-path apps.

### Non-goals

- **Not a reproducible-build guarantee for your JARs.** Brewlet normalizes the *layer
  tar*; reproducibility of the JAR contents themselves is your build's responsibility.
- **No isolation, provisioner, RuntimeClass, or resource-mapping changes.** Layering is
  purely artifact layout + argv.
- **Per-application `jlink`/`jmod` payloads stay out of scope.** Shared
  `NodeProfile` jlink runtimes are independent of artifact layering; see the
  [JPMS note §2.2](jpms-support.md).
- **The fat JAR stays first-class.** Layering is opt-in; small or infrequently-rebuilt
  services should keep shipping a single `entry.mode: jar` artifact.
- **No framework-specific layering-manifest parsing.** Brewlet does **not** read Spring
  Boot's `layers.idx` (or any other framework's layering scheme) in the CLI or Maven
  plugin — supporting one would oblige it to support all. Brewlet's contract is the
  generic `classpath.layer.v1+tar` format; frameworks that emit layered output map onto
  it with generic, structural steps (see the
  [PetClinic interop walkthrough](spring-petclinic.md#mapping-any-frameworks-layered-output)).

---

## 11. References

- [Spring Boot layered jars & `layers.idx`](https://docs.spring.io/spring-boot/reference/packaging/efficient.html)
- [Google Jib](https://github.com/GoogleContainerTools/jib) and
  [Cloud Native Buildpacks](https://buildpacks.io/) — automatic dependency/app layering
- `java` launcher class-path wildcard (`-cp 'lib/*'`)
- Brewlet: [SPECIFICATION §4 (artifact)](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#4-the-oci-application-artifact),
  [§16 Open Questions](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#16-open-questions),
  [JPMS support](jpms-support.md),
  [building & publishing](building-and-publishing.md),
  [reference](reference.md)

---

## 12. Implementation status

Phase A (the class-path runtime) and part of Phase B (pre-built layer authoring) ship
in the PoC:

| Piece | Where |
|---|---|
| `entry.classPath` field + `classpath.layer.v1+tar` media type | `internal/artifact/artifact.go` |
| Multi-layer push / resolve (`PushWithLayers`, `Manifest.ClasspathLayers`) | `internal/artifact/artifact.go` |
| Layered `-cp /app/app.jar:/app/lib/*` argv | `BuildJVMArgs` in `internal/runtime/launch.go` |
| Unpack dependency tars to `/app/lib` (local run + runc bundle) | `AssembleSandbox` / `GenerateBundleWithLauncher` in `internal/runtime` |
| Shim resolves dependency-layer blob paths (layout + containerd) | `shim/cmd/containerd-shim-brewlet-v2/{resolver,bundle_prepare}.go` |
| CLI `push --classpath-layer TAR` (repeatable); `run`/`bundle` wire it | `cmd/brewlet/main.go` |
| Maven plugin model parity (`Entry.classPath`, media-type constant) | [`brewlet/maven-plugin`](https://github.com/brewlet/maven-plugin): `src/main/java/sh/brewlet/maven/plugin/{model,oci}` |
| Maven plugin **auto-splits the POM dependency tree** into reproducible `classpath.layer.v1+tar` layers (`brewlet.layered`) | [`brewlet/maven-plugin`](https://github.com/brewlet/maven-plugin): `src/main/java/sh/brewlet/maven/plugin/util` |
| **Map a Spring Boot repackaged layered JAR onto generic classpath layers** (thin app JAR + per-group dependency layers, via structural steps — *no `layers.idx` parsing*) | [`brewlet/integration-tests`](https://github.com/brewlet/integration-tests/blob/main/fixtures/spring-petclinic/layered-build.sh) fixture |

Verified end-to-end: a thin `app.jar` (application classes only) loads a class from a
dependency JAR delivered in a `classpath.layer.v1+tar` and unpacked to `/app/lib`.
The [Spring PetClinic example](spring-petclinic.md#layered-classpath-redeploy-only-your-business-code)
additionally proves the value on a real ~63 MB app: rebuilding only the business code
changes just the ~390 KB app-JAR layer while the dependency layer's digest is reused
(deduped, not re-pushed), and the layered artifact runs through shim → runc as
`java -cp app.jar:lib/* <MainClass>` under cgroups. See e2e **Tier 7**.

**Not yet implemented (Phase B/C):** an opt-in CLI split from a plain `lib/` directory
— the PetClinic split is a standalone reference script, not a `push` flag.

(The **mixed `-cp` + module-path form**, and pairing a `classpath.layer.v1+tar` with
the JPMS `modulepath.layer.v1+tar` in a single artifact, now ship — see
[JPMS support §6.3](jpms-support.md#63-mixed-class-path--module-path-implemented)
(`entry.mode: module` + `entry.classPath`, closing SPECIFICATION §16 Q3).)

**Explicit non-goal:** parsing a framework layering manifest (Spring Boot's
`layers.idx`) inside the CLI or Maven plugin — the generic classes/deps split covers
the interop without it (see [§10 Non-goals](#non-goals)).
