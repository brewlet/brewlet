# JDK management

The JDK is the part Brewlet moves **off** your image and **onto** the node. This
page covers how JDK runtime roots get installed, versioned, patched, and served to
workloads — the platform team's core operational surface.

Related: [Configuration](configuration.md) · [Launchers](launchers.md) ·
[Security](security.md).

---

## The model

- The platform team declares a **JDK inventory** once (Helm values / operator
  flags). Nothing about JDKs is baked into application artifacts.
- Workloads request a JDK once in the deployment descriptor:
  `spec.jvm.version` (feature, e.g. `21`) with an optional `spec.jvm.distribution`
  (e.g. `microsoft`) on `JavaApplication`, or `brewlet.sh/jdk` on raw Kubernetes
  pods. The controller folds version + distribution into the `brewlet.sh/jdk`
  annotation: a bare feature (`"21"`) matches **any** distribution of that
  version, while `"<distribution>-<feature>"` (`"microsoft-25"`) pins an exact
  root. The launcher request follows the same pattern (`spec.jvm.launcher` /
  `brewlet.sh/launcher`) and may be omitted for vanilla `java`.
- On every opted-in node, the provisioner materializes each declared JDK as a
  **read-only, shared image root** at:

  ```
  /opt/brewlet/jdks/<distribution>-<feature>/
  ```

- The root contains the source image's complete userland plus
  `.brewlet-java-home`, which records the JDK or jlink runtime location inside
  that root. The shim uses the complete root as the sandbox filesystem and
  bind-mounts the recorded Java home at the stable `/opt/jdk` path.
- The inventory path is what the shim's `selectJDK` resolves when a pod requests
  a JDK. The operator/webhook validates descriptor requests, injects node affinity
  for matching capability labels, and propagates the annotations onto the OCI
  runtime spec. The shim reads those annotations at launch; if `brewlet.sh/jdk` is
  absent it defaults to feature 21, selecting the lexically-first installed
  distribution for that feature (no built-in vendor preference). One root is
  shared (overlay-mounted read-only)
  into every JVM sandbox.
- Roots are **versioned and additive**: patching = drop in a new root, retire old
  ones. Running pods keep their JDK until they restart.

Declare the inventory:

```yaml
# Helm values
provisioner:
  jdks: "temurin-21,microsoft-25"   # two roots on every opted-in node
```

The node then advertises what it installed:

```bash
kubectl get node node-1 -o jsonpath='{.metadata.annotations.brewlet\.sh/jdks}{"\n"}'
# temurin-21,microsoft-25
```

…and emits per-capability scheduling labels the admission webhook matches against
(`brewlet.sh/jdk.temurin-21`, `brewlet.sh/jdk-feature.21`, …). A pod requesting a
JDK that no ready node provides fails admission with `NoCompatibleJDK`
([Troubleshooting](troubleshooting.md)).

---

## Inspecting the JDKs available on the cluster

Developers frequently need to know *exactly* which JDKs production offers —
**vendor, major version, minor version, and architecture** — so they can match
their local and CI toolchains. Alongside the coarse `brewlet.sh/jdks` list, each
node publishes a rich inventory annotation, `brewlet.sh/jdks-info`, a JSON array:

```json
[
  {"distribution":"temurin","vendor":"Eclipse Adoptium","feature":21,"version":"21.0.5","arch":"amd64"},
  {"distribution":"microsoft","vendor":"Microsoft","feature":25,"version":"25","arch":"amd64"}
]
```

The `vendor`, `version` (full/minor), and `arch` fields are read straight from the
installed JDK (`java -XshowSettings:properties`) at provision time, so they reflect
the actual build on the node — not just what was requested.

### With `brewlet jdks`

The CLI aggregates that inventory across the fleet:

```bash
brewlet jdks
# VENDOR             DISTRIBUTION   MAJOR   VERSION   ARCH    NODES
# Microsoft          microsoft      25      25        amd64   3
# Eclipse Adoptium   temurin        21      21.0.5    amd64   3
# Eclipse Adoptium   temurin        21      21.0.5    arm64   2

brewlet jdks --output wide     # one row per node
brewlet jdks --output json     # machine-readable, e.g. for CI matrix generation
```

It reads nodes through `kubectl` (respecting `--kubeconfig`, `--context`, and a
`--selector`), so it needs no extra cluster access beyond what `kubectl get nodes`
already has. See the [CLI reference](cli-reference.md#brewlet-jdks).

### With plain `kubectl`

No CLI required — the annotation is queryable directly:

```bash
# rich inventory for one node
kubectl get node node-1 \
  -o jsonpath='{.metadata.annotations.brewlet\.sh/jdks-info}{"\n"}'

# across the fleet
kubectl get nodes \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.brewlet\.sh/jdks-info}{"\n"}{end}'
```

> Nodes that were provisioned before this annotation existed still carry the
> coarse `brewlet.sh/jdks` list; `brewlet jdks` falls back to it (showing only
> distribution + major) so mixed fleets still render.

---

## Installation: copy-from-image

Brewlet obtains every JDK root **copy-from-image**: the provisioner pulls the
vendor's official JDK image through the **host** containerd, mounts its unpacked
root, and copies the complete userland onto the host `hostPath` — no package
manager ever touches the host, and no vendor tarball endpoints need to be
reachable. It requires the containerd socket and host mount namespace access
(already wired into the DaemonSet).

```bash
ctr --address /run/containerd/containerd.sock --namespace k8s.io image pull "$image"
ctr --address /run/containerd/containerd.sock --namespace k8s.io images mount "$image" /tmp/jdk-root
cp -a /tmp/jdk-root/. /opt/brewlet/jdks/<dist>-<feature>/
ctr --address /run/containerd/containerd.sock --namespace k8s.io images unmount /tmp/jdk-root
```

The built-in distribution → image map:

| `distribution` | Image |
|---|---|
| `temurin` | `eclipse-temurin:<feature>` |
| `microsoft` | `mcr.microsoft.com/openjdk/jdk:<feature>-ubuntu` |

`temurin` and `microsoft` are curated: their image and Java-home mappings are
built into the provisioner. Pulls go by image reference, so mirror these images
into your own registry for air-gapped clusters.

Because images are content-addressable (pulled by digest through containerd), you
get integrity end-to-end without a separate checksum step.

### Custom distributions: Azul Zulu example

A Kubernetes administrator can install any image-packaged JDK by declaring its
fully qualified OCI image and the absolute Java-home path inside that image:

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: zulu
spec:
  nodePool:
    names: ["zulu-workers"]
  jdks:
    - distribution: zulu
      feature: 21
      source:
        image: docker.io/library/azul-zulu:21
        javaHome: /usr/lib/jvm/zulu21
  rollout:
    validate: true
    containerdRestart: validated
```

The Docker Official `azul-zulu:21` image is multi-architecture and exposes
`JAVA_HOME=/usr/lib/jvm/zulu21`. Brewlet pulls the platform matching each node,
copies that root to `/opt/brewlet/jdks/zulu-21`, runs `java -version`, and only
then advertises `brewlet.sh/jdk.zulu-21=true`.

For production:

1. Pin `source.image` by multi-architecture manifest digest.
2. Verify the image covers every architecture in the selected node pool.
3. Verify `source.javaHome/bin/java` exists in every platform variant.
4. Add the source registry host to `spec.registry.mirrors` when nodes use an
   internal mirror.

Curated distributions must omit `source`; custom distributions must provide it.
The distribution name becomes part of node labels and must be a lowercase
DNS-1123 label.

### Shared jlink runtimes

A custom source may be a platform-owned **jlink runtime image** instead of a full
vendor JDK. This preserves Brewlet's model: applications still ship only JARs,
while one centrally patched runtime and module set is installed once per node
pool. It does not support embedding a separate jlink runtime in each application
artifact.

For example, a platform team can build a runtime containing approved JDK modules
and an organization module:

```dockerfile
FROM eclipse-temurin:21-jdk AS build
COPY platform-modules/ /platform-modules/
RUN jlink \
    --module-path "$JAVA_HOME/jmods:/platform-modules" \
    --add-modules java.base,java.logging,java.net.http,com.example.platform \
    --strip-debug --no-man-pages --no-header-files --compress=zip-6 \
    --output /runtime

FROM debian:bookworm-slim
COPY --from=build /runtime /opt/java/runtime
```

Publish each required architecture under one multi-architecture image reference,
then configure the runtime exactly like another custom distribution:

```yaml
spec:
  jdks:
    - distribution: platform
      feature: 21
      source:
        image: registry.example.com/java/platform-runtime@sha256:<manifest-digest>
        javaHome: /opt/java/runtime
```

The final image must contain the operating-system loader and libraries needed by
`javaHome/bin/java`; Brewlet uses its complete root as the sandbox userland. The
image does not need `sh`, `cp`, or `tar`. Rebuild and roll out the shared image
when its JDK patch level or centrally approved module set changes.

There is no distribution-name aliasing and no `lts`/`latest` feature shortcut:
the `<distribution>-<feature>` token is taken verbatim so the root path and the
`brewlet.sh/jdk-feature.<feature>` label stay stable and reproducible for the
workloads that pin them.

---

## Architecture mapping (multi-arch)

The node's `uname -m` is mapped to the vendor's arch token:

| Node (`uname -m`) | Temurin / MS token |
|---|---|
| `x86_64` / `amd64` | `x64` |
| `aarch64` / `arm64` | `aarch64` |

Install **one root per node architecture** (amd64/arm64). The provisioner image and
shim are compiled per-arch, but the **OCI artifact is arch-neutral** — the *same*
artifact runs on any provisioned architecture. Multi-arch is transparent to
developers.

---

## Patching & upgrading JDKs

JDK CVE management is **centralized** — patching the node JDK patches *all* workloads
at once, the big advantage over per-image JVMs.

To roll out a new JDK across the fleet:

1. Add the new root to the inventory (e.g. add `temurin-21` alongside an older
   `temurin-17`), keeping both while you migrate:
   ```bash
   helm upgrade brewlet ./charts/brewlet \
     --set provisioner.jdks="temurin-17,temurin-21"
   ```
2. The operator rolls the DaemonSet; each node installs the new root additively.
   Running pods are unaffected.
3. Migrate workloads to the new feature (`spec.jvm.version` or raw pod
   `brewlet.sh/jdk`), then restart them to pick up the new root.
4. Retire the old root once nothing references it:
   ```bash
   helm upgrade brewlet ./charts/brewlet --set provisioner.jdks="temurin-21"
   ```

For a *patch* within the same feature (e.g. `21.0.3 → 21.0.4`), re-provisioning the
root replaces the shared JDK installation; pods pick it up on their next restart. Because roots
are read-only and shared, no per-pod pull/unpack happens.

> **Idempotency.** The provisioner treats a root as present once the Java home
> recorded by `<dist>-<feature>/.brewlet-java-home` contains a runnable `bin/java`.
> To force a re-install of a patched root, remove the existing directory on the
> node (or bump to a version that lands in a different `<dist>-<feature>` path)
> before re-provisioning.

---

## Licensing

Brewlet is **distribution-neutral** and pins nothing. Ship only OpenJDK builds whose
license you accept — the platform team chooses the builds. The curated copy-from-image
map covers Temurin and the Microsoft Build of OpenJDK; add other published JDK images
as needed.

---

## Known limitations (PoC)

- Custom images must expose the same Java-home path on every architecture in the
  selected node pool.
- **A requested `<feature>` must be published as an image** by the chosen vendor for
  the node's architecture.
- **containerd reload is `SIGHUP`.** On some distros a full `systemctl restart
  containerd` is more reliable for picking up the new runtime; operator-driven
  hardening is a follow-up.

## Next steps

- **[Launchers](launchers.md)** — install `jaz` alongside your JDKs.
- **[Configuration](configuration.md)** — where the inventory is set.
- **[Observability & day‑2](observability.md)** — upgrade choreography and GC of old
  roots.
