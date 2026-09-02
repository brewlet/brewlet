# Brewlet

[![CI](https://github.com/brewlet/brewlet/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/brewlet/brewlet/actions/workflows/ci.yml)
[![E2E](https://github.com/brewlet/brewlet/actions/workflows/e2e.yml/badge.svg?branch=main)](https://github.com/brewlet/brewlet/actions/workflows/e2e.yml)
[![Release](https://img.shields.io/github/v/release/brewlet/brewlet?sort=semver)](https://github.com/brewlet/brewlet/releases/latest)
[![Go version](https://img.shields.io/github/go-mod/go-version/brewlet/brewlet?filename=core%2Fgo.mod)](core/go.mod)
[![License](https://img.shields.io/github/license/brewlet/brewlet)](LICENSE)

**Run Java applications from OCI artifacts using JDKs managed once on each
Kubernetes node.**

Brewlet separates the application from the Java runtime. Developers publish
their JAR and launch metadata as a standard OCI artifact, while platform teams
control the JDK distributions and launchers installed on each node. Kubernetes
schedules the workload onto a compatible node and containerd starts it through
the Brewlet runtime shim.

At fleet scale, this model is intended to lower the cost per Java workload,
accelerate runtime security remediation, and reduce operational boilerplate for
both developers and AI-assisted delivery workflows. It also gives platform teams
a central control point for JDK policy, runtime optimization, and upgrades.

This repository contains the complete Brewlet implementation: the CLI and
runtime, Kubernetes platform, node provisioner, Maven plugin, specifications,
integration tests, website, and user-facing documentation.

- [Documentation](https://brewlet.sh/)
- [Getting started](https://brewlet.sh/docs/getting-started/)
- [Ops workshop](https://brewlet.sh/docs/workshops/operations/)
- [Developer workshop](https://brewlet.sh/docs/workshops/developers/)
- [Latest release](https://github.com/brewlet/brewlet/releases/latest)
- [Specification](specs/SPECIFICATION.md)
- [Roadmap](ROADMAP.md)

## How it works

```text
Developer                                  Kubernetes cluster

JAR + launch metadata                      NodeProfile
        |                                      |
        v                                      v
Maven plugin or Brewlet CLI  --->  OCI registry  --->  provisioned node
                                                        |
                                      RuntimeClass: brewlet
                                                        |
                                      containerd shim + node JDK
                                                        |
                                                        v
                                                Java application
```

1. The platform operator installs Brewlet and defines the supported JDK and
   launcher inventory through one or more `NodeProfile` resources.
2. The node provisioner installs the containerd shim and approved runtimes on
   matching nodes, then advertises their capabilities through node metadata.
3. The developer packages a Java application with the CLI or Maven plugin and
   publishes the resulting OCI artifact.
4. The admission webhook validates the runtime requirements and directs the pod
   to a compatible node.
5. The Brewlet shim assembles an OCI bundle and launches the application with
   the selected node-resident JDK.

## Subprojects

| Subproject | Path | Responsibility |
| --- | --- | --- |
| Core runtime | [`core/`](core/) | Contains the Brewlet CLI, artifact tooling, shared runtime packages, and containerd Runtime v2 shim. |
| Node provisioner | [`provisioner/`](provisioner/) | Installs and removes the shim, JDK roots, launchers, and containerd configuration on Linux nodes. |
| Kubernetes platform | [`kubernetes/`](kubernetes/) | Provides the operator, admission webhook, `NodeProfile` and `JavaApplication` APIs, RBAC, manifests, and Helm chart. |
| Maven plugin | [`maven-plugin/`](maven-plugin/) | Builds and publishes Brewlet OCI artifacts and generates Kubernetes workload manifests directly from Maven projects. |
| Specifications | [`specs/`](specs/) | Defines the architecture, artifact formats, runtime contracts, APIs, and compatibility rules. Future work is tracked in the [roadmap](ROADMAP.md). |
| Integration tests | [`integration-tests/`](integration-tests/) | Exercises the CLI, JVM execution, shim, Kubernetes control plane, node provisioning, Helm installation, and representative workloads. |
| Website | [`site/`](site/) | Contains the static landing page, installer, branding assets, and MkDocs configuration published at [brewlet.sh](https://brewlet.sh/). |
| User documentation | [`docs/`](docs/) | Contains installation, configuration, operations, workload deployment, troubleshooting, and workshop guidance. |

## Quick start

### Install the CLI

The checksum-verifying installer selects the correct Linux or macOS archive for
the host. It installs `brewlet` to `$HOME/.local/bin` by default.

```bash
curl -fsSL https://brewlet.sh/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
brewlet version
```

Set `BREWLET_VERSION` to pin a release or `BREWLET_INSTALL_DIR` to choose a
different destination. See the
[CLI documentation](https://brewlet.sh/docs/cli-reference/) for the artifact
workflow.

### Enable a Kubernetes cluster

Brewlet requires containerd, cgroup v2, and permission to run a privileged
host-modifying DaemonSet. The default chart profile provisions every node, so
use named `NodeProfile`s to restrict production or shared clusters to
platform-owned node pools.

```bash
helm upgrade --install brewlet oci://ghcr.io/brewlet/charts/brewlet \
  --version 0.1.0 \
  --namespace brewlet \
  --create-namespace \
  --set-string provisioner.jdks=temurin-21

kubectl get nodes -L brewlet.sh/runtime
brewlet doctor --namespace <developer-namespace>
```

Follow the [installation guide](https://brewlet.sh/docs/installation/) for
prerequisites, scoped node profiles, production configuration, upgrades, and
safe removal.

## Build and test

Development requires Go 1.26 or newer. Maven plugin development requires Maven
3.9+ and JDK 17+, while the optional AppCDS integration test requires a full JDK
17 or newer. Docker with Buildx is required for provisioner images.

Run all checks that do not require a Kubernetes cluster:

```bash
make check-all
```

This validates the core runtime, Kubernetes platform, Maven plugin, and
host-only integration tiers. To build the CLI and shim into `bin/`:

```bash
make binaries
```

To run the full tiered integration harness against a suitable local cluster,
see [`integration-tests/AGENTS.md`](integration-tests/AGENTS.md).

## Releases

Tags matching `v*` publish version-aligned artifacts:

| Artifact | Location |
| --- | --- |
| CLI archives and checksums | [GitHub Releases](https://github.com/brewlet/brewlet/releases) |
| Operator image | `ghcr.io/brewlet/operator:<version>` |
| Admission webhook image | `ghcr.io/brewlet/admission:<version>` |
| Node provisioner image | `ghcr.io/brewlet/node-provisioner:<version>` |
| Helm chart | `oci://ghcr.io/brewlet/charts/brewlet` |
| Maven plugin JAR and POM | [GitHub Releases](https://github.com/brewlet/brewlet/releases) |

The Helm chart's `appVersion` selects matching component image tags by default.
Release images support Linux `amd64` and `arm64`; CLI archives support those
architectures on Linux and macOS.

## License

Brewlet is licensed under the [MIT License](LICENSE).
