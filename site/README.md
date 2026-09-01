# Brewlet site and documentation

[![Deploy website](https://github.com/brewlet/brewlet/actions/workflows/site-pages.yml/badge.svg)](https://github.com/brewlet/brewlet/actions/workflows/site-pages.yml)
[![License: MIT](https://img.shields.io/github/license/brewlet/brewlet)](./LICENSE)

This directory contains the [brewlet.sh](https://brewlet.sh) static website and
branding assets. User-facing documentation and workshop material live in the
repository-level [`docs/`](../docs/) directory.

## Contents

| Path | Purpose |
|---|---|
| `index.html`, `assets/` | Static landing page and its visual assets |
| `../docs/` | User and operator documentation |
| `../docs/workshops/` | Role-based workshop material for operators and developers |
| `assets/images/` | Brand assets and architecture diagrams |
| `CNAME` | GitHub Pages custom domain |

## Local preview

Landing page:

```bash
python3 -m http.server 8099 --directory site
```

Then open <http://localhost:8099>.

Documentation site (`/docs/` with left navigation):

```bash
python3 -m pip install mkdocs mkdocs-material
python3 -m mkdocs serve -f site/mkdocs.yml
```

Then open <http://localhost:8000/docs/>.

## Deployment

Pushes to `main` that change the web assets, documentation, or Pages
workflow build and deploy the static landing page plus a rendered MkDocs site at
`/docs/`.
