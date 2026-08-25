# Brewlet site and documentation

[![Deploy website](https://github.com/brewlet/site/actions/workflows/pages.yml/badge.svg)](https://github.com/brewlet/site/actions/workflows/pages.yml)
[![License: MIT](https://img.shields.io/github/license/brewlet/site)](./LICENSE)

This repository contains the [brewlet.sh](https://brewlet.sh) static website,
user-facing documentation, workshop material, and Brewlet branding assets.
It contains no runtime, Kubernetes, plugin, specification, or test implementation;
those components live in the Brewlet monorepo.

## Contents

| Path | Purpose |
|---|---|
| `index.html`, `assets/` | Static landing page and its visual assets |
| `docs/` | User and operator documentation |
| `docs/workshops/` | Role-based workshop material for operators and developers |
| `assets/images/` | Brand assets and architecture diagrams |
| `CNAME` | GitHub Pages custom domain |

## Related components

- [brewlet/brewlet](https://github.com/brewlet/brewlet) — CLI, containerd shim, and core runtime
- [kubernetes/](https://github.com/brewlet/brewlet/tree/main/kubernetes) — operator, provisioner, Helm chart, and manifests
- [maven-plugin/](https://github.com/brewlet/brewlet/tree/main/maven-plugin) — Maven publishing plugin
- [specs/](https://github.com/brewlet/brewlet/tree/main/specs) — specification and proposals
- [integration-tests/](https://github.com/brewlet/brewlet/tree/main/integration-tests) — end-to-end integration suite

## Local preview

Landing page:

```bash
python3 -m http.server 8099
```

Then open <http://localhost:8099>.

Documentation site (`/docs/` with left navigation):

```bash
python3 -m pip install mkdocs mkdocs-material
mkdocs serve
```

Then open <http://localhost:8000/docs/>.

## Deployment

Pushes to `main` that change the web assets, documentation, or Pages
workflow build and deploy the static landing page plus a rendered MkDocs site at
`/docs/`.
