#!/usr/bin/env bash
set -euo pipefail

E2E_DIR="$(cd "$(dirname "$0")" && pwd)"
exec "$E2E_DIR/run.sh" --tier 2
