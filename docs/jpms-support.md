# JPMS support & capabilities

> **Status.** Implemented (issue #52). This note documents the design and
> rationale for Brewlet's support of **modularized Java applications** (the Java
> Platform Module System, JPMS) via `entry.mode: "module"` and the optional
> `modulepath.layer.v1+tar` layer — resolving the module-path half of
> [SPECIFICATION §16 Open Question #3](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#16-open-questions)
> ("Classpath/modular apps"). The schema and launch behavior described here are
> live in the launch core, CLI, and Maven plugin.

Closes the research asked for in the "Research JPMS support and capabilities" issue.

---

## 1. TL;DR

- **Modular JARs are a natural fit for Brewlet.** JPMS moves *encapsulation and
  dependency metadata* into the artifact (`module-info.class`) but still runs on
  the node's shared JDK installation via `java --module-path … --module …`. It requires **no
  JVM baked into the artifact**, so it aligns with Brewlet's core model exactly the
  way a fat JAR does.
- **The only new mechanism needed is a launch mode.** Add an `entry.mode: "module"`
  to the launch config so the shim emits `java -p <module-path> -m <module>[/<mainClass>]`
  instead of `java -jar`. Single modular JARs need nothing else.
- **Multi-module apps need the "optional classpath/module layer"** already
  anticipated in the spec: a directory of JARs mounted at `/app/mods` that becomes
  the `--module-path`. This is a small extension of the artifact format, not a new
  runtime.
- **`jlink` custom runtime images and `jmod` files are explicitly out of scope.**
  A jlink image *bundles a JVM*, which is the exact thing Brewlet exists to remove
  from the artifact. Teams that truly need a self-contained runtime image should use
  an ordinary container instead (Brewlet is additive — see the project FAQ).

---

## 2. Background: fat JARs vs. JPMS

### 2.1 What a fat / uber JAR is

A "fat JAR" (Spring Boot `bootJar`, Maven Shade, Gradle Shadow) bundles the
application **and all its dependencies** into a single archive that runs on the
**class path**:

```bash
java -jar app.jar            # entry.mode = jar   (today's default)
java -cp app.jar com.acme.Main   # entry.mode = classpath
```

Everything lands in the *unnamed module*. There is no reliable dependency graph,
no strong encapsulation, and split packages are silently tolerated. This is what
Brewlet supports today (`internal/runtime/launch.go`, entry modes `jar` and
`classpath`).

### 2.2 What JPMS is

JPMS (JSR 376, "Project Jigsaw", since Java 9) introduces **named modules**. Each
module carries a `module-info.class` compiled from a `module-info.java`:

```java
module com.acme.orders {
    requires com.acme.commons;
    requires java.net.http;
    exports com.acme.orders.api;
    // opens com.acme.orders.model;   // reflection (e.g. Jackson)
    // uses / provides … with …;       // ServiceLoader
}
```

Modules run on the **module path** rather than the class path:

```bash
java --module-path mods --module com.acme.orders/com.acme.orders.Main
# short form:
java -p mods -m com.acme.orders/com.acme.orders.Main
```

If the module declares a `Main-Class` (via `jar --main-class …`), the launch
class can be omitted: `java -p mods -m com.acme.orders`.

Key properties JPMS adds over the class path:

| Property | Effect |
|---|---|
| **Reliable configuration** | Missing/duplicate modules fail fast at startup, not with a `NoClassDefFoundError` deep in a request. |
| **Strong encapsulation** | Only `exports`/`opens` packages are reachable; internal packages are hidden even via reflection unless `opens`. |
| **Explicit services** | `uses`/`provides` replaces `META-INF/services` scanning. |
| **Smaller, auditable surface** | The module graph is declared, so tooling (and `jlink`) can reason about exactly what is needed. |

### 2.3 Three things people mean by "modular"

These are frequently conflated; Brewlet treats them very differently:

| Term | What it is | Runs on node JDK? | Brewlet fit |
|---|---|---|---|
| **Modular JAR** | An ordinary JAR with a `module-info.class` at its root. | ✅ Yes — `java -p … -m …`. | **In scope** (this doc). |
| **`jmod` file** | A build/link-time package format (native libs, config, headers). **Not runnable** with `java -jar`/`-m`; consumed by `jlink`. | ❌ | Out of scope. |
| **`jlink` runtime image** | A *self-contained* directory tree that **includes a stripped JVM** plus the app modules; launched via its own `bin/java` or a jpackage app-image. | ❌ It *is* the runtime. | **Out of scope** — contradicts "the JDK lives on the node". |

---

## 3. Alignment with the Brewlet model

Brewlet's thesis: ship only the developer's bytecode; the JDK installation lives on the node,
shared and patched centrally (see the [project landing page](/) and
[concepts](concepts.md)). Testing each JPMS flavor against that thesis:

- **Modular JAR + module path — aligns perfectly.** The artifact is still just
  JAR bytes; the module path is resolved *against the JAR(s) we already mount at
  `/app`*, and the node JDK does the launching. Nothing about JPMS requires a
  bundled runtime. The only difference from today is the argv the shim assembles.
- **`jlink` image — anti-aligned.** A jlink image embeds a JVM. Shipping it would
  re-introduce exactly the "JVM copy in every artifact" that Brewlet removes, and
  it would ignore the node's shared, centrally-patched JDK. If a team wants a
  bespoke runtime image, that is a normal container workload, which Brewlet
  deliberately leaves alone (it only intercepts `runtimeClassName: brewlet`).
- **`jmod` — not a runtime artifact at all.** It only feeds `jlink`. No runtime
  support is meaningful.

**Conclusion:** Brewlet should support **modular JARs launched on the module
path**, and should *not* attempt to ship jlink/jmod images.

---

## 4. Capability matrix

| Capability | Without JPMS support | With JPMS support | Notes |
|---|---|---|---|
| Fat JAR (`java -jar`) | ✅ | ✅ | `entry.mode: jar` (default). |
| Class-path main (`java -cp`) | ✅ | ✅ | `entry.mode: classpath`. |
| Single **modular JAR** (`java -p app.jar -m mod`) | ❌ | ✅ | New `entry.mode: module`. |
| Multi-JAR **module path** (`-p /app/mods`) | ❌ | ✅ | Needs the optional module/classpath layer (§6). |
| Mixed class path + module path | ❌ | ✅ | `entry.classPath` + module path (§6.3). |
| Automatic modules (plain JAR on `-p`) | ❌ | ✅ | Works once a module path exists; name from `Automatic-Module-Name` or filename. |
| `--add-modules` / `--add-opens` / `--add-reads` | ✅¹ | ✅ | ¹ `--add-modules`, `--add-opens`, and `--add-exports` are first-class artifact fields (`addModules`, `addOpens`, `addExports`); `--add-reads` and other exotic flags go through descriptor `jvm.args`. |
| `jlink` runtime image | ❌ | ❌ (by design) | Use a container instead. |
| `jmod` packaging | ❌ | ❌ (by design) | Build-time only. |

---

## 5. Launch-config change: `entry.mode: "module"` (implemented)

Extend the launch config (SPECIFICATION §4.2 / [reference](reference.md)) with a
third entry mode. **Fully backward compatible** — existing configs omit it and get
`jar`.

```json
{
  "schemaVersion": 1,
  "mainJar": "orders.jar",
  "entry": {
    "mode": "module",              // "jar" | "classpath" | "module"
    "module": "com.acme.orders",   // required when mode == "module"
    "mainClass": null,             // optional; selects the module's main class (omit if declared)
    "modulePath": null             // optional; defaults to the app dir (see §6)
  }
}
```

Resolved argv:

| Config | Emitted command |
|---|---|
| `mode: module`, single `mainJar` | `java -p /app/orders.jar -m com.acme.orders` |
| `mode: module` + `mainClass` | `java -p /app/orders.jar -m com.acme.orders/com.acme.orders.Main` |
| `mode: module` + `modulePath: mods` | `java -p /app/mods -m com.acme.orders[/…]` |

The change to the launch core (`BuildJVMArgs` in `internal/runtime/launch.go`)
is a single new `case "module"` in the existing `switch cfg.Entry.Mode`, mirroring
the existing `jar`/`classpath` cases:

```go
case "module":
    if cfg.Entry.Module == "" {
        return nil, fmt.Errorf("entry.mode=module but entry.module is empty")
    }
    mp := modulePath(cfg, jarPath)      // modulePath override or the mounted jar/dir
    target := cfg.Entry.Module
    if cfg.Entry.MainClass != "" {
        target += "/" + cfg.Entry.MainClass
    }
    args = append(args, "-p", mp, "-m", target)
```

No shim, provisioner, RuntimeClass, or operator change is required for the
**single modular JAR** case — the artifact and mount path are unchanged; only the
argv differs.

---

## 6. Multi-module apps: the optional module/classpath layer

A real modular app is usually **several** JARs (the app module + library modules).
That is precisely the "optional classpath layer" the spec already anticipates
(Open Question #3). The minimal, model-preserving design:

### 6.1 Artifact format

Today the artifact carries one JAR layer
(`application/vnd.brewlet.jar.layer.v1+jar`, `internal/artifact/artifact.go`).
Add an **optional second layer** carrying a *directory of JARs* (a tar layer), e.g.
`application/vnd.brewlet.modulepath.layer.v1+tar`, that the shim unpacks to `/app/mods`.

> This tar-of-JARs layer is the **module-path twin** of the class-path
> `classpath.layer.v1+tar` designed in the
> [layered classpath deployment note](layered-classpath-deployment.md); the two can
> share one implementation (unpack a tar of JARs to a well-known dir, feeding `-p`
> here vs. `-cp` there). See that note's §8 for the mapping.

### 6.2 Mounting & launch

```
/app/orders.jar        # mainJar (the app module)
/app/mods/             # library modules (from the mods layer)
  ├── commons.jar
  └── json.jar
```

```bash
java -p /app/mods:/app/orders.jar -m com.acme.orders
```

`entry.modulePath` (when set) is resolved relative to `/app`; when unset it
defaults to `mainJar` for the single-JAR case, or `/app/mods` when a mods layer is
present.

### 6.3 Mixed class path + module path (implemented)

Some apps run library JARs on the class path and only the app (plus modular
dependencies) on the module path — e.g. a JPMS app that also depends on
automatic-module or non-modular libraries best carried on the class path. Brewlet
supports this by keeping `entry.mode: module` and additionally permitting
`entry.classPath`: the launcher emits `-cp <classPath>` **before**
`-p <modulePath> -m <module>` so the terminal `-m` stays last:

```
java -cp /app/lib/* -p /app/orders.jar:/app/mods -m com.acme.orders[/<mainClass>]
```

Both a `classpath.layer.v1+tar` (→ `/app/lib`) and a `modulepath.layer.v1+tar`
(→ `/app/mods`) may ship together. See
[layered deployment §8.1](layered-classpath-deployment.md) for the full example
and CLI usage. This closes SPECIFICATION §16 Open Question #3 for the mixed case.

### 6.4 JDK matching is descriptor-driven

Module resolution happens entirely inside the node JDK. The existing
`spec.jvm.version` / `brewlet.sh/jdk` matching (and `NoCompatibleJDK` admission
event) already covers "the module app needs JDK N"; nothing new is required. (One
caveat: `module-info.class` is versioned by the compiler `--release`, so the
descriptor should keep the requested feature ≥ the compile release.)

---

## 7. Tooling implications

The tooling implements module detection and layout end to end:

- **`brewlet` CLI (`push`).** When generating a launch config it detects a
  `module-info.class` at the JAR root (equivalently `jar --describe-module`
  succeeds with a named module); when present it defaults `entry.mode` to `module`
  and reads the module name and its `Main-Class`, falling back to `jar`/`classpath`
  otherwise (`cmd/brewlet/main.go`). This mirrors the manifest-based inference
  in the Maven plugin's `JarInspector` (`entryMode()`).
- **Maven plugin.** `JarInspector` detects a module descriptor (the JDK's
  `java.lang.module.ModuleDescriptor.read(...)` over the JAR's `module-info.class`)
  and sets `entry.mode=module` with the derived module name
  (`brewlet-maven-plugin/.../util/JarInspector.java`). The `layered=true` flag
  assembles the `mods` layer (`modulepath.layer.v1+tar`) from the project's
  resolved runtime module dependencies and sets `entry.modulePath=[mainJar, "mods"]`
  — the module-path twin of the layered classpath deployment.
- **Docs.** The "modular apps" example lives in
  [building & publishing](building-and-publishing.md#modular-jpms-apps) and the
  `entry.mode` row (`jar | classpath | module`) in [reference](reference.md).

> **Ship a modular JAR, not a `.jmod`.** A Maven build targeting Brewlet's
> `module` mode should produce an ordinary `.jar` that happens to contain a
> `module-info.class` at its root (the default output of a normal
> `maven-compiler-plugin`/`maven-jar-plugin` build with a `module-info.java` in
> the module). It should **not** produce a `.jmod` file (via the `jmod` tool) or
> a `jlink` runtime image (via `maven-jlink-plugin`/`jpackage`): `.jmod` is a
> link-time-only package format that `java --module-path`/`-m` refuses to run,
> and a `jlink` image bundles its own JVM — the exact thing Brewlet removes (see
> §2.3, §3). In short, the shippable JPMS artifact is a **modular JAR**.

---

## 8. Recommendation & suggested phasing

1. **Phase A — single modular JAR (small, high value). ✅ Implemented.**
   `entry.mode: "module"` with `entry.module` + optional `entry.mainClass`, the one
   `module` `case` in `BuildJVMArgs`, and CLI/Maven auto-detection of module JARs all
   ship. No artifact-format change; this alone lets single-module services ship.
2. **Phase B — module/classpath layer (multi-JAR). ✅ Implemented.** The optional
   `…/modulepath.layer.v1+tar` layer is unpacked to `/app/mods` and defaults the
   module path accordingly (attached via `brewlet push --module-layer` or the Maven
   `layered` flag), resolving the rest of Open Question #3.
3. **`jlink`/`jmod` are a non-goal** — a bundled runtime is what Brewlet removes; such
   workloads use ordinary containers (see §2.3 and SPECIFICATION §16 Q3).

None of this requires changes to the shim's isolation, the provisioner, the
RuntimeClass, or the resource→JVM mapping. JPMS is an **argv-and-artifact-layout**
concern, which is exactly the layer Brewlet already owns.

---

## 9. References

- [JEP 261: Module System](https://openjdk.org/jeps/261)
- [JSR 376: Java Platform Module System](https://openjdk.org/projects/jigsaw/spec/)
- `java` launcher: `--module-path` / `--module`, `jar --describe-module`
- [Layered classpath deployment](layered-classpath-deployment.md) — the
  class-path counterpart (thin JAR + dependency layers for registry dedup)
- Brewlet: [SPECIFICATION §4 (artifact)](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md), [§16 Open Questions](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#16-open-questions),
  [building & publishing](building-and-publishing.md), [reference](reference.md)
