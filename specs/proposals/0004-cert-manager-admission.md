# Proposal 0004 — cert-manager integration for the admission webhook

- **Status:** Draft
- **Target spec sections:** amend **§8.3** (webhook serving cert)
- **Related code:** [`brewlet/kubernetes`](https://github.com/brewlet/kubernetes):
  `charts/brewlet` (admission templates + values), `cmd/admission`
- **Split from:** the original [0001 (node profiles)](0001-node-profiles.md) draft. A chart-only
  change, independent of the `NodeProfile` CRD (0001 §7 uses the webhook serving cert this provisions).

> Enhancement proposal (KEP-style), not yet implemented.

---

## 1. Summary

Add an opt-in `admission.certManager.enabled` chart value. When true, the chart wires
the `brewlet-admission` webhook's serving certificate through **cert-manager**
(a `Certificate` resource + a `cert-manager.io/inject-ca-from` annotation on the
`MutatingWebhookConfiguration` / `ValidatingWebhookConfiguration`) instead of the
current Helm `genSignedCert` self-signed cert.

## 2. Motivation

Today the chart mints the webhook's serving cert with Helm's `genSignedCert`. That is
fine for a PoC but has two production problems:

- The cert is **regenerated on every `helm upgrade`** (Helm template functions are not
  stateful), churning the CA the API server trusts.
- No automatic **rotation / expiry management**.

Most production clusters already run cert-manager; the docs already *recommend* it.
This makes it a supported switch rather than a manual post-install swap.

## 3. Design

`values.yaml`:

```yaml
admission:
  enabled: true
  certManager:
    enabled: false            # opt-in; default keeps the self-signed path
    issuerRef:                # required when enabled
      name: brewlet-selfsigned
      kind: Issuer            # Issuer | ClusterIssuer
    # duration / renewBefore optional, passed through to the Certificate
```

When `admission.certManager.enabled: true`:

- Render a cert-manager `Certificate` (and, for the zero-dependency default, an
  optional self-signed `Issuer`) that provisions the webhook serving Secret.
- Annotate the webhook configurations with
  `cert-manager.io/inject-ca-from: <namespace>/<certificate-name>` so the CA bundle is
  injected and rotated by cert-manager's ca-injector.
- **Do not** render the `genSignedCert` Secret in this mode.

When `false` (default): unchanged — the existing self-signed serving cert is used, so
installs with no cert-manager keep working.

## 4. Risks & mitigations

- **cert-manager not installed but enabled.** Mitigate: document the prerequisite;
  the `Certificate` simply won't be fulfilled and the webhook Secret won't appear —
  `failurePolicy: Ignore` keeps workloads unblocked meanwhile.
- **CA injection race on first install.** Mitigate: standard cert-manager
  ca-injector behavior; document ordering.

## Appendix — spec integration points

- **§8.3:** note the webhook serving cert can be provisioned by cert-manager via
  `admission.certManager.enabled`, replacing the Helm-minted self-signed cert.
