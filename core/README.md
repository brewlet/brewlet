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
