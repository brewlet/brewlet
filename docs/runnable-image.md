# Runnable-image delivery (kubelet-pullable, the WASI-style pull path)

> **Status.** Design + **implemented in the PoC**. `brewlet push --format=image`, the
> shim's runnable-image resolver, and end-to-end proof (e2e tier 12) ship in the
> reference CLI and shim. This note documents the delivery contract referenced by
> [SPECIFICATION §4.4](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#4-the-oci-application-artifact). It answers
> the question "how does a `runtimeClassName: brewlet` pod name the app as its `image:`
> and let kubelet pull it, exactly like a `runtimeClassName: wasmtime` pod names a
> Wasm module?"

---

## 1. TL;DR

- **The native Brewlet artifact is registry-native but not runnable by containerd.**
  Its custom layer media types (`application/vnd.brewlet.jar.layer.v1+jar`,
  `…classpath.layer.v1+tar`, `…modulepath.layer.v1+tar`) are not among the media types
  containerd's CRI differ can unpack (`tar`, `tar+gzip`, `tar+zstd`). A pod that sets
  `image: <native-artifact-ref>` therefore **fails to pull** (`ImagePullBackOff`); the
  payload has to reach the node **out of band** (a future node pre-puller, or the e2e
  harness's `ctr images import`).
- **Runnable-image mode fixes this without changing the native format.** `brewlet push
  --format=image` publishes the *same* JAR as a **standard, kubelet-pullable OCI
  image**. containerd/kubelet pull + unpack it with no special configuration; the shim
  recognizes it and runs it on the node-resident JDK.
- **The developer experience becomes the WASI/KWasm one:** `image: <ref>` +
  `runtimeClassName: brewlet` and nothing else. kubelet pulls, containerd unpacks, the
  shim launches `java -jar` under the pod's cgroup.
- **Runnable-image mode is now the default.** `brewlet push` (and `mvn brewlet:push`)
  produce a runnable image unless you opt into `--format=artifact`
  (`-Dbrewlet.format=artifact`). It is the delivery path that fulfils the WASI/KWasm
  parity goal, so it is the out-of-the-box behaviour; the native artifact remains an
  opt-in choice for registry-native / pre-puller flows.

---

## 2. Why the native artifact can't be a pod `image:`

containerd's CRI `PullImage` **unpacks** every layer into the snapshotter *before* the
runtime shim's `Create` runs. Unpacking dispatches on the layer's media type, and the
differ only understands `tar`, `tar+gzip`, and `tar+zstd`. Brewlet's native layers use
bespoke `+jar`/`+tar` media types so the artifact stays self-describing and
registry-native — but that is precisely what makes `crictl`/kubelet unable to unpack
them. The pull fails long before the shim is ever consulted.

That is fine for a registry + out-of-band node delivery model, but it means the pod
cannot *name* the artifact as its image. Tiers 8 and 9 of the e2e suite work around
this by importing the artifact straight into the node content store and giving the pod
a **busybox placeholder image** plus `brewlet.sh/artifact-*` annotations — proving the
runtime, but not the `image: <ref>` promise.

## 3. What `--format=image` publishes

```bash
# Same JAR, same launch contract — published as a runnable OCI image.
brewlet push ./target/app.jar registry.example.com/team/app:1.4.2 --format=image
```

The result is an ordinary OCI **image index** (multi-arch) whose per-arch manifests are
plain OCI images:

| Piece | What it is |
|-------|-----------|
| Manifest media type | `application/vnd.oci.image.manifest.v1+json` |
| Index media type | `application/vnd.oci.image.index.v1+json` (multi-arch) |
| Config | `application/vnd.oci.image.config.v1+json` — a **real** image config whose `rootfs.diff_ids` are the sha256 of the **uncompressed** layer tars |
| Layers | `application/vnd.oci.image.layer.v1.tar+gzip` — standard, unpackable |
| Launch config | the §4.2 launch descriptor, carried verbatim in the manifest annotation `brewlet.sh/jvm-config` |
| Layer roles | each layer tagged with `brewlet.sh/layer` = `app` \| `classpath` \| `modulepath` |

Layer layout:

- **app layer** — a flat tar containing the main JAR (named per `mainJar`) plus an
  optional AppCDS `.jsa`.
- **classpath / modulepath layers** — the *same* flat-JAR tars a native artifact would
  ship for [layered classpath](layered-classpath-deployment.md) / [JPMS](jpms-support.md)
  deployments, just gzip-compressed and role-tagged.

**Multi-arch by default.** A portable bytecode JAR is published for `amd64` + `arm64`
(identical layers, per-arch config differing only in `architecture`) so any provisioned
node matches. A JAR carrying native libraries narrows this with `--arch amd64,arm64`
(see [multi-arch.md](multi-arch.md)).

> **OCI correctness note.** A layer descriptor's `digest` is the sha256 of the
> **gzipped** blob, but the image config's `rootfs.diff_ids[i]` must be the sha256 of
> the **uncompressed** tar. Getting this wrong makes containerd reject the image on
> unpack. The writer computes both; a unit test asserts the diff-ids equal the
> uncompressed digests.

## 4. How the shim runs it

On the node the shim distinguishes the two formats by the manifest: the presence of the
`brewlet.sh/jvm-config` annotation ⇒ runnable image (otherwise ⇒ native artifact, the
raw-blob path, unchanged). For a runnable image the shim:

1. follows the image index to the node's **platform** manifest (by `GOARCH`);
2. decodes the launch config from `brewlet.sh/jvm-config`;
3. gunzips the app layer to recover the JAR (and any `.jsa`), and gunzips each
   classpath/modulepath layer to a temporary tar;
4. feeds those tars to the **existing** `StageClasspathLayers` / `StageModulepathLayers`
   bundle-assembly path — so runnable images and native artifacts converge on the same
   `java -jar` / `-cp` / `-p -m` sandbox on the node-resident JDK, under the pod's
   cgroup limits.

Nothing about JVM launch, cgroup-awareness, JDK/launcher selection, or the KWasm-style
overlay rootfs (shared read-only JDK lower + per-container upper) changes.

## 5. Operator & webhook: no change required

- The `JavaApplication` controller already sets the Deployment's container
  `image:` to `spec.artifact.image`. With a runnable image that ref is now **pullable**,
  so the happy path just works.
- The admission webhook still stamps `brewlet.sh/artifact-ref` + `brewlet.sh/artifact-digest`
  from the (digest-pinned) ref; for a runnable image the digest is the **image-index**
  digest, which the shim resolver follows to the platform manifest.

## 6. When to use which

| | Runnable image (default) | Native artifact (`--format=artifact`) |
|--|--------------------------|----------------------------------------|
| Media types | standard OCI `tar+gzip` | custom `+jar` / `+tar` |
| Pod `image: <ref>` pulls via kubelet | ✓ | ✗ (needs out-of-band delivery) |
| Registry-native / smallest | slightly larger (OS-image framing) | ✓ |
| Node delivery | kubelet `PullImage`, like any image | pre-puller / import |
| Developer UX | pure WASI-style `image: <ref>` | ref + node delivery |

The default runnable image gives the pure `image: <ref>` experience end to end — the
WASI/KWasm parity goal. Opt into `--format=artifact` when you have (or are building) a
node pre-puller and want the leanest registry footprint / self-describing media types.

## 7. Proof (e2e tier 12)

Tier 12 of the [e2e suite](https://github.com/brewlet/integration-tests/blob/main/README.md) provisions a real `kind`/CI node,
`brewlet push --format=image`s the demo JAR, imports it into the node's `k8s.io`
content store, and asserts **`ctr images unpack` SUCCEEDS** — the exact operation that
`ImagePullBackOff`s for a native artifact. It then runs a `runtimeClassName: brewlet`
Deployment whose container **`image:` is the brewlet ref itself** (no placeholder,
`imagePullPolicy: Never`) and asserts the pod is Ready with that image, serves a `200`
from `/hello`, and that the JVM is cgroup-aware (`availableProcessors == 1`, bounded
`maxMemory`).

## 8. Implementation status

- **Implemented (PoC):** `brewlet push --format=image`, `brewlet inspect` (reports
  native artifact vs runnable image), the shim's runnable-image resolver (both the
  `containerd` content-store backend and the `layout` test backend), multi-arch index
  emission + `GOARCH` resolution, and e2e tier 12. The **Maven plugin** reaches parity:
  `mvn brewlet:push -Dbrewlet.format=image` (or `<format>image</format>`) publishes a
  byte-compatible runnable image via `RunnableImageBuilder`, and `brewlet:inspect`
  reports the runnable-image shape.
- **Not yet:** a Gradle plugin does not exist yet (it is on the roadmap — see
  [SPECIFICATION §15](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#15-phased-roadmap), Phase 5 — and will reach
  format parity with the Maven plugin when it lands). A registry-based variant of tier
  12 (kubelet pulls over the network rather than a side-loaded import) is also a future
  addition — the pulled bytes are byte-identical, so the shim path is the same.
