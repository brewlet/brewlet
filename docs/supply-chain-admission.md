# Supply-chain verification (cosign / SLSA) — research

> **Status.** Research / design note. Nothing here is implemented yet — it
> fleshes out the Phase 4 roadmap item *"cosign/SLSA admission"*
> ([SPECIFICATION §11](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#11-security-model), §15) and the
> "supply-chain verification" bullet in [security.md](security.md#supply-chain-verification).
> It documents *intended* behavior and the design choices, not shipped code.

---

## 1. TL;DR

- **This is a natural fit and arguably a headline feature.** Brewlet's whole pitch
  is *"stop owning base-image CVEs"*. The complementary control is proving the one
  artifact you *do* still ship — the JAR — is the one your CI built and hasn't been
  tampered with. Because a Brewlet artifact is an **ordinary OCI artifact** stored
  in an OCI registry, cosign signatures and SLSA provenance attestations attach to
  it exactly as they do to a container image.
- **It is a platform-operator concern, not a developer concern.** Developers sign
  in CI (one `cosign sign` / provenance step); platform teams enforce a policy.
  Enforcement belongs at **admission**, keyed on the artifact **digest**.
- **The recommended primary path reuses the ecosystem, not new Brewlet code.**
  Brewlet artifacts are normal digest-pinned refs, so **Sigstore
  policy-controller, Kyverno, or Connaisseur verify them out of the box.** Brewlet
  should *document the pattern* and make signing turnkey in the CLI / Maven plugin.
- **An optional Brewlet-native verifier** (a second, *fail-closed* validating
  webhook) is worth offering for clusters that don't run a policy engine — but it
  must be a **separate** webhook from today's mutating steering webhook, whose
  `failurePolicy: Ignore` is deliberately fail-*open* for availability (§4.3).
- **Verification requires digest pinning.** Tag refs cannot be safely verified; the
  verify path must reject non-digest refs (Brewlet already stamps
  `brewlet.sh/artifact-digest` for pinned refs — §8.3).

---

## 2. Background

### 2.1 What we are verifying

Two related but distinct supply-chain controls, both Sigstore/OCI-native:

| Control | Question it answers | Mechanism |
|---|---|---|
| **cosign signature** | "Was this exact artifact signed by a key/identity I trust?" | A signature stored as a sibling tag (`sha256-<digest>.sig`) in the same repo, verified against a public key (KMS/file) or a keyless Fulcio identity (OIDC + Rekor transparency log). |
| **SLSA provenance** | "Was this artifact produced by the build system/workflow I expect, from the source I expect?" | An in-toto attestation (predicate type `https://slsa.dev/provenance/v1`) attached to the digest, verifiable with `cosign verify-attestation` or `slsa-verifier`. |

Both bind to the **manifest digest** (`sha256:…`). That is the crucial property for
Brewlet: the shim already resolves the JAR from containerd's content store *by
digest* (§6.4), and the admission webhook already stamps that digest
(`brewlet.sh/artifact-digest`, `operator/internal/brewlet/labels.go`).

### 2.2 Do Brewlet artifacts actually support this?

Yes, with no format change. `PushWithLayers`
(`internal/artifact/artifact.go`) writes a standard OCI manifest
(`application/vnd.oci.image.manifest.v1+json`) with a custom `artifactType`.
cosign signs *any* OCI object addressable by digest and stores the signature as a
sibling object — it does not care that the payload is a JAR rather than an image.
The only registry requirement (allowing the `.sig` / attestation sibling tags) is
already met by any registry that can host the Brewlet artifact itself.

---

## 3. Alignment with the Brewlet model

- **Removes the *other* half of the CVE story.** Brewlet removes the base image and
  centralizes the JDK; supply-chain verification protects the residual artifact.
  Together they give "provenance for the app + centrally-patched runtime", which is
  a stronger story than a signed-but-monolithic container image.
- **Digest-first is already the house style.** Docs recommend digest pins
  throughout ([building & publishing](building-and-publishing.md#4-pin-to-a-digest-recommended)),
  the webhook stamps the digest, and the shim resolves by digest. Verification
  simply *requires* what Brewlet already recommends.
- **Keeps developer ergonomics.** Signing is one CI step; the developer never
  writes policy. This matches Phase 2's "developers never touch ORAS" ethos.

**Conclusion:** in scope, high value, and cheap on the developer side. The design
question is *where* enforcement lives.

---

## 4. Where to enforce — three options

### 4.1 Option A (recommended primary): delegate to a policy engine

Because a Brewlet artifact ref is a normal OCI reference, an existing admission
policy engine enforces signatures/provenance with **zero Brewlet code**:

- **Sigstore policy-controller** — a `ClusterImagePolicy` matching the registry
  glob, requiring a keyless identity or a public key.
- **Kyverno** — a `verifyImages` rule (`cosign` / `notary` / attestations).
- **Connaisseur / Ratify + Gatekeeper** — equivalent.

Scope the policy to Brewlet workloads by **namespace** and/or a label; policy
engines match on the *image reference*, and a Brewlet pod's artifact ref is that
reference. Example (Kyverno, illustrative):

```yaml
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata: { name: verify-brewlet-artifacts }
spec:
  validationFailureAction: Enforce
  rules:
    - name: verify-cosign
      match:
        any:
          - resources:
              kinds: [Pod]
              # scope to brewlet workloads by namespace/label
              selector: { matchLabels: { "app.kubernetes.io/managed-by": "brewlet-operator" } }
      verifyImages:
        - imageReferences: ["registry.example.com/team/*"]
          attestors:
            - entries:
                - keyless:
                    subject: "https://github.com/team/*/.github/workflows/release.yml@refs/tags/*"
                    issuer: "https://token.actions.githubusercontent.com"
```

**Pros:** no new attack surface in Brewlet; teams reuse audited tooling and their
existing key/identity policy. **Cons:** requires running a policy engine; the match
is by image glob, not by "is a brewlet pod", so scoping is by convention.

### 4.2 Option B (optional Brewlet-native): a fail-closed verify webhook

For clusters without a policy engine, ship an **opt-in** verifying webhook
(`brewlet-verify`) that, for pods with `runtimeClassName: brewlet`:

1. Requires a digest-pinned ref (rejects tag refs — verification is meaningless
   without a pinned digest).
2. Verifies a cosign signature and/or a required SLSA provenance predicate over
   that digest, using the `sigstore/cosign` Go libraries.
3. Admits or denies with a clear reason (e.g. `UnsignedArtifact`,
   `UntrustedSigner`, `MissingProvenance`).

**This must NOT be folded into the existing mutating webhook** (`PodMutator`,
`operator/internal/admission/webhook.go`). That webhook is deliberately
`failurePolicy: Ignore` (fail-*open*) so a webhook outage never wedges deployments
(§4.3, `values.yaml`). A *security* gate must be fail-*closed* — an outage should
deny, not silently bypass. Reconcile the tension by making verification a **second,
separate `ValidatingWebhookConfiguration`** with `failurePolicy: Fail`, so the two
concerns (availability-preserving steering vs. fail-closed verification) don't
share a failure mode.

**Cons:** the webhook needs registry egress and key/identity configuration, becomes
a hot path (cache verification results by digest), and re-implements what policy
engines already do. Offer it, don't default it.

### 4.3 Option C (defense in depth, later): shim-side verification at launch

The shim already reads the manifest by digest from the content store; it *could*
verify a signature before `runc create`. This catches a JAR that reached the node
by a path that bypassed admission. But the shim is an ephemeral, per-container,
network-shy process; giving it registry/key access and Rekor connectivity is
heavy, and launch-time is late. Treat as a possible hardening layer *after* A/B,
not a primary control.

### 4.4 Recommendation

Ship **A as the documented default** (patterns + examples), **B as an opt-in
chart-enabled webhook** for policy-engine-less clusters, and keep **C** as a
future note. Make the developer side turnkey regardless (§6).

---

## 5. What existing features this touches

| Area | Interaction |
|---|---|
| **Admission webhook (§8.3)** | Do **not** add verification to the fail-open mutating webhook. Add a *separate* fail-closed validating webhook (Option B). |
| **Digest stamping** | Verification depends on `brewlet.sh/artifact-digest`, already stamped for pinned refs. The verify path must **require** a digest (deny tag refs). |
| **`JavaApplication` controller (§8.2)** | Encourage/allow digest-pinned `spec.artifact.image`; optionally surface `spec.artifact.pullSecrets` for private registries the verifier also reads. Consider a validation that warns on tag refs when verification is enabled. |
| **CLI / Maven plugin (Phase 2)** | Add signing/provenance emission so the developer experience stays one step (§6). |
| **Shim content resolution (§6.4)** | Unchanged for A/B; the optional Option C would hook here. |
| **Helm chart / values** | New `verify:` block (enable, keys/identity, required predicates, failurePolicy) gated behind `admission.verify.enabled=false` by default. |
| **Docs** | Promote [security.md → supply-chain](security.md#supply-chain-verification) from "roadmap" to a configured feature; add a signing recipe to [building & publishing](building-and-publishing.md). |

---

## 6. Making the developer side turnkey

Signing must be trivial or teams won't do it. Two hooks:

- **`brewlet push --sign`** (keyless by default in CI): after `PushWithLayers`
  resolves the manifest digest, invoke cosign to sign `repo@sha256:…`. Emit the
  digest to stdout (already the recommended pin source).
- **Maven plugin** (`brewlet:push`): a `<sign>true</sign>` option that shells to
  cosign or uses `sigstore-java`, plus optional SLSA provenance via the CI
  provider (e.g. `slsa-github-generator` attaching an attestation to the digest).

Both operate on the digest the push already computes, so there's no new artifact
plumbing — just an extra registry write of the sibling signature/attestation.

---

## 7. Design caveats

- **Digest pinning is mandatory for verification.** A tag ref is a moving target; a
  verifier that "resolves the tag then verifies" has a TOCTOU gap. Deny tag refs in
  verify mode. This nudges users toward the digest pins Brewlet already recommends.
- **Fail-open vs. fail-closed is a real fork.** Keep the two webhooks separate
  (§4.2). Document that enabling fail-closed verification changes the availability
  posture: a verifier outage blocks new brewlet pods (intended for a security gate).
- **Key management is the hard part, and it's not Brewlet's to solve.** Prefer
  keyless (Fulcio/OIDC) or a cluster KMS; Brewlet only *consumes* a trust policy.
- **Signatures live as sibling tags.** Registry GC / retention policies must not
  reap `sha256-….sig` / attestation objects while the artifact is deployed.
- **Provenance predicate choice matters.** Decide whether to require SLSA
  provenance in addition to a signature (stronger: proves *how* it was built) or a
  signature alone (proves *who* signed). Make it policy, not hard-coded.

---

## 8. Recommendation & phasing

1. **Phase A — document + turnkey signing (no operator code).** Publish
   policy-engine recipes (Kyverno / policy-controller) and add `--sign` to the CLI
   and the Maven plugin. This alone lets teams enforce today with audited tooling.
2. **Phase B — optional native verify webhook.** A separate, fail-closed
   `brewlet-verify` `ValidatingWebhookConfiguration`, chart-gated
   (`admission.verify.enabled`), requiring digest pins, verifying cosign and/or a
   configured SLSA predicate, with per-digest caching. Clear
   `UnsignedArtifact`/`UntrustedSigner`/`MissingProvenance` denial reasons that
   join the §14 failure-mode table.
3. **Phase C — (optional) shim-side launch verification** as defense in depth once
   A/B are in place.

None of this changes the artifact format, the launch core, or the resource→JVM
mapping. Verification is a **digest-keyed admission policy**, which sits cleanly
beside the digest resolution Brewlet already performs.

---

## 9. References

- [Sigstore cosign](https://docs.sigstore.dev/) — signing/verifying OCI artifacts;
  keyless (Fulcio) and Rekor transparency log.
- [SLSA provenance](https://slsa.dev/spec/v1.0/provenance) and
  [slsa-verifier](https://github.com/slsa-framework/slsa-verifier).
- [Sigstore policy-controller](https://docs.sigstore.dev/policy-controller/overview/),
  [Kyverno image verification](https://kyverno.io/docs/writing-policies/verify-images/).
- Brewlet: [SPECIFICATION §11 (security)](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md#11-security-model),
  [§8.3 (admission webhook)](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md), [security.md](security.md),
  [building & publishing](building-and-publishing.md);
  `operator/internal/admission/webhook.go`, `.../mutate.go`,
  `internal/artifact/artifact.go`.
