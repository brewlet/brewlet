# Security

Brewlet keeps **container-grade isolation** (runc) while adopting a Wasm-grade
developer experience. This page covers the isolation model, defaults, digest
verification, and the one genuinely sharp edge: privileged node provisioning.

See also [SPECIFICATION §11](https://github.com/brewlet/brewlet/blob/main/specs/SPECIFICATION.md).

---

## Isolation parity with containers

Execution is **runc-backed**, so a Brewlet workload gets the same isolation
primitives as any ordinary pod:

- **namespaces** (pid/net/mount/ipc/uts) and **cgroup v2** resource control;
- **seccomp / AppArmor** profiles;
- **CNI** networking (a real, isolated pod netns and pod IP).

The JAR is treated as **untrusted code** and runs inside that sandbox — nothing about
"it's just a JAR" weakens the boundary relative to a container image.

---

## Non-root by default

The JVM runs as an **unprivileged uid**; root is squashed unless explicitly
requested. Set the identity via the artifact's `user` (uid/gid) or the pod
`securityContext`:

```yaml
spec:
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    runAsGroup: 1000
  runtimeClassName: brewlet
  containers:
    - name: app
      image: registry.example.com/team/app:1.4.2
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true          # the JDK root is RO already
        capabilities: { drop: ["ALL"] }
```

The JDK runtime root is mounted **read-only** and shared; only a small per-container
upper/scratch layer is writable.

---

## Artifact integrity

Because the JAR is a first-class OCI artifact, standard supply-chain controls apply:

- **Digest-pin** artifact references (`repo@sha256:…`). The admission webhook stamps
  `brewlet.sh/artifact-digest`, and the shim resolves the JAR straight from
  containerd's content store by digest. See [Building & publishing](building-and-publishing.md#4-pin-to-a-digest-recommended).

### Supply-chain attestations

[Managed dependency bundles](managed-dependency-bundles.md) can carry optional
bundle-publisher provenance. A final thin-JAR image can independently carry a
DSSE envelope containing an in-toto Statement v1 whose managed-dependency
predicate binds the image digest, thin-JAR verdict, application JAR, bundle,
dependency layer, lock, SBOM, source BOM, and application-builder identity.
Signatures use ECDSA P-256 and a `keyid` derived from the public key.

The two identities are separate trust roles. Bundle-publisher trust is verified
upstream while composing the image; the final-image predicate exposes only the
application-builder identity. Identity values are free-form signed strings
trusted through the configured key, not OIDC- or Fulcio-issued identities.

Deploy [Ratify/Gatekeeper admission enforcement](admission-enforcement.md) to
require the final-image attestation for `runtimeClassName: brewlet` pods. It
requires an OCI 1.1 Referrers-API registry and digest-pinned images and fails
closed when trusted evidence cannot be discovered or verified.

---

## Centralized JDK CVE management

The single biggest security win: the JVM lives on the node, shared across workloads.
**Patching the node JDK patches every workload at once** — no rebuilding and
re-pushing hundreds of images to ship a JVM CVE fix. See
[JDK management](jdk-management.md#patching-upgrading-jdks).

---

## The sharp edge: privileged provisioning

Node provisioning is **privileged and mutates the host**. The
`brewlet-node-provisioner` DaemonSet:

- runs privileged with `hostPID`;
- writes the shim to the host `PATH` and JDK/launcher roots under `/opt/brewlet`;
- edits `/etc/containerd/config.toml` and reloads containerd.

Mitigations and guardrails:

| Guardrail | How |
|---|---|
| **Provisioning is scoped, but broad by default** | The chart's default `NodeProfile` provisions **every** node (§5.6). To limit the blast radius, disable it (`defaultProfile.enabled=false`) and define named `NodeProfile`s scoped to platform-owned pools. The legacy standalone DaemonSet instead touches only nodes carrying the `brewlet.sh/provision=true` **label**. |
| **Scope to platform-owned pools** | Use named `NodeProfile` pools (or the `brewlet.sh/provision` label for the standalone path) to restrict provisioning to nodes your platform team controls. Do **not** provision shared/hostile multi-tenant nodes. |
| **The operator is unprivileged** | The operator only talks to the API server; only the DaemonSet it manages is privileged. |
| **Webhook can't block workloads** | `admission.failurePolicy: Ignore` (default) means a webhook outage never wedges deployments. |

> ⚠️ Treat enabling Brewlet on a node the same way you'd treat any privileged
> node-bootstrap DaemonSet (a pattern also used for node runtime installation in
> the Wasm ecosystem). Document the blast radius.

---

## Multi-tenancy guidance

- **Trusted tenants / your own services:** runc isolation is equivalent to ordinary
  containers — appropriate as-is.
- **Untrusted or hostile JARs:** keep Brewlet provisioning off shared nodes. runc
  provides the same trust boundary as a normal container, no more and no less.

---

## Hardening checklist

- [ ] Provision only platform-owned node pools; use named `NodeProfile`s for
      operator-managed installations or `brewlet.sh/provision` only for the
      legacy standalone path. See
      [Capability labels and autoscaling](capability-labels-and-autoscaling.md).
- [ ] Pin component images and OCI artifacts to **digests**.
- [ ] Run workloads `runAsNonRoot`, drop capabilities, `readOnlyRootFilesystem`.
- [ ] Use **cert-manager** for the admission webhook serving cert in production
      (not the Helm self-signed cert). See [Configuration](configuration.md#admission-webhook).
- [ ] Require trusted final-image managed-dependency attestations with
      [admission enforcement](admission-enforcement.md); pin images and the
      verifier plugin to digests.
- [ ] Plan JDK patch cadence — it's now a single centralized lever.

Future security capabilities are tracked in the [roadmap](https://github.com/brewlet/brewlet/blob/main/ROADMAP.md#security-and-isolation).
