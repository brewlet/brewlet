# Building & publishing application artifacts

This is the developer's half of Brewlet: turn your Java application into an **OCI
application artifact** and push it to a registry. There is no Dockerfile and no image
build — you ship the application payload (a fat JAR, or dependency + app classpath
layers) plus a small JSON launch config.

- Ops/cluster side: [Installation](installation.md) and [JDK management](jdk-management.md).
- Deploying what you publish here: [Deploying workloads](deploying-workloads.md).

---

## 1. Build your fat JAR (nothing Brewlet-specific)

Build a self-executable (fat/uber) JAR exactly as you do today:

```bash
mvn -q clean package          # → target/app.jar
# or
gradle bootJar                # → build/libs/app.jar
```

Brewlet runs it with the canonical `java -jar app.jar`, so Spring Boot, Quarkus,
Micronaut, JMX, OTel, JFR, and shutdown hooks all behave exactly as they normally
would.

---

## 2. The launch config

Every artifact carries a small JSON **launch config** describing how to run the JAR.
It is deployment-agnostic: the JDK feature and launcher are requested later in the
deployment descriptor. The `brewlet push` CLI generates a minimal config for you, or
you can author it and pass `--config`.

```json
{
  "schemaVersion": 1,
  "mainJar": "app.jar",
  "entry": { "mode": "jar" },
  "enablePreview": true,
  "addOpens": ["java.base/java.lang=ALL-UNNAMED"],
  "systemProperties": { "spring.aot.enabled": "true" },
  "user": { "uid": 1000, "gid": 1000 },
  "env": []
}
```

| Field | Meaning |
|---|---|
| `mainJar` | The JAR filename inside the artifact (mounted at `/app/<mainJar>`). |
| `entry.mode` | `jar` → `java -jar` (default); `classpath` → `java -cp <jar> <mainClass>`; `module` → `java -p <modulePath> -m <module>[/<mainClass>]` (JPMS). |
| `entry.mainClass` | Required when `entry.mode == "classpath"`; optional in `module` mode. |
| `entry.classPath` | Optional, ordered `/app`-relative class-path entries (e.g. `["app.jar", "lib/*"]`) used with `entry.mode == "classpath"` for layered deployment. |
| `entry.module` | Required when `entry.mode == "module"`; the root module name for `java -m`. |
| `entry.modulePath` | Optional, ordered `/app`-relative module-path entries (e.g. `["orders.jar", "mods"]`) fed to `java -p` in `module` mode; defaults to `mainJar`. |
| `enablePreview` | Optional app-intrinsic flag for code compiled with preview features; expands to `--enable-preview`. |
| `addModules` | Optional module names; expands to `--add-modules <comma-joined>`. |
| `addOpens` | Optional module/package access tokens such as `java.base/java.lang=ALL-UNNAMED`; expands to repeated `--add-opens`. |
| `addExports` | Optional module/package export tokens; expands to repeated `--add-exports`. |
| `systemProperties` | Optional string map expanded as sorted `-D<key>=<value>` flags. |
| `cds` | Optional AppCDS block. `cds.archive` is a bare `/app`-relative `.jsa` filename shipped as a CDS layer (`brewlet push --appcds-archive`); `cds.mode` (`dynamic`\|`static`) is informational. Launches with `-Xshare:auto -XX:SharedArchiveFile=/app/<archive>`, so a JDK-build mismatch falls back safely to base CDS. The artifact carries only this shipped *seed* archive; node-side regeneration is a deployment choice set via `spec.jvm.cds.regenerate` on the `JavaApplication` CRD (or `brewlet run/bundle --appcds-regenerate`), not a field in the artifact. See [AppCDS](appcds.md). |
| `user` | uid/gid to run as (also settable via pod `securityContext`). |
| `env` | Environment variables baked into the artifact. |

Ports are **not** an artifact field — they are a deployment concern
(`spec.ports` in the descriptor, or the Maven `manifest` goal's `<ports>`).

Artifact launch knobs are app-intrinsic correctness flags. They expand before
descriptor `jvm.args`, which is where deployment tuning and escape-hatch JVM args
belong.

The JDK and launcher are set once in the deployment descriptor: `spec.jvm.version`
and `spec.jvm.launcher` for `JavaApplication`, or pod annotations
`brewlet.sh/jdk` and `brewlet.sh/launcher` for raw Deployments.

> **Tuning is yours, not Brewlet's.** The container-aware JVM reads the sandbox
> cgroup limits directly; set heap/GC in descriptor `jvm.args` (or let `jaz` derive
> them). The artifact only carries app-intrinsic launch knobs. See [Resource tuning](resource-tuning.md).

### Class-path / non-`-jar` entry

For an app launched via a main class instead of `-jar`:

```json
{ "schemaVersion": 1, "mainJar": "app.jar",
  "entry": { "mode": "classpath", "mainClass": "com.example.Main" } }
```

This produces `java -cp /app/app.jar com.example.Main`.

### Modular (JPMS) apps

A modular JAR — an ordinary JAR with a root `module-info.class` — runs on the
**module path** (`java -p … -m …`) on the node's shared JDK installation, exactly like a fat
JAR runs on the class path. Set `entry.mode: "module"` with the module name:

```json
{ "schemaVersion": 1, "mainJar": "orders.jar",
  "entry": { "mode": "module", "module": "com.acme.orders" } }
```

This produces `java -p /app/orders.jar -m com.acme.orders` (the module's declared
`Main-Class` is used). Add `entry.mainClass` to launch a specific class
(`-m com.acme.orders/com.acme.orders.Main`).

**Auto-detection.** `brewlet push` and the Maven plugin detect a modular JAR
automatically and default to `entry.mode: module`, reading the module name (and
declared main class) from the descriptor — no config needed for a single modular
JAR.

**Multi-JAR module path.** Ship the app module plus its library modules as a
`modulepath.layer.v1+tar` layer (unpacked to `/app/mods`) and list the module
path explicitly:

```json
{ "schemaVersion": 1, "mainJar": "orders.jar",
  "entry": { "mode": "module", "module": "com.acme.orders", "modulePath": ["orders.jar", "mods"] } }
```

```bash
brewlet push ./target/orders.jar demo/orders:1.0 --module-layer mods.tar
```

produces `java -p /app/orders.jar:/app/mods -m com.acme.orders`.

> `jlink` runtime images and `jmod` files remain out of scope by design — they
> bundle a JVM, which is exactly what Brewlet removes. See the
> [JPMS support note](jpms-support.md).

### Layered (thin JAR) apps

Instead of one fat JAR, you can ship a **thin application JAR** plus one or more
**dependency layers** so registries and nodes dedup the unchanged dependencies and
only the small app layer moves per build. Set `entry.classPath` (ordered,
`/app`-relative) and attach the dependency JARs as a tar layer:

```json
{ "schemaVersion": 1, "mainJar": "app.jar",
  "entry": { "mode": "classpath", "mainClass": "com.example.Main", "classPath": ["app.jar", "lib/*"] } }
```

**Maven plugin (recommended).** Flip one flag — the plugin resolves the transitive
dependency tree, packs it into reproducible `classpath.layer.v1+tar` layers, ships a
thin app JAR, and writes the matching `entry.mode: classpath` / `entry.classPath`
config for you:

```bash
# One-off: enable layering on the command line
mvn clean package sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT:push \
  -Dbrewlet.image=registry.example.com/team/app:1.4.2 \
  -Dbrewlet.layered=true
```

```xml
<plugin>
  <groupId>sh.brewlet</groupId>
  <artifactId>brewlet-maven-plugin</artifactId>
  <version>0.1.0-SNAPSHOT</version>
  <configuration>
    <image>registry.example.com/team/app:${project.version}</image>
    <layered>true</layered>                         <!-- thin JAR + dependency layers -->
    <splitSnapshotLayers>true</splitSnapshotLayers>  <!-- deps vs. snapshot-deps (default) -->
  </configuration>
</plugin>
```

By default (`brewlet.splitSnapshotLayers=true`) released dependencies and internal
`-SNAPSHOT` dependencies land in separate `deps` / `snapshot-deps` layers (stable →
volatile) for finer dedup; set it to `false` to pack all dependencies into one layer.
See the [plugin README](https://github.com/brewlet/maven-plugin/blob/main/README.md#configuration-parameters)
for the full option reference.

**CLI.** Pre-build the dependency tar(s) yourself and attach them with
`--classpath-layer` (repeatable):

```bash
# lib/ holds your runtime dependency JARs (e.g. mvn dependency:copy-dependencies)
tar -cf deps.tar -C lib .
brewlet push ./target/app.jar demo/app:1.4.2 --config cfg.json --classpath-layer deps.tar
```

**ORAS.** The multi-layer form is a plain multi-layer OCI push — one
`classpath.layer.v1+tar` per dependency tar, in stable → volatile order:

```bash
oras push registry.example.com/team/app:1.4.2 \
  --artifact-type application/vnd.brewlet.app.v1+json \
  --config   jvm-config.json:application/vnd.brewlet.jvm.config.v1+json \
  target/app.jar:application/vnd.brewlet.jar.layer.v1+jar \
  deps.tar:application/vnd.brewlet.classpath.layer.v1+tar \
  snapshot-deps.tar:application/vnd.brewlet.classpath.layer.v1+tar
```

**The resulting artifact** carries a thin `app.jar` as the
`application/vnd.brewlet.jar.layer.v1+jar` layer plus one or more
`application/vnd.brewlet.classpath.layer.v1+tar` layers. Each dependency tar is
unpacked to `/app/lib` in the sandbox and the app launches with
`java -cp /app/app.jar:/app/lib/* com.example.Main` (the JVM expands the `lib/*`
wildcard). `brewlet inspect` lists every layer with its media type and digest so you
can see the split:

```bash
brewlet inspect registry.example.com/team/app:1.4.2
# == manifest ==
#   layers:
#     application/vnd.brewlet.jar.layer.v1+jar        sha256:…  (thin app.jar)
#     application/vnd.brewlet.classpath.layer.v1+tar  sha256:…  (deps → /app/lib)
#     application/vnd.brewlet.classpath.layer.v1+tar  sha256:…  (snapshot-deps → /app/lib)
# == jvm config ==  entry.mode=classpath, entry.classPath=["app.jar","lib/*"]
```

Because each layer has its own digest, a code-only rebuild changes **only the small
`app.jar` layer** — the dependency layers are already present in the registry and the
node content store, so only the app layer moves over the wire (and identical
dependency layers dedup across apps). See the
[layered classpath deployment note](layered-classpath-deployment.md) for the design,
cache behavior, and layer-ordering strategy.

> **Already have a framework that emits layered output?** If your framework can
> produce an exploded classes directory plus a directory of dependency JARs (Spring
> Boot's `layertools`, `mvn dependency:copy-dependencies`, Gradle `bootJar`, …), you
> can map that straight onto Brewlet's *generic* classpath layers with a few
> structural steps — **Brewlet never parses `layers.idx` or any framework-specific
> layering manifest**. The Spring PetClinic walkthrough works this through end to
> end: [mapping any framework's layered output](spring-petclinic.md#mapping-any-frameworks-layered-output).

---

## 3. Publish the artifact

### Option A — the `brewlet` CLI (simplest)

```bash
brewlet push ./target/app.jar registry.example.com/team/app:1.4.2
```

- Ships **only the JAR** as an OCI artifact — no Dockerfile, no base image.
- Generates a minimal launch config, or embeds one you pass with `--config jvm-config.json`.
- Attach a prebuilt AppCDS archive with `--appcds-archive ./target/app.jsa` to speed up
  startup (mounted at `/app/app.jsa`, launched with `-Xshare:auto`). See [AppCDS](appcds.md).
- Full flags: [CLI reference](cli-reference.md#brewlet-push).

> **PoC scope.** The reference CLI writes to a local **OCI layout** (`--store`,
> default `./oci`) that stands in for a registry. In production, push to a real
> registry with `oras` (below) or the [Maven plugin](#option-c--maven-plugin);
> a Gradle plugin is on the roadmap.

Inspect what you built:

```bash
brewlet inspect registry.example.com/team/app:1.4.2
# == manifest ==   (OCI manifest with brewlet media types)
# == jvm config == (the launch config above)
```

### Option B — ORAS (a real registry, today)

The artifact is a standard OCI artifact, so `oras` pushes it to any OCI 1.1+
registry:

```bash
cat > jvm-config.json <<'EOF'
{ "schemaVersion": 1, "mainJar": "app.jar",
  "entry": { "mode": "jar" } }
EOF

oras push registry.example.com/team/app:1.4.2 \
  --artifact-type application/vnd.brewlet.app.v1+json \
  --config   jvm-config.json:application/vnd.brewlet.jvm.config.v1+json \
  target/app.jar:application/vnd.brewlet.jar.layer.v1+jar
```

The [media types](reference.md#oci-media-types) are what mark this as a Brewlet JAR
artifact rather than a container image.

### Option C — Maven plugin

The [Brewlet Maven plugin](https://github.com/brewlet/maven-plugin/) wraps steps 2–3 so developers
never touch ORAS or hand-author the launch config. It infers the entry point and
framework from the project and JAR manifest; its `manifest` goal writes the
descriptor's JDK feature request and infers the container `ports`.

```bash
# Build the fat JAR and push it as a Brewlet OCI artifact in one line:
mvn clean package sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT:push \
  -Dbrewlet.image=registry.example.com/team/app:1.4.2
```

Or configure it once in `pom.xml` and bind `push` / `manifest` to the lifecycle:

```xml
<plugin>
  <groupId>sh.brewlet</groupId>
  <artifactId>brewlet-maven-plugin</artifactId>
  <version>0.1.0-SNAPSHOT</version>
  <configuration>
    <image>registry.example.com/team/app:${project.version}</image>
    <jdkFeature>21</jdkFeature>
    <ports><port><name>http</name><containerPort>8080</containerPort></port></ports>
  </configuration>
  <executions>
    <execution><goals><goal>push</goal><goal>manifest</goal></goals></execution>
  </executions>
</plugin>
```

Goals: `brewlet:config` (generate the launch config), `brewlet:build` (assemble a
local OCI layout), `brewlet:push` (publish to a registry), `brewlet:manifest`
(emit a `JavaApplication`/Deployment YAML), `brewlet:inspect` (dry-run preview),
and `brewlet:appcds` (generate an AppCDS startup archive — see [AppCDS](appcds.md)).
See the [plugin README](https://github.com/brewlet/maven-plugin/blob/main/README.md) for the full goal and
parameter reference. A **Gradle plugin** is on the roadmap
([SPECIFICATION §15](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md)).

---

## 4. Pin to a digest (recommended)

Prefer digest-pinned references for deploys:

```
registry.example.com/team/app@sha256:<digest>
```

Digest pinning lets the shim resolve the artifact straight from containerd's content
store (the admission webhook stamps `brewlet.sh/artifact-digest`), and it's the
basis for cosign/SLSA supply-chain policy. See [Security](security.md).

---

## What you did *not* have to do

- No `Dockerfile`.
- No base image to pick, pin, or patch.
- No JVM copied into an image.
- No multi-hundred-MB push — **only the JAR moved over the wire**.

## Next steps

- **[Deploying workloads](deploying-workloads.md)** — run the artifact on a cluster.
- **[Launchers](launchers.md)** — pick `java` vs `jaz`.
- **[Resource tuning](resource-tuning.md)** — set the right JVM flags.
