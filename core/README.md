# Brewlet core

[![CI](https://github.com/brewlet/brewlet/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/brewlet/brewlet/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/brewlet/brewlet?filename=core%2Fgo.mod)](go.mod)
[![Release](https://img.shields.io/github/v/release/brewlet/brewlet?sort=semver)](https://github.com/brewlet/brewlet/releases/latest)
[![License](https://img.shields.io/github/license/brewlet/brewlet)](../LICENSE)

This Go module contains the Brewlet command-line interface, OCI artifact
tooling, shared runtime packages, and containerd Runtime v2 shim.

```text
cmd/brewlet/                         Brewlet CLI
internal/                            Artifact, diagnostics, inventory, and runtime packages
shim/cmd/containerd-shim-brewlet-v2/ containerd runtime shim
```

From the repository root, build and test the module with:

```bash
go -C core build ./...
go -C core test ./...
```

## Managed dependency bundles

Platform teams can publish an approved dependency lock and deterministic
classpath tar as a managed OCI bundle:

```bash
brewlet dependency-bundle dependencies.tar platform/spring-web:2026.08 \
  --name spring-web \
  --version 2026.08 \
  --source-bom com.example.platform:approved-spring-bom:2026.08 \
  --lock dependency-lock.json \
  --compatible-jdks 21,25
```

Applications then compose that bundle with a thin JAR:

```bash
brewlet push target/orders.jar apps/orders:1.4.2 \
  --dependency-bundle platform/spring-web:2026.08 \
  --dependency-lock target/dependency-lock.json \
  --main-class com.example.OrdersApplication
```

The application lock must describe its resolved Maven runtime graph and match
the bundle lock exactly. The bundle owns a standard gzip OCI classpath layer tagged with
`brewlet.sh/layer=classpath`. The application image reuses that exact descriptor
and blob, rather than repacking it. Brewlet rejects nested dependency JARs in the
application JAR and records versioned managed-dependency evidence on the final
image manifest. `brewlet inspect` displays the source BOM, bundle, layer, lock,
and application JAR digests.

The evidence annotation is canonical input for a trusted publisher to sign as
an in-toto/Sigstore attestation. It is not itself a signature; cluster-side
verification remains the responsibility of the supply-chain admission feature.
