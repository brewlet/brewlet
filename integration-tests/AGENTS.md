# Brewlet E2E runbook

## Reliable invocation

Run against the monorepo checkout:

```bash
integration-tests/e2e/run.sh --reset
integration-tests/e2e/run.sh
```

The harness does not switch branches or modify component sources. Override
`BREWLET_CORE_DIR` or `BREWLET_KUBERNETES_DIR` only when testing an external
checkout.

## Prerequisites

| Tool | Tiers |
|---|---|
| Go | all |
| JDK 21+ | 2, 3, 7, 8, 9, 12, 14 |
| Docker | 3, 6, 7, 8-12, 14 |
| kubectl and a reachable cluster | 4-14 |
| Helm | 4 (optional), 10 |
| OpenSSL | 5, 6, 11 |

Host-only tiers 1-3 need no cluster. Tiers 4-7 and 13 exercise API-server
behavior. Tiers 6, 8-12, and 14 require local containerd nodes that Docker can
enter, such as kind. Managed clusters skip those node-side paths.

## Cleanup and diagnostics

Run `./e2e/run.sh --reset` before repeating Kubernetes tiers. It removes only
Brewlet-owned labels, annotations, CRDs, runtime classes, webhook configuration,
RBAC, and fixed test namespaces. It does not delete application workloads outside
the harness namespaces.

Generated artifacts and diagnostic logs are written beneath the printed work
directory. Set `E2E_WORK` to retain them at a known path. For rollout failures,
read `diag-*.log` first.

Common environment-specific skips:

- Tier 5 skips when a cluster cannot reach a host-bound webhook; tier 6 covers
  the same assertions in-cluster.
- Tiers 8, 9, 12, and 14 skip if no schedulable local containerd node can be
  provisioned.
- Tier 12 skips when the node's `ctr` lacks `images unpack`.

Specifications belong in `specs/`, and general project documentation belongs in
[brewlet/site](https://github.com/brewlet/site), not this directory.
