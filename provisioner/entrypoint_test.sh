#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/entrypoint.sh"

calls="$(mktemp)"
dest="$(mktemp -d)"
trap 'rm -f "$calls"; chmod -R u+w "$dest" 2>/dev/null || true; rm -rf "$dest"' EXIT

host_ctr() {
  printf '%s\n' "$*" >>"$calls"
}

host_exec() {
  printf '%s\n' "$*" >>"$calls"
}

export JDK_CUSTOM_SOURCE_COUNT=1
export JDK_CUSTOM_SOURCE_0_TOKEN=zulu-21
export JDK_CUSTOM_SOURCE_0_IMAGE=docker.io/library/azul-zulu:21
export JDK_CUSTOM_SOURCE_0_JAVA_HOME=/usr/lib/jvm/zulu21

parse_custom_jdk_sources
jdk_from_image zulu 21 "$dest"

grep -Fq "image pull docker.io/library/azul-zulu:21" "$calls"
grep -Fq "images mount docker.io/library/azul-zulu:21 /opt/brewlet/.image-mount-zulu-21" "$calls"
grep -Fq "cp -a /opt/brewlet/.image-mount-zulu-21/. $dest/" "$calls"
grep -Fxq "/usr/lib/jvm/zulu21" "$dest/.brewlet-java-home"
grep -Fxq "docker.io/library/azul-zulu:21" "$dest/.brewlet-source"
grep -Fxq "/usr/lib/jvm/zulu21" "$dest/.brewlet-source"

if (
  JDK_CUSTOM_SOURCE_COUNT=0
  CUSTOM_JDK_TOKENS=("")
  CUSTOM_JDK_IMAGES=("")
  CUSTOM_JDK_JAVA_HOMES=("")
  parse_custom_jdk_sources
  jdk_from_image unknown 21 "$dest"
) >/dev/null 2>&1; then
  echo "expected an unknown JDK without a custom source to fail" >&2
  exit 1
fi


if (
  JDK_CUSTOM_SOURCE_COUNT=1
  JDK_CUSTOM_SOURCE_0_IMAGE=azul-zulu:21
  parse_custom_jdk_sources
) >/dev/null 2>&1; then
  echo "expected an unqualified custom image reference to fail" >&2
  exit 1
fi

if (
  JDK_CUSTOM_SOURCE_COUNT=1
  JDK_CUSTOM_SOURCE_0_JAVA_HOME=/
  parse_custom_jdk_sources
) >/dev/null 2>&1; then
  echo "expected javaHome=/ to fail" >&2
  exit 1
fi

if (
  JDK_CUSTOM_SOURCE_COUNT=1
  JDK_CUSTOM_SOURCE_0_TOKEN=../../../host-21
  parse_custom_jdk_sources
) >/dev/null 2>&1; then
  echo "expected a path-traversing custom JDK token to fail" >&2
  exit 1
fi
