# Brewlet specifications

This directory is the authoritative source for Brewlet architecture and
compatibility contracts. Implementations and user documentation must follow
[`SPECIFICATION.md`](SPECIFICATION.md); substantial changes should begin as a
reviewable design in [`proposals/`](proposals/).

## Versioning and citations

The specification is a living, versioned contract. Backward-incompatible
changes require an explicit version transition, while compatible clarifications
and additions may land incrementally. Brewlet components and documentation cite
requirements by section using the existing `§N` convention (for example, `§4.2`).

## Implementations

- [core runtime](..) — CLI,
  containerd shim, and node provisioner
- [Kubernetes platform](../kubernetes) — Kubernetes
  operator, admission webhooks, CRDs, manifests, and Helm chart
- [Maven plugin](../maven-plugin) — Maven build
  and publishing integration

Changes here describe the contract; implementation-specific behavior belongs in
the directory that owns that component.

## Supporting project areas

- [brewlet/site](https://github.com/brewlet/site) — user and operator documentation
- [integration tests](../integration-tests) — cross-component validation and
  fixture applications

A specification change that affects multiple implementations should update the
relevant components and integration tests in the same pull request where practical.
