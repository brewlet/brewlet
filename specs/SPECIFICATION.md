# Brewlet — Run Java applications on Kubernetes like WASM

**Status:** Draft v0.1 — living specification that tracks the implementation
(Phases 0–3 landed; see §15). Not production-ready.
**Audience:** Platform engineers, Kubernetes operators, JVM platform owners
**Analogous prior art:** [KWasm](https://kwasm.sh/) / [SpinKube](https://www.spinkube.dev/) / [runwasi](https://github.com/containerd/runwasi)

---

## 1. Vision & Problem Statement

Today, shipping a Java service to Kubernetes forces developers to become container
authors: pick a base image, write a `Dockerfile`, manage CVEs in the OS layer,
build and push a full container image, and keep the JVM inside that image patched.
The actual artifact they care about — a single self-executable (fat/uber) JAR — is
buried inside hundreds of megabytes of OS and JVM packaging that they did not write
and do not want to maintain.

WebAssembly on Kubernetes solved the analogous problem. With **KWasm**, the Wasm
*runtime* lives on the node, the developer ships only a Wasm *module* as an OCI
artifact, and a `RuntimeClass` tells the kubelet/containerd to execute that module
directly. There is no base image, no OS layer, no Dockerfile.

> **Brewlet brings the same model to Java.**
> Developers push **their Java application** to their OCI registry — most commonly a
> single `app.jar`, but equally a layered classpath app or a JPMS module. A
> node-resident JVM runtime executes it directly (e.g. `java -jar app.jar`), inside an
> isolated sandbox with CPU/memory limits taken from the deployment descriptor. No
> Dockerfile, no base image, no container build step.

### 1.1 The KWasm parallel (what we are copying)

| Concern                  | KWasm (Wasm)                                  | Brewlet (JVM)                                       |
|--------------------------|-----------------------------------------------|-----------------------------------------------------|
| Developer artifact       | `.wasm` module pushed as OCI artifact         | `app.jar` pushed as OCI artifact                    |
| Runtime location         | Wasm runtime (wasmtime/wasmedge) on the node  | JDK/JVM distribution on the node                    |
| Node enablement          | Privileged provisioner DaemonSet installs shim| Privileged provisioner DaemonSet installs shim+JDK  |
| Execution routing        | `RuntimeClass` → containerd shim              | `RuntimeClass` → containerd shim                    |
| Container build needed?  | No                                            | No                                                  |
| Operator                 | `kwasm-operator` provisions nodes             | `brewlet-operator` provisions nodes + reconciles CRD|

---

## 2. Goals & Non-Goals

### 2.1 Goals
- **G1 — Zero-Dockerfile delivery.** Developer publishes their Java application —
  a single self-executable JAR, a layered classpath app, or a JPMS module — as an
  OCI artifact and nothing else.
- **G2 — Native execution.** The JVM runs the application with a canonical
  invocation (e.g. `java -jar app.jar`); no wrapper semantics change.
- **G3 — Declarative resources.** CPU/memory limits, replicas, ports, and JVM
  options come from a Kubernetes deployment descriptor and are enforced via cgroups.
- **G4 — KWasm-grade ergonomics.** Enabling a node is a single Helm install +
  node annotation; deploying a workload is a single manifest with a `runtimeClassName`.
- **G5 — Shared, patchable JVM.** The JDK is owned by the platform, lives on the
  node, is shared across workloads, and is upgraded independently of app artifacts.
- **G6 — First-class Kubernetes citizen.** Services, Ingress, probes, HPA, logs,
  and metrics all work unchanged.

### 2.2 Non-Goals (for v1)
- Replacing OCI *images* for apps that legitimately need OS packages/native deps.
- Building JARs (CI's job). Brewlet only *runs* the application.
- Multi-language polyglot runtimes (Python, Node). JVM-only for v1.
- Live migration of running JVMs between nodes.

---

## 3. High-Level Architecture

```
 Developer / CI             Control Plane                      Worker Node (provisioned)
 ──────────────             ─────────────                      ─────────────────────────

  mvn package               ┌────────────────────────┐         ┌────────────────────────────┐
  oras push ──┐             │ brewlet-operator       │ watches │ Node                       │
              │             │ - node provisioning    │──────►  │  ┌──────────────────────┐  │
              ▼             │ - RuntimeClass mgmt    │annotates│  │ containerd           │  │
       ┌──────────────┐     │ - JavaApplication CRD  │         │  │ + shim:              │  │
       │ OCI Registry │     └───────────┬────────────┘         │  │ containerd-shim-     │  │
       └──────┬───────┘                 │ generates            │  │ brewlet              │  │
              │                         ▼                      │  └──────────┬───────────┘  │
              │               ┌─────────┬──────────┐ scheduled │             │ runc         │
              │               │ Deployment / Pod   │ ────────► │             ▼              │
              │               │ runtimeClassName:  │           │  ┌──────────┬───────────┐  │
              │               │   brewlet          │           │  │ Sandbox              │  │
              │               └────────────────────┘           │  │ (cgroup + netns)     │  │
              │                                                │  │ java -jar            │  │
              └─────── shim pulls OCI artifact ──────────►     │  │ /app/app.jar         │  │
                                                               │  │ (node JDK RO)        │  │
                                                               │  └──────────────────────┘  │
                                                               └────────────────────────────┘
```

### 3.1 Component inventory

1. **OCI Application Artifact format** (§4) — how a Java application (a JAR, or an
   app split into classpath layers) is packaged/pushed/pulled.
2. **`brewlet-node-provisioner`** (§5) — privileged DaemonSet/Job that installs the
   shim binary and one or more JDK distributions onto nodes, then registers the
   containerd runtime.
3. **`containerd-shim-brewlet-v2`** (§6) — containerd Runtime v2 shim that assembles
   a sandbox from the node JDK + the JAR layer and launches `java -jar`.
4. **`RuntimeClass/brewlet`** (§7) — routes pods to the shim handler.
5. **`brewlet-operator`** (§8) — provisions nodes (KWasm-style) and reconciles the
   higher-level `JavaApplication` CRD into Deployments.
6. **`JavaApplication` CRD** (§9) — the developer-facing deployment descriptor.

---

## 4. The OCI Application Artifact

The Java application is shipped as an **OCI Artifact** (OCI Image Spec ≥ 1.1), *not* as
a runnable container image. There is no OS layer and no JVM inside it — only the
application payload (a fat JAR, or dependency/module layers) plus a small JSON
config describing how to launch it.

### 4.1 Media types

| Component        | Media type                                          | Contents                          |
|------------------|-----------------------------------------------------|-----------------------------------|
| Artifact type    | `application/vnd.brewlet.app.v1+json`               | Manifest `artifactType`           |
| Config blob      | `application/vnd.brewlet.jvm.config.v1+json`        | Launch descriptor (below)         |
| Payload layer    | `application/vnd.brewlet.jar.layer.v1+jar`          | The raw self-executable JAR       |
| (optional) layer | `application/vnd.brewlet.classpath.layer.v1+tar`    | Dependency JARs; unpacked to `/app/lib` for layered class-path deployment ([docs](https://github.com/brewlet/site/blob/main/docs/layered-classpath-deployment.md)) |
| (optional) layer | `application/vnd.brewlet.modulepath.layer.v1+tar`   | Library modules for a modular (JPMS) app; unpacked to `/app/mods` and fed to `--module-path` ([docs](https://github.com/brewlet/site/blob/main/docs/jpms-support.md)) |

### 4.2 Launch config (config blob) schema

```json
{
  "schemaVersion": 1,
  "mainJar": "app.jar",
  "entry": {
    "mode": "jar",                       // "jar" => java -jar; "classpath" => java -cp ... Main; "module" => java -p ... -m module
    // classpath mode adds: "mainClass" (required) and optional "classPath"
    // (e.g. ["app.jar","lib/*"]) for layered classpath deployment.
    // module mode adds: "module" (required) and optional "mainClass" + "modulePath"
    // (e.g. ["orders.jar","mods"]) for a JPMS module path — see the JPMS support docs.
    // jar mode must NOT carry mainClass/classPath (the manifest Main-Class wins).
  },
  "enablePreview": true,
  "addOpens": ["java.base/java.lang=ALL-UNNAMED"],
  "systemProperties": { "spring.aot.enabled": "true" },
  "user": { "uid": 1000, "gid": 1000 },
  "env": []
}
```

- The launch config records the artifact's launch contract only. JDK feature +
  distribution and launcher are specified once in the deployment descriptor
  (`spec.jvm.version` / `spec.jvm.distribution` / `spec.jvm.launcher` on
  `JavaApplication`, or `brewlet.sh/jdk` / `brewlet.sh/launcher` pod annotations
  for raw Deployments). The artifact is deployment-agnostic.
- The artifact field set is exactly `schemaVersion`, `mainJar`, `entry`,
  `enablePreview`, `addModules`, `addOpens`, `addExports`, `systemProperties`,
  `user`, `env`, and the two optional constraints/hints `arch` (an architecture
  constraint for non-portable/JNI JARs, steering `kubernetes.io/arch`
  nodeAffinity — §14) and `cds` (an AppCDS archive hint pairing the artifact with
  its `cds.layer.v1+jsa` layer — §13). Ports are a deployment concern
  (`spec.ports`), not part of the artifact.
- The descriptor's launcher selects the JVM launcher that fronts the entrypoint. It is **generic
  and OpenJDK-neutral**: omitted (or `"java"`) means the stock `java` launcher from
  the selected JDK. Brewlet injects **no JVM tuning flags** in either case — the
  container-aware JDK reads the sandbox cgroup limits and the user supplies any
  tuning via descriptor `jvm.args`. Any other name (e.g. `jaz`, the [Azure
  Command Launcher for Java](https://learn.microsoft.com/java/jaz/overview)) is a
  **node-installed, drop-in java-compatible launcher** that additionally auto-tunes
  the JVM from the cgroup limits on the user's behalf. A launcher is a separate node
  package (not part of any JDK), so it composes over any OpenJDK distribution. If the
  requested launcher is not installed on the node, the pod fails to admit (event
  `NoCompatibleLauncher`).
- Artifact launch knobs (`enablePreview`, `addModules`, `addOpens`, `addExports`,
  `systemProperties`) carry app-intrinsic correctness flags. Deployment tuning
  (heap, GC, agents, container flags) belongs in descriptor `jvm.args`, which is
  applied after the artifact knobs and before the entrypoint.
- Launch expansion order is: `--enable-preview`, `--add-modules`, `--add-opens`,
  `--add-exports`, sorted `-D` system properties, descriptor `jvm.args`, then the
  entrypoint (`-jar`, `-cp … <MainClass>`, or `-p … -m …`).
- **Mode owns its fields.** Each `entry.mode` uses a fixed set of fields and
  fields foreign to the selected mode are rejected rather than silently ignored:
  `jar` mode must not carry `mainClass`/`classPath` (the manifest `Main-Class`
  wins), `classpath` mode requires `mainClass`, and `module` mode requires
  `module` and additionally permits `classPath` (the **mixed form**: a
  supplementary `-cp` alongside the module path — §16 Q3). Unknown modes and foreign-mode
  fields are rejected by the Maven plugin at build time and by the launch core at
  publish and launch time; unknown JSON *fields* are additionally rejected by the
  launch core, which parses configs with strict field checking (publish and launch
  time). Because unknown fields are rejected, old artifacts whose `jvm-config.json`
  still contains `jdk`, `launcher`, `labels`, or legacy free-form JVM args must be
  re-pushed. The Maven plugin
  generates the config from typed models, so it validates mode/field consistency
  rather than parsing arbitrary JSON.

### 4.3 Build & publish flow (developer experience)

```bash
# 1. Build the fat JAR as usual — nothing Brewlet-specific.
mvn -q clean package            # → target/app.jar

# 2. Author (or let the CLI generate) the launch config.
cat > jvm-config.json <<'EOF'
{ "schemaVersion": 1, "mainJar": "app.jar",
  "entry": { "mode": "jar" } }
EOF

# 3. Push as an OCI artifact with ORAS (no docker build!).
oras push registry.example.com/team/app:1.4.2 \
  --artifact-type application/vnd.brewlet.app.v1+json \
  --config   jvm-config.json:application/vnd.brewlet.jvm.config.v1+json \
  target/app.jar:application/vnd.brewlet.jar.layer.v1+jar
```

The `brewlet` CLI (`brewlet push ./target/app.jar registry.example.com/team/app:1.4.2`)
and the [Brewlet Maven plugin](https://github.com/brewlet/maven-plugin) (`mvn brewlet:push`) wrap
steps 2–3 so developers never touch ORAS directly. A Gradle plugin is on the roadmap.

### 4.4 Runnable-image delivery mode (kubelet-pullable, the WASI-style pull path)

The native artifact above is **registry-native but not runnable by containerd**: its
custom layer media types (`…+jar`, `.classpath+tar`, `.modulepath+tar`) are not
`tar`/`tar+gzip`/`tar+zstd`, so containerd's CRI differ cannot unpack them. A pod that
names such an artifact as its `image:` therefore fails to pull (`ImagePullBackOff`),
and the payload must be delivered to nodes **out of band** (a node pre-puller, or the
e2e harness's `ctr images import`). That breaks the KWasm/WASI promise that a
`runtimeClassName: brewlet` pod can simply set `image: <ref>` and let kubelet pull it,
exactly as a `runtimeClassName: wasmtime` pod names a Wasm module.

**Runnable-image mode** closes this gap without changing the native format. `brewlet
push --format=image` (and the Maven plugin's `mvn brewlet:push -Dbrewlet.format=image`)
publishes the *same* JAR as a **standard, kubelet-pullable OCI image**:

- a real `application/vnd.oci.image.config.v1+json` config (with `rootfs.diff_ids`
  over the **uncompressed** layer tars, as the OCI image spec requires);
- **`application/vnd.oci.image.layer.v1.tar+gzip`** layers — the app JAR (plus an
  optional AppCDS `.jsa`) in one layer, and the same classpath/modulepath tars a
  native artifact would ship as additional layers, each tagged with its role via a
  `brewlet.sh/layer` annotation (`app` / `classpath` / `modulepath`);
- the launch config (§4.2) carried verbatim in the manifest annotation
  **`brewlet.sh/jvm-config`** rather than as a config blob;
- published as a **multi-arch OCI image index** (default `amd64` + `arm64` for a
  portable bytecode JAR, so any provisioned node matches; narrowed to `--arch` for a
  JAR carrying native libraries).

containerd/kubelet pull and unpack this image with **no special configuration**. The
shim recognizes it (the presence of `brewlet.sh/jvm-config` ⇒ runnable image),
reads the launch config from the annotation, follows the index to the node's platform
manifest, and assembles the same `java -jar`/`-cp`/`-p -m` sandbox on the node-resident
JDK it would for a native artifact — the layers are gunzipped and fed to the existing
bundle-assembly path unchanged. The operator and admission webhook need no change: the
webhook still stamps `brewlet.sh/artifact-digest` (here the image-index digest) and the
Deployment's `image:` is simply the now-pullable ref.

Runnable-image mode is the **default** delivery format for `brewlet push` and the
Maven plugin, since it fulfils the pure `image: <ref>` promise end to end. Native
artifact mode (`--format=artifact` / `-Dbrewlet.format=artifact`) remains available for
clusters with a node pre-puller that want the registry-native, smallest, no-OS-image
framing (and its self-describing media types). See
[`docs/runnable-image.md`](https://github.com/brewlet/site/blob/main/docs/runnable-image.md)
for the full contract. The kubelet-pull → unpack → shim-run path is covered on a
live node by the end-to-end test suite.

---

## 5. Node Provisioning (`brewlet-node-provisioner`)

Mirrors KWasm's bootstrap: a privileged DaemonSet — placed by a `NodeProfile`
pool (§5.6), or by a `brewlet.sh/provision` node label on the standalone no-operator
path (§5.5) — installs the runtime onto the host and wires it into containerd.

### 5.1 Activation
```bash
helm repo add brewlet https://charts.brewlet.sh
helm install -n brewlet --create-namespace brewlet brewlet/brewlet-operator
```

The chart renders a **default `NodeProfile`** (§5.6) that provisions **every
node** — the KWasm-style "all nodes" activation, with no per-node opt-in step to
manage. To scope provisioning to specific pools instead, define named
`NodeProfile`s or disable the default profile (`defaultProfile.enabled=false`,
§5.6). The legacy per-node opt-in — a `brewlet.sh/provision=true` node **label**
(not an annotation; it drives `nodeAffinity`) consumed by the standalone
the [`deploy/node-provisioner.yaml`](https://github.com/brewlet/kubernetes/blob/main/deploy/node-provisioner.yaml)
DaemonSet — remains for the no-operator path (§5.5).

### 5.2 What the provisioner does on each opted-in node
1. Installs the shim binary `containerd-shim-brewlet-v2` into `$PATH` (e.g. `/opt/brewlet/bin`).
2. Installs one or more **JDK runtime roots** under `/opt/brewlet/jdks/<dist>-<feature>/`
   (a minimal, read-only Linux userland + JDK installation; shared by all workloads). See §5.3.
2b. Optionally installs one or more **launcher layers** under
   `/opt/brewlet/launchers/<name>/` (e.g. `jaz`) — read-only, drop-in
   java-compatible launchers overlaid into the sandbox and prepended to `PATH`.
   These are independent of the JDK roots, so any launcher composes over any JDK.
   See §5.4.
3. Appends a runtime entry to `/etc/containerd/config.toml`:
   ```toml
   [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.brewlet]
     runtime_type = "io.containerd.brewlet.v2"
     # Forward the deployment-descriptor annotations the admission webhook stamps
     # so the shim receives the artifact digest (to resolve the artifact from the
     # content store) and the cds-regenerate toggle.
     pod_annotations = ["brewlet.sh/*"]
     [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.brewlet.options]
       SystemdCgroup = true  # mirror the node's runc cgroup driver
   ```
   Because `brewlet` is not a built-in `runc` handler, CRI hands the shim a
   generic `runtimeoptions.Options` carrying this `options` block verbatim; the
   shim translates it back into `runc` options (preserving `SystemdCgroup`) and,
   for the pod's pause/sandbox container, delegates to the embedded `runc` task
   service unchanged rather than rewriting it into a JVM launch.
4. Reloads containerd (SIGHUP / restart), verifies the shim responds.
5. On success, labels the node `brewlet.sh/runtime=ready` and annotates with the
   available JDKs, e.g. `brewlet.sh/jdks=temurin-17,temurin-21,temurin-25`, and
   any installed launchers, e.g. `brewlet.sh/launchers=java,jaz`. It also emits
   per-capability **scheduling labels** the admission webhook matches nodeAffinity
   against (annotations can't drive nodeAffinity): `brewlet.sh/jdk.<dist>-<feature>`,
   `brewlet.sh/jdk-feature.<feature>`, and `brewlet.sh/launcher.<name>` (§8/§14).

The `brewlet.sh/runtime=ready` label is used by the `RuntimeClass` `nodeSelector`
so workloads only schedule onto provisioned nodes.

### 5.3 Installing JDK runtime roots on nodes

Which JDKs a node offers is **declarative**: the platform team lists them in the
provisioner (Helm value / DaemonSet env), and the DaemonSet materializes them on
every opted-in node. Nothing is baked into application artifacts.

```yaml
# values.yaml (brewlet-operator Helm chart)
jdks:
  - distribution: temurin      # curated source mapping
    feature: 21
  - distribution: microsoft
    feature: 25
  - distribution: zulu         # custom source mapping
    feature: 21
    source:
      image: docker.io/library/azul-zulu:21
      javaHome: /usr/lib/jvm/zulu21
```

On each node the provisioner installs every listed JDK under
`/opt/brewlet/jdks/<distribution>-<feature>/` as a **read-only, shared** root, via
**copy-from-image** — the sole acquisition mechanism. The vendor's official JDK
image is pulled through the host containerd and its complete root filesystem is
mounted and copied onto the host `hostPath`, so no package manager touches the
host and every root arrives by a content-addressable (digest-verified) pull:

```bash
# inside the provisioner, per JDK, for the node's arch:
ctr image pull mcr.microsoft.com/openjdk/jdk:25-ubuntu
ctr images mount mcr.microsoft.com/openjdk/jdk:25-ubuntu /tmp/jdk-root
cp -a /tmp/jdk-root/. /opt/brewlet/jdks/microsoft-25/
ctr images unmount /tmp/jdk-root
```

The result contains the source image's userland and a `.brewlet-java-home` file
recording the JDK or jlink runtime location within it. The shim uses the complete
root as the sandbox lower layer and exposes that Java home at `/opt/jdk` (§6.1).
This permits both full vendor JDKs and centrally curated jlink runtimes with an
administrator-approved module set. The latter is installed once per node pool;
it is not carried in each application artifact. Roots are **versioned and
additive**: patching means dropping in a new root and retiring old ones; running
pods are unaffected until they restart. Install one root per node architecture
(amd64/arm64); the JAR is arch-neutral so the same artifact runs on either.

> **Configuration interface (PoC).** The reference provisioner takes the JDK
> inventory as a `JDKS` env var — a comma-separated list of `<distribution>-<feature>`
> tokens (e.g. `temurin-21,microsoft-25`) — and the launcher inventory as a
> `LAUNCHERS` env var (a comma-separated list of launcher names, e.g. `jaz`;
> `java` is implicit). `temurin` and `microsoft` are the curated distributions,
> each mapped to its official image (`eclipse-temurin:<feature>`,
> `mcr.microsoft.com/openjdk/jdk:<feature>-ubuntu`). Other distributions declare
> `source.image` and `source.javaHome` on their `NodeProfile`; the operator renders
> indexed custom-source env variables consumed by the provisioner. Full operator reference, including
> the distribution → image matrix, is in
> [`provisioner/README.md`](https://github.com/brewlet/brewlet/blob/main/provisioner/README.md).

> **Licensing:** ship only OpenJDK builds whose license you accept. Brewlet is
> distribution-neutral and pins nothing — the platform team chooses the builds.

### 5.4 Installing a custom launcher on nodes (e.g. `jaz`)

A launcher is installed the same declarative way, independently of the JDKs:

```yaml
# values.yaml
launchers:
  - jaz        # Azure Command Launcher for Java
```

`jaz` is **not** part of any JDK — it is a separate Linux package (see the
[install guide](https://learn.microsoft.com/java/jaz/install)). The provisioner
stages it into `/opt/brewlet/launchers/<name>/bin/<name>` so it can be overlaid
into the sandbox and put on `PATH` (§6). As with JDKs, prefer copy-from-image so
the host package manager is untouched (the Microsoft Build of OpenJDK images ship
`jaz` preinstalled):

```bash
# copy-from-image (recommended): jaz is preinstalled in the MS OpenJDK images
ctr image pull mcr.microsoft.com/openjdk/jdk:25-ubuntu
mkdir -p /opt/brewlet/launchers/jaz/bin
ctr run --rm --mount type=bind,src=/opt/brewlet/launchers/jaz,dst=/out,options=rbind:rw \
  mcr.microsoft.com/openjdk/jdk:25-ubuntu cp -a /usr/bin/jaz /out/bin/jaz
```

Or install the package directly (matching the node OS), then stage the binary:

```bash
# Azure Linux
sudo tdnf install -y jaz
# Ubuntu/Debian (after adding the Microsoft repo)
sudo apt-get install -y jaz
# then stage it into the launcher root:
mkdir -p /opt/brewlet/launchers/jaz/bin && cp -a "$(command -v jaz)" /opt/brewlet/launchers/jaz/bin/
```

If a bundled/copied launcher needs shared libraries not present in the JDK root,
include them under the launcher root (e.g. `lib/`); the layer is mounted read-only
alongside the JDK. Because `jaz` locates the JVM via `JAVA_HOME` (which Brewlet
pins to the selected JDK), the **same** launcher layer works with any installed
OpenJDK. The node advertises what it installed via `brewlet.sh/launchers=…`, and
a descriptor requesting a launcher the node lacks fails admission with
`NoCompatibleLauncher` (§14).

> **Security note (inherited from KWasm):** node provisioning is privileged and
> modifies the host. It must be scoped to nodes the platform team controls, and is
> not recommended for hostile multi-tenant nodes without further isolation (§11).

### 5.5 Implementation status

The provisioner is **implemented** as a container image built from the
[`provisioner/`](https://github.com/brewlet/brewlet/tree/main/provisioner)
directory (`Dockerfile` + `entrypoint.sh`) and deployed by
[`deploy/node-provisioner.yaml`](https://github.com/brewlet/kubernetes/blob/main/deploy/node-provisioner.yaml):

- **Image** — a multi-stage build that first compiles `containerd-shim-brewlet-v2`
  from the [core runtime](https://github.com/brewlet/brewlet) for the target architecture
  (so the installed shim always
  matches the node arch), then assembles a small Debian-based runtime carrying the
  entrypoint plus `bash`/`curl`/`kubectl`. Build with `make provisioner-image`
  (single arch) or `make provisioner-image-push` (multi-arch via buildx).
- **Entrypoint** — an idempotent script that performs all five §5.2 steps:
  installs the shim to `/opt/brewlet/bin` and the host `/usr/local/bin` (containerd's
  PATH); materializes each declared JDK root under `/opt/brewlet/jdks/<dist>-<feature>/`
  via **copy-from-image** (`ctr` against the host containerd); stages launcher layers
  (e.g. `jaz`) under `/opt/brewlet/launchers/`; appends the
  `runtimes.brewlet` block to `/etc/containerd/config.toml` and reloads containerd
  (SIGHUP via `hostPID`) — gated by a post-install JDK smoke test and configurable
  per the restart policy in §5.6 (`validate` / `containerdRestart`); then labels the
  node `brewlet.sh/runtime=ready`,
  annotates the installed JDKs/launchers, and emits the per-capability scheduling
  labels the admission webhook uses (`brewlet.sh/jdk.*`, `brewlet.sh/jdk-feature.*`,
  `brewlet.sh/launcher.*`). The manifest ships the `ServiceAccount` + `ClusterRole`/binding
  (`get`/`patch` on nodes) the labelling step needs.

Node provisioning is driven by the **operator** (§8.1) and admission is handled
by the **pod webhook** (§8.3) — both implemented. The whole set is packaged by
the [`charts/brewlet`](https://github.com/brewlet/kubernetes/tree/main/charts/brewlet)
Helm chart (Phase 1).

Operator reference for the provisioner (env-var interface, copy-from-image
mechanics, curated distribution → image matrix, deployment): see
[`provisioner/README.md`](https://github.com/brewlet/brewlet/blob/main/provisioner/README.md).

### 5.6 Node profiles (per-pool preparation)

Provisioning every node identically — whether via the chart's default profile
or the legacy `brewlet.sh/provision` label (§5.1/§5.5) — ignores that real
clusters are heterogeneous: a batch pool wants a different JDK than the web pool,
an air-gapped pool needs a registry mirror, some pools must never have containerd
restarted. The cluster-scoped **`NodeProfile`** CRD (`node.brewlet.sh/v1alpha1`)
binds a **node pool** to a **JDK/launcher inventory** and a rollout policy.
*Selecting a pool is the opt-in* — every node in the pool, present and future, is
provisioned; there is no per-node label to manage.

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata: { name: batch }
spec:
  nodePool:
    names: ["batch"]        # matched on the resolved pool key
    # key: agentpool        # optional; auto-detected when omitted
  jdks:
    - { distribution: microsoft, feature: 25 }
  launchers: ["jaz"]
  registry:                  # optional, air-gapped pulls (see below)
    mirrors: { "mcr.microsoft.com": "mirror.internal/mcr" }
  rollout:
    maxUnavailable: 1
    validate: true           # gate readiness on a post-install JDK smoke test
    containerdRestart: validated   # validated | sighup | none
```

- **Pool key resolution.** `spec.nodePool.key` pins the node label carrying the
  pool name; when empty the operator auto-detects the provider key by probing the
  fleet for the well-known keys (`cloud.google.com/gke-nodepool`, `agentpool`,
  `eks.amazonaws.com/nodegroup`, `karpenter.sh/nodepool`). Bare-metal/kubeadm
  clusters with no such label fall back to "every node".
- **The default profile.** A profile with no `nodePool.names` is the **catch-all
  default**: it owns every node *not* claimed by a named-pool profile, expressed
  as a `NotIn [named pools]` nodeAffinity so the pools stay disjoint. The Helm
  chart renders one from `provisioner.jdks/launchers`. Two profiles may not
  name the same pool — the validating webhook (§8.3) rejects the overlap.
- **One DaemonSet per profile.** The operator's `NodeProfileReconciler` (§8.1)
  reconciles each profile into its own `brewlet-node-provisioner-<profile>`
  DaemonSet whose pod `nodeAffinity` is the profile's pool and whose `JDKS` /
  `LAUNCHERS` / `MIRRORS` / `BREWLET_CONTAINERD_RESTART` env come from the spec.
  Non-curated JDK image and Java-home mappings are rendered as indexed
  `JDK_CUSTOM_SOURCE_*` env variables.
- **Registry mirrors (air-gap).** `spec.registry.mirrors` maps a curated upstream
  host to an internal mirror; the provisioner rewrites every copy-from-image pull
  ref through it, so no node ever reaches out to `docker.io` / `mcr.microsoft.com`.
- **Reversal.** Deleting a NodeProfile does not silently strip nodes. A finalizer
  (`node.brewlet.sh/cleanup`) holds the object while the operator runs a
  short-lived `brewlet-cleanup-<profile>` DaemonSet (`BREWLET_MODE=cleanup`) that
  restores the containerd config backup, removes the shim, and drops the runtime +
  capability labels; only once every assigned node is cleaned is the finalizer
  removed and the object garbage-collected.

Sample manifests:
[`deploy/sample-nodeprofile.yaml`](https://github.com/brewlet/kubernetes/blob/main/deploy/sample-nodeprofile.yaml);
design detail in [`proposals/0001-node-profiles.md`](proposals/0001-node-profiles.md).

---

## 6. The containerd Shim (`containerd-shim-brewlet-v2`)

Implements the **containerd Runtime v2 (TTRPC) shim API**, the same integration
point runwasi/Spin shims use. Brewlet takes the **runc-backed** approach: rather
than re-implement namespaces, cgroups, and CNI, the shim *assembles an OCI runtime
bundle and delegates isolation to runc*. This maximizes correctness and reuse.

### 6.1 Per-container lifecycle (`Create`)

1. **Resolve artifact.** Read the image the kubelet handed us. Separate the
   `jvm.config.v1+json` blob from the `jar.layer.v1+jar` blob (containerd content
   store already cached them).
2. **Select JDK/launcher.** Read `brewlet.sh/jdk` and `brewlet.sh/launcher` from
   the OCI runtime spec annotations that originated on the pod. The JDK annotation
   selects the node-resident JDK root (a bare feature or empty request picks the
   lexically-first installed distribution for that feature — feature 21 when the
   annotation is absent — while `<dist>-<feature>` pins an exact root), and
   the launcher annotation selects `java` or a node-installed launcher such as `jaz`.
   Fail fast with a clear event if the requested runtime is unavailable.
3. **Assemble rootfs (overlayfs):**
   - `lowerdir` = the selected read-only JDK runtime root (shared across all pods).
   - `upperdir`/`workdir` = per-container writable scratch.
   - The JAR layer is mounted read-only at `/app/`.
4. **Generate `config.json` (OCI runtime spec):**
   - `process.args = ["java", <merged JVM args>, "-jar", "/app/app.jar"]`
     (or `-cp ... <mainClass>` in classpath mode).
   - `process.user` from config / pod `securityContext`.
   - `linux.resources` populated from the **pod container resource limits**
     (CPU shares/quota, memory limit) — see §10.
   - Standard pod mounts, env, hostname, and the **CNI-provided network namespace**
     injected by the kubelet/containerd (so the pod gets a normal pod IP).
5. **Delegate to runc** to create/start the container. stdout/stderr flow back
   through containerd exactly like any container → `kubectl logs` just works.

### 6.2 Signals & lifecycle
- `Kill`/SIGTERM is forwarded to the JVM PID 1 → JVM shutdown hooks run; honors
  `terminationGracePeriodSeconds` and `preStop`.
- Exit code of the `java` process is the container exit code (drives restarts).

### 6.3 Why runc-backed (vs. a from-scratch JVM launcher)
- Reuses battle-tested cgroup v2, namespace, seccomp/AppArmor, and CNI plumbing.
- Probes (`exec`, `httpGet`, `tcpSocket`), `kubectl exec`, ephemeral debug
  containers, and metrics-server all behave normally.
- The novel part stays small: artifact disassembly + rootfs assembly + arg building.

### 6.4 Implementation status

The shim is **implemented** in the
[`shim/cmd/containerd-shim-brewlet-v2`](https://github.com/brewlet/brewlet/tree/main/shim/cmd/containerd-shim-brewlet-v2)
package
and builds/runs on Linux:

- It embeds containerd's `runtime/v2/shim` framework and reuses the runc-backed
  Task service for the full lifecycle (`Create`/`Start`/`Kill`/`Delete`/`Exec`/
  `Wait`), exactly as runwasi's `runc-v2` shim does (`main_linux.go`,
  `service_linux.go`).
- The Brewlet-specific work is a `Create()` decorator that performs §6.1
  steps 1–4: it resolves the OCI artifact, selects the node JDK/launcher,
  assembles the **overlay rootfs** (shared RO JDK lower + per-container
  upper/work, JAR at `/app`), and rewrites the OCI spec's args/env/mounts — while
  preserving the CRI-provided namespaces (incl. the CNI netns) and cgroup
  resources. runc then does the real create/start.
- Artifact blobs are read through a pluggable resolver (`resolver.go`): a
  `containerd` backend reads the manifest + config + JAR straight from
  containerd's on-disk content store by digest (production), and a `layout`
  backend reads a Brewlet-local OCI layout (the PoC/e2e harness path).

The artifact reference and manifest digest are stamped **cluster-side, not in the
shim**: the `brewlet-admission` webhook (§8.3, implemented) stamps the
`brewlet.sh/artifact-ref` and `brewlet.sh/artifact-digest` annotations onto pods
using `runtimeClassName: brewlet`, so the shim can resolve the artifact from the
content store. On non-Linux dev hosts only the portable bundle-assembly core builds locally;
[integration-test tier 3](https://github.com/brewlet/integration-tests/blob/main/e2e/tier3-runc.sh)
exercises the real Linux/runc path against selected core and Kubernetes checkouts.

---

## 7. RuntimeClass

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: brewlet
handler: brewlet                 # matches the containerd runtime name (§5.2)
scheduling:
  nodeSelector:
    brewlet.sh/runtime: "ready"  # only land on provisioned nodes
overhead:
  podFixed:
    memory: "64Mi"                # JVM/runtime baseline overhead accounting
    cpu: "50m"
```

A raw Pod/Deployment is enough to use Brewlet — set `runtimeClassName: brewlet`
and reference the OCI artifact as the container `image`. The CRD (§9) is a higher-level
convenience on top of this.

---

## 8. The Operator (`brewlet-operator`)

Two responsibilities, the first identical in spirit to `kwasm-operator`:

### 8.1 Node lifecycle controller
- Watches `Node` objects and reflects each provisioned node's state.
- `NodeProfileReconciler` reconciles each `NodeProfile` (§5.6) into a per-profile
  provisioner DaemonSet + the shared `RuntimeClass`, with finalizer-driven cleanup.
- Surfaces node readiness/health and JDK inventory as conditions/events.

> **Implementation status.** Built as a controller-runtime operator in
> the [`brewlet/kubernetes`](https://github.com/brewlet/kubernetes)
> module (separate from the shim, to isolate client-go /
> controller-runtime from the containerd-pinned deps). Two controllers split the
> old monolith:
>
> - **`NodeProfileReconciler`** owns the provisioning mechanism (§5.6). For each
>   `NodeProfile` it resolves the pool key, builds the per-profile
>   `brewlet-node-provisioner-<profile>` DaemonSet (pool `nodeAffinity`, plus
>   `JDKS`/`LAUNCHERS`/`MIRRORS`/`BREWLET_CONTAINERD_RESTART` env from the spec),
>   ensures the `brewlet` RuntimeClass, and reports `assignedNodes` / `readyNodes`
>   and a `Ready` condition (`EmptyPool` / `NodesNotReady` reasons) on status. A
>   `node.brewlet.sh/cleanup` finalizer holds a deleted profile while a
>   `brewlet-cleanup-<profile>` DaemonSet reverses host state; the object is only
>   GC'd once cleanup completes.
> - **`NodeReconciler`** is now a per-node *state mirror*: it watches provisioned
>   nodes (pool membership + the legacy `brewlet.sh/provision` label, gated on the
>   runtime-ready label) and reflects state via the `brewlet.sh/provision-state`
>   annotation plus `Provisioning` / `NodeReady` / `ProvisionFailed` events (§14),
>   reading the `brewlet.sh/provision-error` annotation the provisioner writes on
>   failure. DaemonSet/RuntimeClass ownership moved to `NodeProfileReconciler`.
>
> RBAC + Deployment ship in
> [`config/operator.yaml`](https://github.com/brewlet/kubernetes/blob/main/config/operator.yaml)
> and the [`charts/brewlet`](https://github.com/brewlet/kubernetes/tree/main/charts/brewlet)
> Helm chart. The `NoCompatibleJDK` / `NoCompatibleLauncher`
> events are owned by the pod admission/scheduling webhook (§8.3, implemented);
> the `JavaApplication` controller (§8.2) is implemented too.

### 8.2 `JavaApplication` controller (developer ergonomics)
- Watches the `JavaApplication` CRD (§9) and reconciles it into a managed
  `Deployment` + `Service` + (optional) `HPA`, with:
  - `runtimeClassName: brewlet`,
  - the container `image` = the OCI artifact ref,
  - `resources` copied from the descriptor (enforced as the sandbox cgroup),
  - user-supplied `jvm.args`/`env` wired through (Brewlet injects no tuning of its own),
  - probes, ports, and env wired through.
- Owns and continuously reconciles the generated objects (GC via owner refs).

This is what the prompt calls *“provisioning a container with CPU and Memory limits
as per deployment descriptor.”* The descriptor is the `JavaApplication`.

> **Implementation status.** Built as `JavaApplicationReconciler` in the operator
> module (`internal/controller/javaapplication_controller.go`, with pure,
> unit-tested builders in `javaapplication_resources.go`). It reconciles the
> Deployment/Service/HPA, owns them via controller references, stamps the
> `brewlet.sh/jdk` / `brewlet.sh/launcher` pod annotations the admission webhook
> (§8.3) consumes, wires `jvm.args` through via `JDK_JAVA_OPTIONS`
> (`JAVA_TOOL_OPTIONS` on JDK 8), and reports
> `readyReplicas` / `selectedJdk` / a `Ready` condition on status. The API types
> live in `api/v1alpha1`; RBAC ships in `config/operator.yaml` and the
> [`charts/brewlet`](https://github.com/brewlet/kubernetes/tree/main/charts/brewlet)
> Helm chart (which also installs the CRD).
>
> **Autoscaling (HPA).** When `spec.autoscaling.enabled` is `true`, the controller
> renders an `autoscaling/v1` `HorizontalPodAutoscaler` targeting the managed
> Deployment (`minReplicas` / `maxReplicas` /
> `targetCPUUtilizationPercentage`), and does not reconcile the Deployment's
> `replicas` so the HPA owns scaling. Disabling autoscaling deletes the managed HPA and
> restores `spec.replicas` (default `1`). The HPA is owned via a controller
> reference and garbage-collected with the `JavaApplication`.

### 8.3 Pod admission/scheduling webhook

A mutating+validating admission webhook (`brewlet-admission`) closes the loop
between a brewlet pod and the ready fleet. For every pod on CREATE with
`runtimeClassName: brewlet` it:

- **Stamps** `brewlet.sh/artifact-ref` (from the brewlet container `image`, or
  the container named by `brewlet.sh/artifact-container`) and, when the ref is
  digest-pinned (`repo@sha256:…`), `brewlet.sh/artifact-digest` — the annotations
  the shim resolves the JAR from containerd's content store by (§6.4).
- **Matches** any explicitly requested JDK/launcher (pod annotations
  `brewlet.sh/jdk` = `<dist>-<feature>` or a bare feature such as `21`, and
  `brewlet.sh/launcher`) against ready nodes, read from their `brewlet.sh/jdks` /
  `brewlet.sh/launchers` annotations. If no ready node is compatible, admission is
  denied with a `NoCompatibleJDK` / `NoCompatibleLauncher` reason (§14).
- **Steers** scheduling by injecting `nodeAffinity` onto the provisioner's
  per-capability labels (`brewlet.sh/jdk.<d-f>`, `brewlet.sh/jdk-feature.<f>`,
  `brewlet.sh/launcher.<n>`), so the scheduler skips incompatible nodes rather
  than failing at runtime.

Non-brewlet pods pass through untouched; a pod with no explicit JDK/launcher
request is admitted with just the artifact annotations stamped, and the shim
defaults the JDK to feature 21 (lexically-first installed distribution) and the
launcher to `java`. `failurePolicy: Ignore`
ensures a webhook outage never blocks workloads.

> **Implementation status.** Built as a second binary in the operator module
> ([`cmd/admission`](https://github.com/brewlet/kubernetes/tree/main/cmd/admission)
> + pure, unit-tested logic in
> `internal/admission`). Deployed — with a self-signed serving cert — by the
> [`charts/brewlet`](https://github.com/brewlet/kubernetes/tree/main/charts/brewlet)
> Helm chart (`admission.enabled=true`).
>
> **NodeProfile validation.** The same binary also serves a *validating* webhook
> at `/validate-nodeprofiles` (`NodeProfileValidator`): on `NodeProfile`
> CREATE/UPDATE it rejects an empty JDK list, custom distributions without a valid
> image/Java-home source, curated distributions that try to override their source,
> an invalid `containerdRestart`, and — after listing existing profiles — two
> profiles naming the same pool (`PoolConflict`). Unlike the pod webhook it is
> `failurePolicy: Fail`: a malformed profile would mis-provision the whole fleet,
> so it is rejected up front (§5.6).

---

## 9. `JavaApplication` CRD (the deployment descriptor)

`apiVersion: apps.brewlet.sh/v1alpha1`

```yaml
apiVersion: apps.brewlet.sh/v1alpha1
kind: JavaApplication
metadata:
  name: orders-api
  namespace: payments
spec:
  artifact:
    image: registry.example.com/team/orders:1.4.2   # OCI artifact (digest pinned recommended)
    pullPolicy: IfNotPresent
    pullSecrets: [regcred]
  replicas: 3
  resources:
    requests: { cpu: "500m", memory: "512Mi" }
    limits:   { cpu: "2",    memory: "1Gi" }
  jvm:
    version: 21                  # JDK feature version; authoritative scheduling/runtime request
    distribution: temurin        # optional; pins <distribution>-<feature>. Omit to accept any distribution of this feature
    launcher: java               # vanilla OpenJDK launcher (default/omittable); tune the JVM yourself via args
    # Brewlet injects no -XX flags of its own; the container-aware JDK reads the
    # cgroup memory/cpu limits directly. Set any tuning explicitly here:
    args: ["-XX:MaxRAMPercentage=75.0", "-XX:+UseZGC", "-XX:+ExitOnOutOfMemoryError"]
  env:
    - name: SPRING_PROFILES_ACTIVE
      value: prod
  ports:
    - name: http
      containerPort: 8080
  service:
    enabled: true
    type: ClusterIP
  probes:
    readiness: { httpGet: { path: /actuator/health/readiness, port: 8080 } }
    liveness:  { httpGet: { path: /actuator/health/liveness,  port: 8080 } }
  autoscaling:
    enabled: true
    minReplicas: 3
    maxReplicas: 10
    targetCPUUtilizationPercentage: 70
status:
  observedGeneration: 4
  readyReplicas: 3
  selectedJdk: "temurin-21"      # resolved node JDK; a bare-feature request (no distribution) records the shim-selected distribution
  conditions:
    - type: Ready
      status: "True"
```

### 9.1 Minimal example (the “hello world”, KWasm-style)

```yaml
apiVersion: apps.brewlet.sh/v1alpha1
kind: JavaApplication
metadata: { name: hello }
spec:
  artifact: { image: registry.example.com/demo/hello:1.0.0 }
  resources:
    limits: { cpu: "1", memory: "512Mi" }
  ports: [{ name: http, containerPort: 8080 }]
```

### 9.2 Raw equivalent (no CRD, just RuntimeClass)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: hello }
spec:
  replicas: 1
  selector: { matchLabels: { app: hello } }
  template:
    metadata:
      labels: { app: hello }
      annotations:
        brewlet.sh/jdk: "21"
        brewlet.sh/launcher: java
    spec:
      runtimeClassName: brewlet
      containers:
        - name: hello
          image: registry.example.com/demo/hello:1.0.0   # the OCI artifact
          resources: { limits: { cpu: "1", memory: "512Mi" } }
          ports: [{ containerPort: 8080 }]
```

### 9.3 Choosing a launcher: vanilla `java` vs. `jaz`

The two launchers represent two philosophies of JVM tuning. Brewlet itself injects
**no** `-XX` flags in either case — the difference is who does the tuning.

**Vanilla `java` (default) — you tune it.** The container-aware JDK reads the
cgroup limits; you set heap/GC/etc. explicitly:

```yaml
  jvm:
    version: 21
    launcher: java                 # default; may be omitted
    args:                          # tuning is YOUR responsibility
      - "-XX:MaxRAMPercentage=75.0"
      - "-XX:+UseZGC"
      - "-XX:+ExitOnOutOfMemoryError"
```

**`jaz` — it tunes for you.** The [Azure Command Launcher for
Java](https://learn.microsoft.com/java/jaz/overview) inspects the container's
resources and picks sensible JVM ergonomics automatically, so you typically pass
**no** manual tuning flags. Do not restate what `jaz` derives (e.g.
`MaxRAMPercentage`); reserve `args` for genuinely app-specific flags only:

```yaml
  jvm:
    version: 21
    launcher: jaz                  # auto-tunes heap/GC/CPU from the cgroup limits
    # no MaxRAMPercentage / GC selection needed — jaz derives them
    # args: ["-Dfoo=bar"]          # only truly app-specific flags, if any
```

> The node must have the requested launcher installed (§5.4); otherwise the pod
> fails admission with `NoCompatibleLauncher`.

---

## 10. Resource Limits ↔ JVM Mapping

The deployment descriptor's CPU/memory drive the **sandbox cgroup only**. Brewlet
injects **no JVM tuning flags**: modern JDKs are container-aware (`cgroup v2`) and
read the sandbox limits directly. Artifact launch knobs carry app-intrinsic
correctness flags only; JVM tuning is the **user's** responsibility, set via the
descriptor's `jvm.args`.

| Descriptor field          | Cgroup effect (via runc)        | JVM effect                                             |
|---------------------------|---------------------------------|-------------------------------------------------------|
| `resources.limits.memory` | `memory.max`                    | `-XX:+UseContainerSupport` (default on) reads the cgroup and sizes the heap |
| `resources.limits.cpu`    | `cpu.max` (quota/period)        | cgroup-aware JDK auto-detects available processors from the quota; GC/JIT thread counts scale |

**Recommended user-set tuning (not injected by Brewlet):**
- Container memory must leave headroom for **non-heap** (Metaspace, thread stacks,
  code cache, direct/`mmap` buffers, GC structures). Users typically set
  `-XX:MaxRAMPercentage` (e.g. `75.0`, reserving ~25%) via `jvm.args`.
- `-XX:+ExitOnOutOfMemoryError` recommended so OOM → clean restart by the kubelet.
- Modern JDKs are cgroup-v2 aware; Brewlet **requires cgroup v2** on nodes.
- A custom launcher (e.g. `jaz`) can auto-tune these from the cgroup limits on the
  user's behalf.

---

## 11. Security Model

- **Isolation parity with containers.** Because execution is runc-backed, workloads
  get the same namespace/cgroup/seccomp/AppArmor isolation as ordinary pods. The JAR
  is treated as untrusted code.
- **Non-root by default.** JVM runs as an unprivileged uid; root squashed unless
  explicitly requested via `securityContext`.
- **Supply-chain verification.** The shim/operator will be able to require a valid
  **cosign** signature and/or SLSA provenance on the OCI artifact before launch
  (admission policy) — *planned, not yet implemented.* Digest-pinned refs
  recommended today. See the
  [supply-chain verification research note](https://github.com/brewlet/site/blob/main/docs/supply-chain-admission.md).
- **Privileged provisioning is the sharp edge.** As with KWasm, the node provisioner
  is privileged and mutates the host. Scope it to platform-owned node pools; document
  the blast radius; consider gVisor/Kata as the underlying OCI runtime for stronger
  isolation on shared nodes. See the
  [sandbox runtimes research note](https://github.com/brewlet/site/blob/main/docs/sandbox-runtimes.md).
- **JDK CVE management is centralized.** Patching the node JDK patches *all*
  workloads at once — a major advantage over per-image JVMs.

---

## 12. Networking, Observability, Day-2

- **Networking:** normal pod IP via CNI (runc owns the netns). Services/Ingress/
  NetworkPolicy unchanged.
- **Logs:** JVM stdout/stderr → containerd → `kubectl logs`.
- **Metrics/Tracing:** JMX/Micrometer/OTel work as usual; JFR can be enabled via
  `jvm.args`. Node-level JVM metrics (per-sandbox RSS, GC) would be exported by the
  shim — *planned, not yet built* ([research](https://github.com/brewlet/site/blob/main/docs/metrics-exporter.md)).
- **Probes & exec:** `kubectl exec`, ephemeral debug containers, and all probe types
  work because runc backs the sandbox.
- **Upgrades:** JDK roots are versioned and additive on nodes; old versions retained
  until no workload references them, then GC'd by the provisioner.
- **Multi-arch:** JDK roots installed per node architecture (amd64/arm64); the JAR
  artifact is arch-independent, so the *same* artifact runs on any provisioned arch
  (see [multi-arch](https://github.com/brewlet/site/blob/main/docs/multi-arch.md)).

---

## 13. Performance & Startup

Cold-start is the JVM's classic weakness vs. Wasm. Brewlet leans on node-resident
caching and JVM features:

- **Shared, pre-warmed JDK** on the node → no per-pod JDK pull/unpack.
- **Artifact caching:** containerd content store caches the JAR layer; only the
  (small) JAR moves over the network, not a full image.
- **AppCDS / dynamic CDS:** optionally ship a class-data archive as a
  `cds.layer.v1+jsa` layer (`brewlet push --appcds-archive`) to cut startup; it is
  mounted at `/app/<archive>` and consumed with `-Xshare:auto` so a JDK-build
  mismatch falls back safely to base CDS. Alternatively opt into **node-side
  regeneration** at the deployment level (`spec.jvm.cds.regenerate` on the
  `JavaApplication` CRD → `brewlet.sh/cds-regenerate` pod annotation; `brewlet
  run/bundle --appcds-regenerate` locally): the node maintains a
  per-`(artifact, JDK-build)` archive cache driven by `-XX:+AutoCreateSharedArchive`
  (JDK 19+) that self-heals on every central JDK patch. See the
  [AppCDS note](https://github.com/brewlet/site/blob/main/docs/appcds.md).
- **Project Leyden (AOT):** track for future static-image-like startup.

---

## 14. Failure Modes & Edge Cases

| Scenario                                   | Behavior                                                            |
|--------------------------------------------|--------------------------------------------------------------------|
| No compatible JDK on any ready node        | Pod stays `Pending`; event `NoCompatibleJDK`; scheduler skips node  |
| Requested launcher not installed on node   | Pod stays `Pending`; event `NoCompatibleLauncher`; scheduler skips node |
| Non-portable JAR needs an arch with no ready node | Pod stays `Pending`; event `NoCompatibleArch`; scheduler skips node |
| OCI artifact missing/unauthorized          | `ImagePull`-style failure surfaced on the pod                       |
| Signature/provenance verification fails    | Admission denied (if policy enabled); clear reason on the object    |
| JVM OOM                                     | `ExitOnOutOfMemoryError` → exit → kubelet restart per `restartPolicy`|
| Node provisioning fails                     | Node not labeled `ready`; operator event `ProvisionFailed`          |
| NodeProfile names a pool with no matching nodes | Profile `Ready=False` reason `EmptyPool`; DaemonSet lands nowhere |
| Two NodeProfiles name the same pool         | Rejected at admission with reason `PoolConflict` (§5.6/§8.3)         |
| NodeProfile deleted                         | Held by `node.brewlet.sh/cleanup` finalizer until the cleanup DaemonSet reverses host state |
| Shim crash                                  | containerd reports task failure; pod restarts                       |
| cgroup v1-only node                         | Provisioner refuses; node not marked ready (cgroup v2 required)     |

> The `NoCompatibleJDK` / `NoCompatibleLauncher` / `NoCompatibleArch` rows are
> enforced by the pod admission webhook (§8.3): an incompatible explicit request is
> denied at admission with that reason, and compatible pods get nodeAffinity so the
> scheduler skips nodes lacking the JDK/launcher or of the wrong architecture. The
> `arch` constraint (mapped to the kubelet-provided `kubernetes.io/arch` label) is
> optional and only needed for non-portable JARs that bundle JNI native libraries;
> arch-neutral bytecode artifacts leave it unset and run on any provisioned arch.

---

## 15. Phased Roadmap

**Phase 0 — Proof of concept**
- OCI artifact format + ORAS push.
- `containerd-shim-brewlet-v2` implementing the containerd Runtime v2 (TTRPC)
  Task service (runc-backed): embeds `runtime/v2/shim`, assembles an overlay-rootfs
  `java -jar` sandbox from a node-resident JDK, and resolves the OCI artifact from
  containerd's content store (see §6.4).
- `brewlet-node-provisioner` image + entrypoint that installs the shim + JDK
  roots + launcher layers and registers the containerd runtime on opted-in nodes
  (see §5.5).
- `brewlet-operator` node lifecycle controller (§8.1) that watches provision-opted
  nodes, manages the provisioner DaemonSet, and reconciles the `RuntimeClass`
  (replacing the by-hand wiring).

**Phase 1 — KWasm parity (MVP) ✅ implemented**
- Helm chart to package the operator + provisioner + RuntimeClass with `jdks:` /
  `launchers:` values (the KWasm-style `helm install` activation) —
  [`charts/brewlet`](https://github.com/brewlet/kubernetes/tree/main/charts/brewlet).
- Pod admission/scheduling webhook: matches a pod's requested JDK/launcher against
  ready nodes (`NoCompatibleJDK` / `NoCompatibleLauncher`) and stamps the artifact
  ref/digest annotations the shim resolves —
  [`cmd/admission`](https://github.com/brewlet/kubernetes/tree/main/cmd/admission)
  (§8.3).
- `kubectl logs/exec`, probes, Services — work unchanged because the shim runs a
  real runc task with the CRI-provided netns/cgroups (see
  [`deploy/raw-deployment.yaml`](https://github.com/brewlet/kubernetes/blob/main/deploy/raw-deployment.yaml)).

**Phase 2 — Developer ergonomics**
- `JavaApplication` CRD + controller ✅ implemented (`JavaApplicationReconciler`,
  §8.2) — reconciles descriptors into Deployment + Service + HPA.
- `brewlet` CLI ✅ implemented
  ([`cmd/brewlet`](https://github.com/brewlet/brewlet/tree/main/cmd/brewlet) —
  `push` / `inspect` / `run` /
  `bundle` / `jdks`; see [CLI reference](https://github.com/brewlet/site/blob/main/docs/cli-reference.md)).
- Brewlet **Maven plugin** ✅ implemented
  ([`brewlet/maven-plugin`](https://github.com/brewlet/maven-plugin) — goals
  `config` / `build` / `push` / `manifest` / `inspect` (plus `appcds`, §13);
  wraps §4.3 steps 2–3 so
  developers never touch ORAS). Supports both delivery formats via
  `-Dbrewlet.format` (runnable `image`, the default, and native `artifact`, §4.4).
- User-supplied JVM tuning via `jvm.args` + custom launcher support (e.g. jaz).

**Phase 3 — Hardening & speed ✅ implemented**
- AppCDS ✅ implemented — build-time class-data archives (the
  `application/vnd.brewlet.cds.layer.v1+jsa` layer + the `brewlet:appcds` Maven goal /
  `brewlet push --appcds-archive`, with `-Xshare:auto -XX:SharedArchiveFile` launch
  wiring and deterministic JAR-mtime normalization) **and** node-side regeneration
  (`spec.jvm.cds.regenerate` / `--appcds-regenerate`, backed by
  `-XX:+AutoCreateSharedArchive` and a per-`(artifact, jdk-build)` node cache that
  self-heals on every central JDK patch) both ship ([docs/appcds.md](https://github.com/brewlet/site/blob/main/docs/appcds.md), §13).
- Multi-arch ✅ implemented (Phase A) — per-node-arch JDK provisioning (the
  provisioner's copy-from-image selects the matching platform), multi-arch component
  images (buildx `*-image-push` targets), and an optional `arch` launch-config/CRD
  constraint that steers non-portable (JNI) JARs via `kubernetes.io/arch` nodeAffinity
  with a `NoCompatibleArch` denial ([docs/multi-arch.md](https://github.com/brewlet/site/blob/main/docs/multi-arch.md)).
- HPA ✅ — landed in Phase 2; the `JavaApplication` controller reconciles a
  `HorizontalPodAutoscaler` (§8.2).

**Phase 4 — Hardening & speed (remaining)**
- cosign/SLSA supply-chain admission ([research](https://github.com/brewlet/site/blob/main/docs/supply-chain-admission.md));
  gVisor/Kata stronger-isolation option ([research](https://github.com/brewlet/site/blob/main/docs/sandbox-runtimes.md)).
- Metrics exporter ([research](https://github.com/brewlet/site/blob/main/docs/metrics-exporter.md)); multi-arch Phase B —
  coverage observability + accelerator guardrails ([research](https://github.com/brewlet/site/blob/main/docs/multi-arch.md)).

**Phase 5 — Additional developer ergonomics**
- Brewlet **Gradle plugin** (`config` / `build` / `push` / `manifest` / `inspect`
  parity with the Maven plugin; wraps §4.3 steps 2–3 so developers never touch ORAS).

---

## 16. Open Questions

1. **JDK distribution & licensing on nodes** — which builds ship by default
   (Temurin only?), and how are LTS upgrades rolled out across node pools?
2. **Shim build base** — *resolved (PoC):* the shim embeds containerd's own
   `runtime/v2/shim` framework and reuses its runc-backed Task service (the Go
   equivalent of the runwasi/`runc-v2` approach), rather than a from-scratch Go
   shim over `runc` libcontainer. See §6.4.
3. **Classpath/modular apps** — *implemented in the PoC.* Non-modular
   apps can be split into stable dependency layers + a thin app layer for registry
   dedup via `entry.classPath` over the `classpath.layer.v1+tar` layer (unpacked to
   `/app/lib`) — see [docs/layered-classpath-deployment.md](https://github.com/brewlet/site/blob/main/docs/layered-classpath-deployment.md).
   Modular (JPMS) apps launch via `entry.mode: module` (emit `java -p … -m …`)
   with an optional `modulepath.layer.v1+tar` module layer (unpacked to
   `/app/mods`) — see [docs/jpms-support.md](https://github.com/brewlet/site/blob/main/docs/jpms-support.md). The **mixed
   form** is also supported: `entry.mode: module` additionally permits
   `entry.classPath`, emitting `java -cp … -p … -m …` so a modular app can carry
   automatic-module or non-modular libraries on the class path
   ([§8.1](https://github.com/brewlet/site/blob/main/docs/layered-classpath-deployment.md)).
   Application artifacts do not carry `jlink` runtimes or `.jmod` files. A
   platform administrator may install a shared jlink runtime, including centrally
   approved modules, as node inventory through a `NodeProfile` custom source; the
   application remains an ordinary JAR artifact.
4. **Artifact format standardization** — pursue a shared/standard media type so the
   ecosystem (registries, scanners, CLIs) recognizes "JAR-as-OCI-artifact"?
5. **Strong multi-tenancy** — is runc isolation sufficient, or is Kata/gVisor the
   default for untrusted JARs?
6. **Cold-start SLOs** — what startup targets justify shipping AppCDS archives (§13)
   over relying on the shared, pre-warmed node JDK alone?

---

## 17. Glossary

- **OCI Artifact** — non-image content stored/distributed via an OCI registry using
  custom media types (OCI Image Spec ≥ 1.1).
- **containerd Runtime v2 shim** — pluggable per-runtime process that containerd
  talks to (TTRPC) to manage a container/task; the integration seam used by KWasm.
- **RuntimeClass** — Kubernetes object selecting which node runtime/handler executes
  a pod.
- **JDK runtime root** — a minimal, read-only Linux userland + JDK installed on the
  node and shared (overlayed) into every JVM sandbox.
