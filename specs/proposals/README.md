# Brewlet enhancement proposals

KEP-style design proposals for changes that are large enough to warrant a written,
reviewable design before implementation. Each proposal targets one or more
[SPECIFICATION](../SPECIFICATION.md) sections (the `§N` convention) and is absorbed
into the spec once implemented.

| # | Title | Status | Targets |
|---|-------|--------|---------|
| [0001](0001-node-profiles.md) | Node profiles: per-pool cluster preparation | Implemented | §5.6 (new), §8.1, §8.3, §14 |
| [0002](0002-validated-node-reconfig.md) | Validated containerd reconfiguration & readiness smoke gate | Partially implemented | §5.2, §14 |
| [0003](0003-capability-label-taxonomy.md) | Stable capability-label taxonomy for autoscaling | Draft | §5.2, §8.3 |
| [0004](0004-cert-manager-admission.md) | cert-manager integration for the admission webhook | Draft | §8.3 |
| [0005](0005-replica-coalescing.md) | Replica coalescing: fewer, larger JVMs | Draft | §8.2, §9, §10 |

> Proposals 0002–0004 were split out of the original 0001 draft so each is
> independently reviewable and shippable; 0001 references them where it depends on them.
