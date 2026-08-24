# CLI reference — `brewlet`

The `brewlet` CLI lets a developer ship
**their Java application** — a fat JAR, plus optional classpath layers — as an OCI
artifact and the node-resident JVM runs it (e.g. `java -jar`). Download a
verified platform archive with the installer:

```bash
curl -fsSL https://brewlet.sh/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
brewlet version
```

The installer selects the latest release by default. Set `BREWLET_VERSION=0.1.0`
to pin a release or `BREWLET_INSTALL_DIR=/custom/bin` to change the destination.
You can instead build the CLI with `make binaries` (producing `./bin/brewlet`).

```
brewlet push    <jar> <ref> [flags]   publish a JAR as an OCI artifact
brewlet inspect <ref>       [flags]   show the artifact manifest + config
brewlet run     <ref>       [flags]   pull + launch java -jar on this node
brewlet bundle  <ref>       [flags]   emit an OCI runc bundle (the shim path)
brewlet jdks                [flags]   list JDKs available across the cluster
brewlet doctor              [flags]   diagnose cluster and developer readiness
brewlet version                       print the CLI version
```

`<ref>` is a `name:tag`, e.g. `demo/hello:1.0.0`.

> The native-artifact path reads and writes a local **OCI layout** directory
> (`--store`, default `./oci`). The default runnable-image format publishes to a
> registry. See [Building & publishing](building-and-publishing.md).

Flags may appear before *and* after positional arguments. For `run`, everything
after a literal `--` is passed as **extra JVM args**.

---

## `brewlet push`

Publish a JAR to an OCI registry (generates a minimal launch config, or embeds one
you provide). By default it publishes a **runnable, kubelet-pullable OCI image**; pass
`--format=artifact` for the native Brewlet artifact instead (see
[runnable-image delivery](runnable-image.md)).

```
brewlet push <jar> <ref> [--format image|artifact] [--store DIR] [--config FILE]
                         [--arch LIST | --no-arch]
                         [--classpath-layer TAR ...] [--module-layer TAR ...]
                         [--appcds-archive JSA | --appcds [--appcds-java JAVA]
                          [--appcds-timeout SEC] [--appcds-arg ARG ...]]
```

| Flag | Default | Meaning |
|---|---|---|
| `--format` | `image` | Delivery format: `image` (standard, kubelet-pullable OCI image — a `runtimeClassName: brewlet` pod can name it as `image: <ref>`) or `artifact` (native Brewlet OCI artifact with custom media types, delivered to nodes out of band). See [runnable-image delivery](runnable-image.md). |
| `--store` | `./oci` | OCI layout directory to write the artifact into. |
| `--config` | *(none)* | Path to a `jvm-config.json` to embed verbatim (overrides the generated one). See the [launch config schema](building-and-publishing.md#2-the-launch-config). |
| `--arch` | *(auto-detected)* | Comma-separated architecture constraint (e.g. `amd64` or `amd64,arm64`) for a **non-portable (JNI) JAR**: injects `kubernetes.io/arch` nodeAffinity and denies scheduling with `NoCompatibleArch` when no ready node matches. Overrides native-library auto-detection. Omit for arch-neutral bytecode (the default). See [multi-arch](multi-arch.md). |
| `--no-arch` | `false` | Disable native-library auto-detection and publish with **no** arch constraint (force arch-neutral), even when bundled natives are found. |
| `--classpath-layer` | *(none)* | Tar of dependency JARs to attach as a class-path layer (repeatable), unpacked to `/app/lib`. See [layered classpath deployment](layered-classpath-deployment.md). |
| `--module-layer` | *(none)* | Tar of library modules to attach as a module-path layer (repeatable), unpacked to `/app/mods` and fed to `--module-path`. See [JPMS support](jpms-support.md). |
| `--appcds-archive` | *(none)* | Prebuilt Application Class-Data Sharing archive (`.jsa`) to ship, mounted at `/app/<name>` and launched with `-Xshare:auto -XX:SharedArchiveFile`. Sets `cds.archive` in the config from the file's basename (unless `--config` already declares one, which must then match). Best-effort startup accelerator; see [AppCDS](appcds.md). |
| `--appcds` | `false` | Generate the AppCDS archive **turnkey** — run a self-terminating training JVM against the JAR, then ship the result (the generate-it-for-me equivalent of `--appcds-archive`). Fat-JAR only; mutually exclusive with `--appcds-archive`, `--classpath-layer`, and `--module-layer`. See [AppCDS §4.2](appcds.md). |
| `--appcds-java` | *(auto)* | `java` executable (or a `JAVA_HOME` directory) used for `--appcds` training. Defaults to `$JAVA_HOME/bin/java`, then `java` on `PATH`. |
| `--appcds-timeout` | `120` | Seconds to wait for the `--appcds` training JVM to self-terminate. |
| `--appcds-arg` | *(none)* | Workload argument passed to the `--appcds` training JVM to drive class loading (repeatable). |

By default `push` scans the JAR for bundled native libraries and sets the `arch`
constraint automatically for non-portable artifacts (pass `--arch` to override, or
`--no-arch` to opt out). AppCDS is opt-in: ship a prebuilt archive with
`--appcds-archive`, or let the CLI build one with `--appcds` (the two are mutually
exclusive).

```bash
brewlet push ./target/app.jar demo/hello:1.0.0                        # runnable image (default)
brewlet push ./target/app.jar demo/hello:1.0.0 --format artifact      # native artifact
brewlet push ./target/app.jar demo/hello:1.0.0 --config ./jvm-config.json
brewlet push ./target/app.jar demo/hello:1.0.0 --config ./cfg.json --classpath-layer deps.tar
brewlet push ./target/orders.jar demo/orders:1.0.0 --module-layer mods.tar
brewlet push ./target/app.jar demo/hello:1.0.0 --appcds-archive ./target/app.jsa
brewlet push ./target/app.jar demo/hello:1.0.0 --appcds                # generate + ship an AppCDS archive
brewlet push ./target/native-app.jar demo/native:1.0.0 --arch amd64,arm64   # non-portable (JNI) JAR
```

Output confirms the pushed digest and store (for a runnable image, the multi-arch index
digest and target platforms; for a native artifact, the manifest digest and
`artifactType`) — and reminds you that you shipped **only the JAR**, no Dockerfile.

---

## `brewlet inspect`

Print the artifact's OCI manifest and its JVM launch config.

```
brewlet inspect <ref> [--store DIR]
```

| Flag | Default | Meaning |
|---|---|---|
| `--store` | `./oci` | OCI layout directory to read from. |

```bash
brewlet inspect demo/hello:1.0.0
# == manifest ==   (OCI manifest with brewlet media types)
# == jvm config == (mainJar, entry, enablePreview, addOpens, systemProperties, …)
```

---

## `brewlet run`

Resolve the artifact, assemble a local sandbox, and launch `java -jar` in the
foreground using a node-resident JDK. This is the Layer‑1 demo path (no cgroups).

```
brewlet run <ref> [--store DIR] [--jdk-root DIR] [--launcher NAME] [--appcds-regenerate] [-- <extra jvm args>]
```

| Flag | Default | Meaning |
|---|---|---|
| `--store` | `./oci` | OCI layout directory to read from. |
| `--jdk-root` | *(none)* | Node JDK home to launch with. When unset, falls back to `BREWLET_JDK_HOME`, then `JAVA_HOME`, then `java` on `PATH`. |
| `--launcher` | `java` | Launcher binary name under the selected JDK (or a compatible node-installed launcher name such as `jaz`). |
| `--appcds-regenerate` | `false` | Opt into **node-side AppCDS regeneration** ([AppCDS §4.3](appcds.md); the local-dev equivalent of the deployment's `spec.jvm.cds.regenerate`). Maintains a per-`(artifact, JDK-build)` archive cache under `$BREWLET_CDS_CACHE` (default `/opt/brewlet/cds`) driven by `-XX:+AutoCreateSharedArchive` (JDK 19+), self-healing on every central JDK patch. Any shipped archive becomes optional *seed* data — works with no archive at all. |
| `-- <args>` | *(none)* | Everything after `--` is appended as extra JVM args. |

```bash
brewlet run demo/hello:1.0.0
brewlet run demo/hello:1.0.0 -- -Dspring.profiles.active=dev -XX:+UseZGC
brewlet run demo/hello:1.0.0 --jdk-root /opt/brewlet/jdks/temurin-21
brewlet run demo/hello:1.0.0 --launcher jaz
brewlet run demo/hello:1.0.0 --appcds-regenerate
```

It prints the selected node JDK, the launcher (if a custom one like `jaz` is
configured), the sandbox path, and the exact launch command line before the JVM's
own output.

---

## `brewlet bundle`

Emit the **OCI runtime bundle** (`config.json` + rootfs layout) that the containerd
shim feeds to runc. Useful to see exactly what will run on a node.

```
brewlet bundle <ref> [--store DIR] [--cpu N] [--memory M] [--jdk-root DIR] [--launcher NAME] [--launcher-root DIR] [--appcds-regenerate] [--out DIR]
```

| Flag | Default | Meaning |
|---|---|---|
| `--store` | `./oci` | OCI layout directory to read from. |
| `--cpu` | *(unlimited)* | CPU limit, e.g. `2` or `500m` → sandbox `cpu.max`. |
| `--memory` | *(unlimited)* | Memory limit, e.g. `512Mi` or `1Gi` → sandbox `memory.max`. |
| `--jdk-root` | `/opt/brewlet/jdks/temurin-21` | Node JDK runtime root to mount read-only. |
| `--launcher` | `java` | Launcher binary name to record in the runtime spec annotations and execute. |
| `--launcher-root` | *(none)* | Node launcher-layer root for a custom launcher (e.g. `jaz`). |
| `--appcds-regenerate` | `false` | Opt into **node-side AppCDS regeneration** ([AppCDS §4.3](appcds.md); the local equivalent of the deployment's `spec.jvm.cds.regenerate`). Bind-mounts a per-`(artifact, JDK-build)` archive cache into the sandbox and prepends `-XX:+AutoCreateSharedArchive` (JDK 19+). |
| `--out` | `./bundle` | Output bundle directory. |

```bash
brewlet bundle demo/hello:1.0.0 --cpu 2 --memory 512Mi --out ./bundle
cat ./bundle/config.json
# On a Linux node the shim runs the equivalent of:
#   runc run -b ./bundle brewlet-<id>
```

---

## `brewlet jdks`

List the JDKs available across the cluster — **vendor, major version, minor
version, and architecture** — so you can match your dev and CI toolchains to
production. It reads the `brewlet.sh/jdks-info` inventory annotation each Brewlet
node advertises, via `kubectl get nodes` (no in-process Kubernetes client, so it
uses your existing kubeconfig/context).

```
brewlet jdks [--output table|wide|json] [--kubeconfig FILE] [--context CTX] [--selector SEL]
```

| Flag | Default | Meaning |
|---|---|---|
| `--output` | `table` | `table` = distinct JDKs across the fleet with a node count; `wide` = one row per node; `json` = machine-readable aggregation (e.g. to drive a CI matrix). |
| `--kubeconfig` | *(kubectl default)* | Path to a kubeconfig file. |
| `--context` | *(current)* | kubeconfig context to use. |
| `--selector` | *(none)* | Label selector to filter nodes (passed to `kubectl -l`). |

```bash
brewlet jdks
# VENDOR             DISTRIBUTION   MAJOR   VERSION   ARCH    NODES
# Microsoft          microsoft      25      25        amd64   3
# Eclipse Adoptium   temurin        21      21.0.5    amd64   3
# Eclipse Adoptium   temurin        21      21.0.5    arm64   2

brewlet jdks --output wide                      # per-node breakdown
brewlet jdks --output json                      # for scripting / CI matrices
brewlet jdks --selector brewlet.sh/runtime=ready
```

Nodes provisioned before the rich annotation existed fall back to the coarse
`brewlet.sh/jdks` list (distribution + major only). See
[JDK management → Inspecting the JDKs available](jdk-management.md#inspecting-the-jdks-available-on-the-cluster)
for the equivalent plain-`kubectl` queries.

---

## `brewlet doctor`

Check whether a cluster is ready to accept Brewlet workloads and whether the
current identity can deploy a `JavaApplication` in a target namespace.

```
brewlet doctor [--namespace NS] [--output table|json]
               [--kubeconfig FILE] [--context CTX]
```

The command checks the selected context, API connectivity, the `brewlet`
RuntimeClass, the `JavaApplication` CRD, schedulable Brewlet-ready containerd
nodes, advertised JDK inventory, and create permission in the selected
namespace. Failed checks include a remediation hint.

```bash
brewlet doctor --namespace my-team
brewlet doctor --context staging --namespace my-team
brewlet doctor --namespace my-team --output json
```

The command exits non-zero when a blocking check fails. JSON output is suitable
for CI and platform handoff automation.

---

## `brewlet version`

Print the release version embedded in the binary:

```bash
brewlet version
# 0.1.0
```

Source builds without release linker flags print `dev`.

---

## Environment variables

| Variable | Used by | Meaning |
|---|---|---|
| `JAVA_HOME` | `run`, `push --appcds` | Default node JDK home if `--jdk-root` is unset (`run`); default training JVM if `--appcds-java` is unset (`push --appcds`). |
| `BREWLET_JDK_HOME` | `run` | Overrides `JAVA_HOME` for JDK resolution. |
| `BREWLET_STORE_ROOT` | shim (`layout` resolver) | OCI layout root used by the local layout resolver. |
| `BREWLET_CDS_CACHE` | `run`, `bundle`, shim | Node AppCDS regeneration cache dir (default `/opt/brewlet/cds`). Used when a deployment opts into `spec.jvm.cds.regenerate` (or `--appcds-regenerate` locally) ([AppCDS §4.3](appcds.md)). |
| `BREWLET_METRICS_DIR` | `run`, shim | Directory for the best-effort node-local CDS metric textfile (`brewlet_cds_archive_mapped`). Unset disables the metric. |

---

## Exit codes

- `0` — success.
- `1` — runtime error (message on stderr, prefixed `error:`).
- `2` — usage error (unknown/mis-invoked command; prints usage).

For `run`, the launched JVM's exit code propagates as the process exit status.

## See also

- [Building & publishing](building-and-publishing.md) — the developer workflow.
- [Getting started](getting-started.md) — integration-test tiers 2 and 3 exercise
  these commands against the monorepo source.
- [Reference](reference.md) — media types, schema, and well-known paths.
