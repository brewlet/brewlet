#!/bin/sh
set -eu

repository="brewlet/brewlet"
version="${BREWLET_VERSION:-latest}"
install_dir="${BREWLET_INSTALL_DIR:-${HOME:?HOME is not set}/.local/bin}"

usage() {
  cat <<'EOF'
Install the Brewlet CLI.

Usage:
  install.sh [--version VERSION] [--install-dir DIRECTORY]

Environment:
  BREWLET_VERSION       Release version to install (default: latest)
  BREWLET_INSTALL_DIR   Destination directory (default: $HOME/.local/bin)
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      [ "$#" -ge 2 ] || { echo "missing value for --version" >&2; exit 1; }
      version="$2"
      shift 2
      ;;
    --install-dir)
      [ "$#" -ge 2 ] || { echo "missing value for --install-dir" >&2; exit 1; }
      install_dir="$2"
      shift 2
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      echo "unknown option: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

for command in curl tar awk install; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "required command not found: $command" >&2
    exit 1
  }
done

case "$(uname -s)" in
  Darwin) os="darwin" ;;
  Linux) os="linux" ;;
  *)
    echo "unsupported operating system: $(uname -s)" >&2
    exit 1
    ;;
esac

case "$(uname -m)" in
  arm64|aarch64) arch="arm64" ;;
  x86_64|amd64) arch="amd64" ;;
  *)
    echo "unsupported architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

if [ "$version" = "latest" ]; then
  release_url="$(curl -fsSL -o /dev/null -w '%{url_effective}' \
    "https://github.com/${repository}/releases/latest")"
  version="${release_url##*/}"
fi
version="${version#v}"
[ -n "$version" ] || { echo "could not resolve a Brewlet release" >&2; exit 1; }

archive="brewlet_${version}_${os}_${arch}.tar.gz"
base="https://github.com/${repository}/releases/download/v${version}"
work="$(mktemp -d "${TMPDIR:-/tmp}/brewlet-install.XXXXXX")"
trap 'rm -rf "$work"' 0
trap 'exit 1' 1 2 15

echo "Downloading Brewlet v${version} for ${os}/${arch}..."
curl -fsSL -o "$work/$archive" "$base/$archive"
curl -fsSL -o "$work/checksums.txt" "$base/checksums.txt"

expected="$(awk -v archive="$archive" '$2 == archive { print $1 }' "$work/checksums.txt")"
[ -n "$expected" ] || {
  echo "checksum for $archive was not found in the release" >&2
  exit 1
}

if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$work/$archive" | awk '{ print $1 }')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "$work/$archive" | awk '{ print $1 }')"
else
  echo "sha256sum or shasum is required to verify the download" >&2
  exit 1
fi

[ "$actual" = "$expected" ] || {
  echo "checksum verification failed for $archive" >&2
  exit 1
}

tar -xzf "$work/$archive" -C "$work"
[ -f "$work/brewlet" ] || {
  echo "release archive does not contain the brewlet binary" >&2
  exit 1
}

mkdir -p "$install_dir"
install -m 0755 "$work/brewlet" "$install_dir/brewlet"

echo "Installed Brewlet v${version} to $install_dir/brewlet"
case ":${PATH:-}:" in
  *":$install_dir:"*) ;;
  *) echo "Add $install_dir to PATH: export PATH=\"$install_dir:\$PATH\"" ;;
esac
