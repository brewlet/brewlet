# Reference

Quick-lookup tables for the identifiers, formats, and paths Brewlet uses. Sources:
[`operator/internal/brewlet/labels.go`](https://github.com/brewlet/kubernetes/blob/main/operator/internal/brewlet/labels.go),
[`internal/artifact/`](https://github.com/brewlet/brewlet/tree/main/internal/artifact/), and
[SPECIFICATION](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md).

---

## Labels & annotations

### Node labels (set by the provisioner; drive scheduling)

| Key | Value | Meaning |
|---|---|---|
| `brewlet.sh/provision` | `true` | Opt a node in. The platform team sets it; the operator manages the provisioner DaemonSet for matching nodes. |
| `brewlet.sh/runtime` | `ready` | Set once the shim + a JDK are installed and the runtime is registered. The `RuntimeClass` `nodeSelector` matches on it. |
| `brewlet.sh/jdk.<dist>-<feature>` | *(present)* | Boolean-presence label: this exact JDK root is installed (e.g. `brewlet.sh/jdk.temurin-21`). |
| `brewlet.sh/jdk-feature.<feature>` | *(present)* | Some JDK of that feature is installed (e.g. `brewlet.sh/jdk-feature.21`), for distro-agnostic requests. |
| `brewlet.sh/launcher.<name>` | *(present)* | This launcher layer is installed (e.g. `brewlet.sh/launcher.jaz`). |

The admission webhook matches the per-capability labels with `Operator: Exists` when
injecting `nodeAffinity`.

### Node annotations

| Key | Example | Meaning |
|---|---|---|
| `brewlet.sh/jdks` | `temurin-21,microsoft-25` | Advertised JDK roots (comma-separated). |
| `brewlet.sh/launchers` | `java,jaz` | Advertised launcher layers. |
| `brewlet.sh/provision-state` | `Provisioning` \| `Ready` \| `Failed` | The operator's view of the node's lifecycle (distinct from the provisioner-owned `runtime=ready` label). |

### Pod annotations

| Key | Example | Set by | Meaning |
|---|---|---|---|
| `brewlet.sh/jdk` | `21` or `temurin-21` | you | Request a JDK feature (any distro) or an exact `<dist>-<feature>`. Validated + scheduled by the webhook. |
| `brewlet.sh/launcher` | `jaz` | you | Request a launcher. Empty / `java` = vanilla OpenJDK launcher. |
| `brewlet.sh/arch` | `amd64` or `amd64,arm64` | you (or the `JavaApplication` controller from `spec.arch`) | Optional architecture constraint for **non-portable JARs** bundling JNI natives. Injects `kubernetes.io/arch` nodeAffinity; if no ready node of a required arch exists → `NoCompatibleArch`. Omit for arch-neutral bytecode. |
| `brewlet.sh/artifact-container` | `app` | you | Which container's `image` is the OCI artifact (defaults to the brewlet container). |
| `brewlet.sh/artifact-ref` | `repo:tag` | webhook | The OCI artifact ref the shim resolves. |
| `brewlet.sh/artifact-digest` | `sha256:…` | webhook | Stamped when the ref is digest-pinned; lets the shim read the artifact from the content store by digest. |

---

## Event reasons

Recorded by the operator / admission webhook (see [Troubleshooting](troubleshooting.md)):

| Reason | Emitted when |
|---|---|
| `Provisioning` | The operator has requested provisioning for a node. |
| `NodeReady` | A node is provisioned and advertising the brewlet runtime. |
| `ProvisionFailed` | The provisioner pod on a node is failing (e.g. `CrashLoopBackOff`). |
| `NoCompatibleJDK` | A pod requested a JDK no ready node provides → admission denied. |
| `NoCompatibleLauncher` | A pod requested a launcher no ready node provides → admission denied. |
| `NoCompatibleArch` | A non-portable JAR requested an `arch` no ready node provides → admission denied. |

---

## Well-known names

| Name | Value | Notes |
|---|---|---|
| RuntimeClass / containerd handler | `brewlet` | |
| Provisioner DaemonSet | `brewlet-node-provisioner` | Managed by the operator. |
| Default namespace | `brewlet` | Created by the Helm chart. |
| containerd runtime type | `io.containerd.brewlet.v2` | Registered in `/etc/containerd/config.toml`. |
| Vanilla launcher name | `java` | Provided by every JDK; needs no layer. |

---

## OCI media types

The Java application is an **OCI Artifact** (OCI Image Spec ≥ 1.1), not a runnable image.

| Component | Media type | Contents |
|---|---|---|
| Artifact type | `application/vnd.brewlet.app.v1+json` | Manifest `artifactType`. |
| Config blob | `application/vnd.brewlet.jvm.config.v1+json` | The launch config (below). |
| Payload layer | `application/vnd.brewlet.jar.layer.v1+jar` | The raw self-executable JAR. |
| Optional layer | `application/vnd.brewlet.classpath.layer.v1+tar` | Extra JARs (dependency layers) unpacked to `/app/lib`; see [layered classpath deployment](layered-classpath-deployment.md). |
| Optional layer | `application/vnd.brewlet.modulepath.layer.v1+tar` | Library modules for a modular (JPMS) app, unpacked to `/app/mods` and fed to `--module-path`; see [JPMS support](jpms-support.md). |
| Optional layer | `application/vnd.brewlet.cds.layer.v1+jsa` | A single Application Class-Data Sharing archive (`.jsa`), mounted read-only at `/app/<archive>` and consumed with `-Xshare:auto -XX:SharedArchiveFile`; best-effort startup accelerator, see [AppCDS](appcds.md). |

Push with `oras` using exactly these types — see
[Building & publishing](building-and-publishing.md#option-b--oras-a-real-registry-today).

---

## Layered-deployment options (CLI & Maven)

Opt-in flags for [layered (thin JAR) deployment](layered-classpath-deployment.md) —
a thin app JAR plus one or more `classpath.layer.v1+tar` dependency layers unpacked to
`/app/lib`. The fat JAR (`entry.mode: jar`) remains the default.

| Option | Tool | Default | Meaning |
|---|---|---|---|
| `push --classpath-layer TAR` | CLI | *(none)* | Attach a pre-built tar of dependency JARs as a `classpath.layer.v1+tar` layer, unpacked to `/app/lib`. Repeatable, in stable → volatile order. |
| `push --module-layer TAR` | CLI | *(none)* | Attach a pre-built tar of library module JARs as a `modulepath.layer.v1+tar` layer, unpacked to `/app/mods` and fed to `--module-path`. Repeatable; see [JPMS support](jpms-support.md). |
| `<layered>` / `-Dbrewlet.layered` | Maven | `false` | Ship a thin app JAR plus the resolved transitive POM dependency tree packed into reproducible OCI layers. In `classpath` mode this produces `classpath.layer.v1+tar` layers and sets `entry.classPath=[mainJar, "lib/*"]`; in `module` mode the dependency modules are packed into a single `modulepath.layer.v1+tar` layer (unpacked to `/app/mods`) and `entry.modulePath=[mainJar, "mods"]` is set. Forces `entry.mode=classpath` only when the JAR is not modular. |
| `<splitSnapshotLayers>` / `-Dbrewlet.splitSnapshotLayers` | Maven | `true` | When `layered`, pack released deps and `-SNAPSHOT` deps into separate `deps` / `snapshot-deps` layers (stable → volatile) for finer dedup. |

Full flag reference: [CLI reference](cli-reference.md#brewlet-push) and the
[Maven plugin README](https://github.com/brewlet/maven-plugin/blob/main/README.md#configuration-parameters).

---

## Launch config schema (config blob)

```json
{
  "schemaVersion": 1,
  "mainJar": "app.jar",
  "entry": { "mode": "jar" },
  "enablePreview": true,
  "addOpens": ["java.base/java.lang=ALL-UNNAMED"],
  "systemProperties": { "spring.aot.enabled": "true" },
  "cds": { "archive": "app.jsa", "mode": "dynamic" },
  "arch": ["amd64"],
  "user": { "uid": 1000, "gid": 1000 },
  "env": []
}
```

| Field | Type | Notes |
|---|---|---|
| `schemaVersion` | int | `1`. |
| `mainJar` | string | Physical filename of the single primary JAR, mounted read-only at `/app/<mainJar>`. This is **not** the entrypoint (the mode selects that via the manifest `Main-Class`, `mainClass`, or `module`); it only names the file that `classPath`/`modulePath` entries reference by name. Defaults to `app.jar`. |
| `entry.mode` | `jar` \| `classpath` \| `module` | `jar` → `java -jar`; `classpath` → `java -cp <jar> <mainClass>`; `module` → `java [-cp <classPath>] -p <modulePath> -m <module>[/<mainClass>]` (JPMS, optionally with a supplementary class path — the mixed form; see [JPMS support](jpms-support.md) and [layered deployment §8](layered-classpath-deployment.md)). |
| `entry.mainClass` | string | Required iff `entry.mode == classpath`; optional in `module` mode (selects `<module>/<mainClass>`). |
| `entry.classPath` | array | Optional; ordered `/app`-relative class-path entries (e.g. `["app.jar","lib/*"]`) for [layered deployment](layered-classpath-deployment.md). Used in `classpath` mode and, optionally, in `module` mode as a supplementary class path alongside the module path (the mixed form). |
| `entry.module` | string | Required iff `entry.mode == module`; the root module name for `java -m`. |
| `entry.modulePath` | array | Optional; ordered `/app`-relative module-path entries (e.g. `["orders.jar","mods"]`) fed to `java -p`. Only in `module` mode; defaults to `mainJar`. |
| `enablePreview` | boolean | Optional; expands to `--enable-preview` for preview-feature code. |
| `addModules` | array | Optional; expands to `--add-modules <comma-joined>`. |
| `addOpens` | array | Optional; each token expands to `--add-opens <module>/<package>=<target>`. |
| `addExports` | array | Optional; each token expands to `--add-exports <module>/<package>=<target>`. |
| `systemProperties` | object | Optional string map expanded, sorted by key, as `-D<key>=<value>`. |
| `cds` | object | Optional Application Class-Data Sharing hint: `{archive, mode}`. `archive` is a bare filename (e.g. `app.jsa`) shipped as a `cds.layer.v1+jsa` layer, mounted read-only at `/app/<archive>`; launch prepends `-Xshare:auto -XX:SharedArchiveFile=/app/<archive>`. `mode` (`dynamic`\|`static`, informational) records how it was produced. Best-effort accelerator: a build/version/classpath mismatch falls back to base CDS, never fails. See [AppCDS](appcds.md). |
| `arch` | array | Optional architecture constraint (`amd64`, `arm64`). Omit for arch-neutral bytecode (the default — runs on any provisioned arch). Set only for **non-portable JARs** that bundle JNI native libraries or arch-specific deps; steers scheduling to matching-arch nodes via `kubernetes.io/arch` nodeAffinity, and denies admission with `NoCompatibleArch` when no ready node of a required arch exists. The CLI (`brewlet push`) and Maven plugin auto-detect bundled natives and default this accordingly. |
| `user` | object | `{uid, gid}`. |
| `env` | array | `{name, value}`. |

Artifact launch knobs expand first in this order: `-Xshare:auto`
`-XX:SharedArchiveFile` (when `cds` is set), `--enable-preview`, `--add-modules`,
`--add-opens`, `--add-exports`, sorted `-D` flags. Descriptor `jvm.args` follows
for deployment tuning/escape-hatch flags, then the entrypoint.

JDK feature/distribution and launcher are not part of this artifact config. They
are specified in the deployment descriptor (`spec.jvm.version` /
`spec.jvm.distribution` / `spec.jvm.launcher`) or raw pod annotations
(`brewlet.sh/jdk` / `brewlet.sh/launcher`).

**Mode owns its fields (validated).** Each `entry.mode` uses a fixed set of
fields; fields foreign to the selected mode are **rejected**, not silently
ignored. `jar` mode must not set `mainClass` or `classPath` (the manifest
`Main-Class` is authoritative); `classpath` mode requires `mainClass` and
forbids `module`/`modulePath`; `module` mode requires `module` and additionally
permits `classPath` for the **mixed form** (a supplementary `-cp` alongside the
module path — see [layered deployment §8](layered-classpath-deployment.md)).
Unknown
modes and foreign-mode fields are errors, caught by the Maven plugin at build
time (`mvn brewlet:config`/`build`/`push`) and by the launch core at publish and
run time. Unknown JSON *fields* (e.g. a typo like `maimJar`) are additionally
rejected by the launch core — the CLI and shim parse configs with strict field
checking — at publish and run time.

**Top-level JAR references are cross-checked.** Dependency layers unpack under
`/app/lib` (class path) or `/app/mods` (module path); the single primary JAR is
the only file at the `/app` top level. So a bare `<name>.jar` entry (no `/`, no
`*`) in `classPath`/`modulePath` can only resolve to the primary JAR. When
`mainJar` is set, any such entry must equal it, or validation fails with a
dangling-reference error — this catches a `mainJar`/path-entry filename mismatch
before deploy time. Nested entries like `lib/legacy.jar` and wildcards like
`lib/*` are unaffected.

Full field semantics: [Building & publishing](building-and-publishing.md#2-the-launch-config).

---

## Well-known host paths (on a provisioned node)

| Path | Contents |
|---|---|
| `/opt/brewlet/bin/containerd-shim-brewlet-v2` | The shim binary (also linked into `/usr/local/bin`). |
| `/opt/brewlet/jdks/<dist>-<feature>/bin/java` | A shared, read-only JDK runtime root. |
| `/opt/brewlet/launchers/<name>/bin/<name>` | A shared, read-only launcher layer. |
| `/etc/containerd/config.toml` | Patched with the `runtimes.brewlet` block. |
| `/app/<mainJar>` | The JAR, mounted read-only inside the sandbox. |

Override the `/opt/brewlet` prefix with `BREWLET_PREFIX`
([Configuration](configuration.md#node-provisioner-environment-variables)).

---

## containerd runtime registration

The provisioner appends this to `/etc/containerd/config.toml`:

```toml
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.brewlet]
  runtime_type = "io.containerd.brewlet.v2"
```

---

## Glossary

| Term | Definition |
|---|---|
| **OCI Artifact** | Non-image content stored/distributed via an OCI registry using custom media types (OCI Image Spec ≥ 1.1). |
| **containerd Runtime v2 shim** | Pluggable per-runtime process containerd talks to (TTRPC) to manage a container/task — the integration seam KWasm/runwasi use. |
| **RuntimeClass** | Kubernetes object selecting which node runtime/handler executes a pod. |
| **JDK runtime root** | A minimal, read-only Linux userland + JDK installed on the node and overlay-mounted into every JVM sandbox. |
| **Launcher** | The java-compatible program that fronts the entrypoint (`java`, or `jaz`). |
| **Overlay rootfs** | The sandbox filesystem: shared RO JDK lower + per-container upper/work, JAR at `/app`. |
| **AppCDS** | Application Class-Data Sharing — a class archive that cuts startup (ships as a `cds.layer`). |

## See also

- [Concepts & architecture](concepts.md) · [Configuration](configuration.md) ·
  [CLI reference](cli-reference.md) · [SPECIFICATION](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md).
