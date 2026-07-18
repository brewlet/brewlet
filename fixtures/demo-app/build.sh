#!/usr/bin/env bash
# Builds the demo self-executable JAR using only the JDK (no Maven/Gradle).
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
out="$here/target"
rm -rf "$out"
mkdir -p "$out/classes"

echo "[build] compiling..."
javac --release 21 -d "$out/classes" "$here/src/com/example/Hello.java"

echo "[build] packaging executable jar (Main-Class: com.example.Hello)..."
jar --create --file "$out/app.jar" \
    --main-class com.example.Hello \
    -C "$out/classes" .

echo "[build] done -> $out/app.jar"
jar --describe-module --file "$out/app.jar" 2>/dev/null || true
unzip -p "$out/app.jar" META-INF/MANIFEST.MF
