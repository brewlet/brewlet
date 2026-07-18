# Brewlet site and documentation

[![Deploy website](https://github.com/brewlet/site/actions/workflows/pages.yml/badge.svg)](https://github.com/brewlet/site/actions/workflows/pages.yml)
[![License: MIT](https://img.shields.io/github/license/brewlet/site)](./LICENSE)

This repository contains the [brewlet.sh](https://brewlet.sh) static website,
user-facing documentation, workshop material, and Brewlet branding assets.

## Contents

| Path | Purpose |
|---|---|
| `index.html`, `styles.css` | Static landing page |
| `docs/` | User and operator documentation |
| `WORKSHOP.md` | Hands-on Brewlet workshop |
| `brewlet-logo.svg`, `brewlet-social.png` | Brand assets |
| `architecture-*.svg` | Architecture diagrams |
| `CNAME` | GitHub Pages custom domain |

## Related repositories

- [brewlet/brewlet](https://github.com/brewlet/brewlet) — CLI, containerd shim, and core runtime
- [brewlet/kubernetes](https://github.com/brewlet/kubernetes) — operator, provisioner, Helm chart, and manifests
- [brewlet/maven-plugin](https://github.com/brewlet/maven-plugin) — Maven publishing plugin
- [brewlet/specs](https://github.com/brewlet/specs) — specification and proposals
- [brewlet/integration-tests](https://github.com/brewlet/integration-tests) — end-to-end integration suite

## Local preview

```bash
python3 -m http.server 8099
```

Then open <http://localhost:8099>.

## Deployment

Pushes to `main` that change the web assets, documentation, workshop, or Pages
workflow deploy the repository root through GitHub Actions.
