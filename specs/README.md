# Brewlet specifications

This directory is the authoritative source for Brewlet architecture and
compatibility contracts. Implementations and user documentation must follow
[`SPECIFICATION.md`](SPECIFICATION.md); substantial changes should begin as a
reviewable design in [`proposals/`](proposals/) and remain on the
[roadmap](../ROADMAP.md) until they ship.

## Versioning and citations

The specification is a living, versioned contract. Backward-incompatible
changes require an explicit version transition, while compatible clarifications
and additions may land incrementally. Brewlet components and documentation cite
requirements by section using the existing `§N` convention (for example, `§4.2`).

Public reference contracts:

- [Capability labels](CAPABILITY_LABELS.md) — stable node scheduling labels,
  admission affinity semantics, and autoscaler recipes.

## Implementations

- [core runtime](..) — CLI,
  containerd shim, and node provisioner
- [Kubernetes platform](../kubernetes) — Kubernetes
  operator, admission webhooks, CRDs, manifests, and Helm chart
- [Maven plugin](../maven-plugin) — Maven build
  and publishing integration

Changes here describe the contract; implementation-specific behavior belongs in
the directory that owns that component.

The specification documents only behavior available in Brewlet releases.
Proposed functionality belongs in the [roadmap](../ROADMAP.md), with detailed
engineering designs retained under [`proposals/`](proposals/).

## Supporting project areas

- [user and operator documentation](../docs/)
- [integration tests](../integration-tests) — cross-component validation and
  fixture applications

A specification change that affects multiple implementations should update the
relevant components and integration tests in the same pull request where practical.
