# Brewlet Maven Plugin

Build and publish a [Brewlet](https://github.com/brewlet/brewlet) OCI app straight from your Maven build —
no Dockerfile, no base image, no container build. A single `mvn brewlet:push`
serializes a launch config, packages your fat JAR, and publishes it to any OCI 1.1+
registry. By default it publishes a **runnable, kubelet-pullable OCI image** (a
`runtimeClassName: brewlet` pod can name it as `image: <ref>`); pass
`-Dbrewlet.format=artifact` for the native Brewlet artifact instead (see
[Delivery format](#delivery-format-native-artifact-vs-runnable-image)).

The plugin wraps steps 2–3 of the [build & publish flow](../specs/SPECIFICATION.md#43-build--publish-flow-developer-experience)
so you never touch `oras` or hand-author a `jvm-config.json`. It infers as much as
possible (main class, framework, ports) from the project and the built JAR's
manifest. JDK feature and launcher requests belong to the generated deployment
descriptor, not the artifact config.

- **Coordinates:** `sh.brewlet:brewlet-maven-plugin:0.1.0`
- **Requires:** Maven 3.9+, JDK 17+ (to run the build). The `appcds`
  training goal requires a JDK 21+ training runtime.

---

## Quick start

Until the plugin is available from Maven Central, download the JAR and POM from
the [v0.1.0 release](https://github.com/brewlet/brewlet/releases/tag/v0.1.0)
and install them into your local Maven repository:

```bash
curl -fLO https://github.com/brewlet/brewlet/releases/download/v0.1.0/brewlet-maven-plugin-0.1.0.jar
curl -fLO https://github.com/brewlet/brewlet/releases/download/v0.1.0/brewlet-maven-plugin-0.1.0.pom
mvn org.apache.maven.plugins:maven-install-plugin:3.1.4:install-file \
  -Dfile=brewlet-maven-plugin-0.1.0.jar \
  -DpomFile=brewlet-maven-plugin-0.1.0.pom
```

Build the fat JAR and push it as a runnable, kubelet-pullable OCI image in one line:

```bash
mvn clean package sh.brewlet:brewlet-maven-plugin:0.1.0:push \
  -Dbrewlet.image=registry.example.com/team/app:1.4.2
```

Or configure it once in `pom.xml` and bind `push` / `manifest` to the lifecycle:

```xml
<plugin>
  <groupId>sh.brewlet</groupId>
  <artifactId>brewlet-maven-plugin</artifactId>
  <version>0.1.0</version>
  <configuration>
    <image>registry.example.com/team/app:${project.version}</image>
    <jdkFeature>21</jdkFeature>       <!-- written to brewlet:manifest descriptors -->
    <launcher>java</launcher>         <!-- omit for vanilla java; use jaz if installed -->
    <ports>                             <!-- descriptor spec.ports (manifest goal) -->
      <port><name>http</name><containerPort>8080</containerPort></port>
    </ports>
    <enablePreview>true</enablePreview>      <!-- artifact correctness flag -->
    <addOpens>
      <addOpen>java.base/java.lang=ALL-UNNAMED</addOpen>
    </addOpens>
    <systemProperties>
      <spring.aot.enabled>true</spring.aot.enabled>
    </systemProperties>
    <jvmArgs>                               <!-- deployment tuning; spec.jvm.args -->
      <jvmArg>-XX:MaxRAMPercentage=75.0</jvmArg>
    </jvmArgs>
  </configuration>
  <executions>
    <execution>
      <goals><goal>config</goal><goal>push</goal><goal>manifest</goal></goals>
    </execution>
  </executions>
</plugin>
```

With that, `mvn deploy` publishes the runnable OCI image and writes the deployment
descriptor as part of the normal lifecycle.

---

## Goals

| Goal | Default phase | What it does |
|---|---|---|
| `brewlet:config` | `package` | Generate `target/brewlet/jvm-config.json` from POM metadata + the JAR manifest. Input for `build`/`push`. |
| `brewlet:build` | — | Assemble the OCI artifact into a local **OCI image-layout** dir (`target/brewlet/oci`) without pushing. Good for inspection, air-gapped flows, or local-registry tests. |
| `brewlet:push` | `deploy` | Build and push to the registry in `<image>`. By default (`image` format) this pushes a **runnable OCI image** — a standard, kubelet-pullable image (see [Delivery format](#delivery-format-native-artifact-vs-runnable-image)). With `-Dbrewlet.format=artifact` it pushes the native Brewlet artifact instead (JAR layer + launch-config blob + manifest with `artifactType: application/vnd.brewlet.app.v1+json`). |
| `brewlet:appcds` | — | Generate a dynamic AppCDS archive (`target/brewlet/app.jsa`) with a self-terminating fat-JAR training run. Attach it later with `-Dbrewlet.cdsArchive=...`. |
| `brewlet:manifest` | — | Emit a `JavaApplication` CR (or raw `Deployment`) YAML compatible with the [Brewlet Kubernetes components](../kubernetes) to `target/brewlet/` for `kubectl apply`, including `spec.jvm.version` / `spec.jvm.launcher`. |
| `brewlet:inspect` | — | Print the fully-resolved launch config and OCI descriptor that *would* be pushed — a dry run to verify inference. |

Run any goal directly, e.g. `mvn brewlet:inspect`.

---

## Configuration parameters

All parameters are optional unless noted; most have a `-Dbrewlet.*` command-line
property. Values configured in `<configuration>` and CLI properties can be mixed.

### Core

| Parameter | Property | Default | Notes |
|---|---|---|---|
| `image` | `brewlet.image` | — | Target OCI ref, e.g. `registry.example.com/team/app:1.4.2`. **Required for `build`, `push`, and `manifest`.** |
| `format` | `brewlet.format` | `image` | Delivery format for `push`: `image` (runnable, kubelet-pullable OCI image — the default) or `artifact` (native Brewlet OCI artifact). See [Delivery format](#delivery-format-native-artifact-vs-runnable-image). |
| `jarFile` | `brewlet.jarFile` | project's primary artifact | Path to the fat JAR to publish. |
| `mainClass` | `brewlet.mainClass` | inferred from `Main-Class` | Main class to launch. Does **not** by itself set the entry mode — the mode is inferred from the JAR's shape (see `entryMode`). Used in `classpath` mode (the class launched via `-cp`) and optionally in `module` mode (selects `<module>/<mainClass>`); ignored in `jar` mode (uses the manifest's `Main-Class`). |
| `entryMode` | `brewlet.entryMode` | inferred from manifest | `jar`, `classpath`, or `module` (auto-detected for modular JARs with a root `module-info.class`). |
| `outputDirectory` | — | `${project.build.directory}/brewlet` | Where generated files land. |
| `skip` | `brewlet.skip` | `false` | Skip all Brewlet goals. |
| `dryRun` | `brewlet.dryRun` | `false` | Generate + display the config but do not push. |
| `layered` | `brewlet.layered` | `false` | **Layered deployment.** Ship a thin app JAR plus the resolved (transitive) POM dependency tree packed into reproducible OCI layers, instead of one opaque JAR. In `classpath` mode this produces `classpath.layer.v1+tar` layers unpacked to `/app/lib` and sets `entry.classPath=[mainJar, "lib/*"]`. In `module` mode the dependency modules are packed into a single `modulepath.layer.v1+tar` layer unpacked to `/app/mods` and `entry.modulePath=[mainJar, "mods"]` is set, so the app launches with `java -p /app/<jar>:/app/mods -m ...`. When the mode is not modular, `layered` forces `entry.mode=classpath`. Unchanged dependency layers dedup by digest across rebuilds/apps. See [layered class-path deployment](https://github.com/brewlet/site/blob/main/docs/layered-classpath-deployment.md). |
| `splitSnapshotLayers` | `brewlet.splitSnapshotLayers` | `true` | When `layered`, pack released deps and `-SNAPSHOT` deps into separate `deps` / `snapshot-deps` layers (stable→volatile) for finer dedup. |
| `cdsArchive` | `brewlet.cdsArchive` | — | Optional prebuilt AppCDS `.jsa` archive to append as a `application/vnd.brewlet.cds.layer.v1+jsa` layer after dependency layers. The archive basename becomes `cds.archive`, is mounted at `/app/<name>`, and launches with `-Xshare:auto -XX:SharedArchiveFile=/app/<name>` as best-effort acceleration. See [AppCDS §4.1](https://github.com/brewlet/site/blob/main/docs/appcds.md#41-build-time-archive-layer-recommended-primary). |

### Descriptor JDK / launcher requests

These parameters feed `brewlet:manifest` and are written to the deployment
descriptor. They are **not** serialized into `target/brewlet/jvm-config.json`.

| Parameter | Property | Default | Notes |
|---|---|---|---|
| `jdkFeature` | `brewlet.jdkFeature` | inferred from project release/target | JDK feature version written as `spec.jvm.version` (or `brewlet.sh/jdk` for raw Deployments). |
| `jdkDistribution` | `brewlet.jdkDistribution` | *(none — any distribution)* | Optional JDK distribution (`temurin`, `microsoft`) written as `spec.jvm.distribution`. With `jdkFeature` it pins an exact `<distribution>-<feature>` node JDK; omit to accept any distribution of that feature. |
| `launcher` | `brewlet.launcher` | `java` | Launcher written as `spec.jvm.launcher` (or `brewlet.sh/launcher`); use `jaz` for the auto-tuning launcher. |

### Runtime shape

| Parameter | Notes |
|---|---|
| `ports` | Descriptor `spec.ports` (manifest goal): `<port>` entries (`name`, `containerPort`, `protocol`). Defaults to `8080/http` with a warning for Spring Boot / Quarkus. Not part of the artifact. |
| `enablePreview` (`brewlet.enablePreview`) | App-intrinsic artifact knob; writes `enablePreview` and expands to `--enable-preview`. |
| `addModules` / `addOpens` / `addExports` | App-intrinsic artifact lists for JPMS/module access; configure with `<addModule>`, `<addOpen>`, and `<addExport>` entries. |
| `systemProperties` | App-intrinsic artifact map expanded as sorted `-D<key>=<value>` flags. |
| `jvmArgs` (`brewlet.jvmArgs`) | Deployment tuning/escape-hatch args written directly to descriptor `spec.jvm.args`; use for heap, GC, agents, and `-XX` flags. |
| `env` | `<envVar>` entries (`name`, `value`). |
| `user` | Optional `uid`/`gid` override for the sandbox process. |

Framework auto-detection is still used for port inference, but framework labels are
not written into the artifact.

---

## AppCDS (`brewlet:appcds`)

AppCDS is Brewlet's optional startup accelerator from
[AppCDS §4.2](https://github.com/brewlet/site/blob/main/docs/appcds.md#42-turnkey-generation-in-the-maven-plugin--cli):
run the app once with `-XX:ArchiveClassesAtExit`, produce `app.jsa`, then ship it
as an extra artifact layer.

```bash
mvn package brewlet:appcds \
  -Dbrewlet.appcds.trainingArgs=--warmup \
  -Dbrewlet.appcds.timeoutSeconds=180

mvn brewlet:push \
  -Dbrewlet.image=registry.example.com/team/app:1.4.2 \
  -Dbrewlet.cdsArchive=target/brewlet/app.jsa
```

You can also bind `appcds` to `pre-integration-test` when your application has a
short startup/warmup mode that exits on its own.

### Long-running servers — `signal` mode

Most Brewlet workloads are long-running servers that never exit on their own.
Dynamic CDS only writes the archive when the JVM exits cleanly, so for these use
`-Dbrewlet.appcds.mode=signal`: the goal starts the app, waits for a readiness
signal, then sends `SIGTERM` so the shutdown hook runs and the archive flushes.

```bash
# Spring Boot: wait for the "Started … in … seconds" line, then SIGTERM
mvn package brewlet:appcds \
  -Dbrewlet.appcds.mode=signal \
  -Dbrewlet.appcds.readyLog='Started .* in .* seconds' \
  -Dbrewlet.appcds.timeoutSeconds=180

# …or poll a health endpoint until it answers 2xx/3xx
mvn package brewlet:appcds \
  -Dbrewlet.appcds.mode=signal \
  -Dbrewlet.appcds.readyHttp=http://localhost:8080/actuator/health

# …or just warm up for a fixed number of seconds
mvn package brewlet:appcds \
  -Dbrewlet.appcds.mode=signal \
  -Dbrewlet.appcds.readyDelaySeconds=20
```

`signal` mode is Unix-oriented: it relies on a graceful `SIGTERM` shutdown to flush
the archive (Brewlet nodes are Linux). The app **must** shut down cleanly on
`SIGTERM` within `shutdownGraceSeconds`, or the archive won't be written.

Parameters:

| Parameter | Property | Default | Notes |
|---|---|---|---|
| `cdsArchiveOutput` | — | `${project.build.directory}/brewlet/app.jsa` | Where `brewlet:appcds` writes the generated dynamic-CDS archive. |
| `trainingArgs` | `brewlet.appcds.trainingArgs` | — | Extra program args passed after `-jar <mainJar>` during training. Use these to exercise startup paths (and, in `exit` mode, to make the app terminate). |
| `timeoutSeconds` | `brewlet.appcds.timeoutSeconds` | `120` | Max wait for the app to exit (`exit` mode) or become ready (`signal` mode). |
| `trainingJavaHome` | `brewlet.appcds.javaHome` | Maven's `java.home` | JDK used for training. Must be JDK 21+ per [AppCDS §2.2](https://github.com/brewlet/site/blob/main/docs/appcds.md#22-minimum-jdk-version). |
| `mode` | `brewlet.appcds.mode` | `exit` | `exit` (self-terminating app) or `signal` (start → wait for readiness → `SIGTERM`). |
| `readyLog` | `brewlet.appcds.readyLog` | — | `signal` mode: regex matched line-by-line against the app's stdout/stderr; readiness fires on first match. |
| `readyHttp` | `brewlet.appcds.readyHttp` | — | `signal` mode: HTTP(S) URL polled until it returns 2xx/3xx. |
| `readyDelaySeconds` | `brewlet.appcds.readyDelaySeconds` | `0` | `signal` mode: fixed warmup delay before `SIGTERM` (used alone, or as a settle time after `readyLog`/`readyHttp`). |
| `shutdownGraceSeconds` | `brewlet.appcds.shutdownGraceSeconds` | `30` | `signal` mode: time allowed after `SIGTERM` for a graceful shutdown that flushes the archive. |
| `readyPollMillis` | `brewlet.appcds.readyPollMillis` | `500` | `signal` mode: poll interval for `readyHttp`. |

Layered class-path and JPMS module apps are supported: run `brewlet:appcds` with
the **same** `-Dbrewlet.layered=true` (and module shape) you push with, so the goal
stages the resolved runtime dependencies into `lib/` (class-path) or `mods/`
(module-path) and trains with `-cp <mainJar>:lib/*` / `-p <mainJar>:mods -m …`,
matching the shim's `/app/lib` and `/app/mods` layout. If you push a fat JAR, train
a fat JAR — the training layout must match the runtime layout for the archive to
map.

Pairing caveat: HotSpot validates dynamic-CDS archives against each app-classpath
JAR's basename, size, and mtime, and Brewlet nodes pin the app JAR mtime to a
canonical timestamp. `brewlet:appcds` trains against a copy with that same mtime,
but the archive is still tied to the exact JDK build and classpath layout. With
`-Xshare:auto`, a mismatch safely falls back to base CDS (no correctness risk, just
no AppCDS benefit); see [AppCDS §7](https://github.com/brewlet/site/blob/main/docs/appcds.md#7-the-jdk-coupling-problem-design-core).

### `brewlet:manifest` extras

| Parameter | Property | Default |
|---|---|---|
| `namespace` | `brewlet.namespace` | `default` |
| `appName` | `brewlet.appName` | `${project.artifactId}` |
| `replicas` | `brewlet.replicas` | `1` |
| CPU/memory requests & limits | `brewlet.resources.*` | `500m` / `256Mi` req, `2` / `512Mi` limit |

---

---

## Delivery format: native artifact vs runnable image

`brewlet:push` can publish in two formats, selected by `format` / `-Dbrewlet.format`:

| Format | What it publishes | When a pod names it as `image:` |
|---|---|---|
| `image` (default) | A **runnable OCI image**: your JAR (+ dependency/module/CDS payload) packaged as standard `tar+gzip` layers with a real OCI image config and a multi-arch index. The Brewlet launch contract rides in the `brewlet.sh/jvm-config` manifest annotation. | kubelet/containerd pull and unpack it like any image, so a `runtimeClassName: brewlet` pod can set `image: <ref>` directly — the SpinKube-style workflow. |
| `artifact` | The **native Brewlet OCI artifact**: your JAR plus a launch-config blob under Brewlet [media types](https://github.com/brewlet/site/blob/main/docs/reference.md#oci-media-types) (`artifactType: application/vnd.brewlet.app.v1+json`). Compact and the canonical Brewlet shape. | kubelet/containerd **cannot** unpack the custom layer media types, so a bare pod `image:` reference `ImagePullBackOff`s. It is resolved by the shim out-of-band via the `brewlet.sh/artifact-*` annotations the admission webhook stamps. |

```bash
# The default push already produces a runnable, kubelet-pullable image:
mvn clean package sh.brewlet:brewlet-maven-plugin:push \
  -Dbrewlet.image=registry.example.com/team/orders:1.4.2

# Opt into the native artifact instead (registry-native / pre-puller flows):
mvn brewlet:push \
  -Dbrewlet.image=registry.example.com/team/orders:1.4.2 \
  -Dbrewlet.format=artifact
```

Or pin the format in `pom.xml` (only needed to opt out of the `image` default):

```xml
<configuration>
  <image>registry.example.com/team/orders:1.4.2</image>
  <format>artifact</format>
</configuration>
```

The runnable image is **byte-compatible** with `brewlet push` from the Go CLI:
standard layers, `rootfs.diff_ids` computed over the *uncompressed* tars, and a
multi-arch index (a portable JAR publishes `amd64`+`arm64`; a JAR pinned to native
architectures via `arch` publishes just those). `brewlet:inspect` reports the runnable
shape (media types, `jvm-config` annotation, platforms). See
[runnable image documentation](https://github.com/brewlet/site/blob/main/docs/runnable-image.md) for the full design.

---

## Output

- `target/brewlet/jvm-config.json` — the generated launch config
  (`application/vnd.brewlet.jvm.config.v1+json`).
- `target/brewlet/app.jsa` — optional AppCDS archive from `brewlet:appcds`.
- `target/brewlet/oci/` — a local OCI image-layout (from `brewlet:build`); readable
  with `brewlet inspect` or pushable with `oras push`.
- `target/brewlet/javaapplication.yaml` — the CR/Deployment manifest (from
  `brewlet:manifest`).

The published artifact uses the Brewlet [media types](https://github.com/brewlet/site/blob/main/docs/reference.md#oci-media-types),
which mark it as an OCI artifact rather than a container image.

---

## Relationship to the `brewlet` CLI

The plugin mirrors the Go [`brewlet` CLI](https://github.com/brewlet/site/blob/main/docs/cli-reference.md): `brewlet:push`
≈ `brewlet push`, `brewlet:inspect` ≈ `brewlet inspect`, and `brewlet:build`
produces the same OCI layout as `brewlet bundle`/`--store`. Use whichever fits your
pipeline — the resulting artifact is identical.

## Building the plugin

From the monorepo root:

```bash
mvn -f maven-plugin/pom.xml install
```

## License

[MIT](LICENSE).
