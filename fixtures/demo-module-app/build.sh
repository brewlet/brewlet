#!/usr/bin/env bash
# Builds the modular (JPMS) demo app using only the JDK (no Maven/Gradle).
#
# Produces two artifacts that exercise Brewlet's `entry.mode: module` path:
#   target/orders.jar  - the MAIN modular JAR (module com.example.orders), packaged
#                        with `jar --main-class ...` so it carries a ModuleMainClass
#                        attribute. Brewlet auto-detects this as a modular app.
#   target/mods.tar    - a `modulepath.layer.v1+tar` layer holding the library
#                        module com.example.greeter (greeter.jar), unpacked on the
#                        node to /app/mods and fed to `--module-path`.
#
# On the node Brewlet launches:
#   java -p /app/orders.jar:/app/mods -m com.example.orders/com.example.orders.OrdersApp
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
out="$here/target"
rm -rf "$out"
mkdir -p "$out/classes" "$out/mods"

echo "[build] compiling both modules (module source path)..."
javac --release 21 \
    --module-source-path "$here/src" \
    -d "$out/classes" \
    --module com.example.greeter,com.example.orders

echo "[build] packaging the library module -> mods/greeter.jar..."
jar --create --file "$out/mods/greeter.jar" \
    -C "$out/classes/com.example.greeter" .

echo "[build] packaging the main modular jar (ModuleMainClass: com.example.orders.OrdersApp)..."
jar --create --file "$out/orders.jar" \
    --main-class com.example.orders.OrdersApp \
    -C "$out/classes/com.example.orders" .

echo "[build] packing the module layer -> mods.tar (contains greeter.jar)..."
# COPYFILE_DISABLE avoids macOS BSD tar embedding AppleDouble (._*) sidecar
# entries, which would otherwise land in /app/mods and break module resolution.
COPYFILE_DISABLE=1 tar -cf "$out/mods.tar" -C "$out/mods" .

echo "[build] compiling the legacy (non-modular) class-path helper -> lib/legacy.jar..."
# The mixed class-path + module-path scenario (docs §8.1): a plain, non-modular
# helper that ships on the supplementary class path (/app/lib) alongside the
# module path. It lives in the unnamed module, so it is compiled WITHOUT the
# module source path.
mkdir -p "$out/legacy-classes" "$out/lib"
javac --release 21 \
    -d "$out/legacy-classes" \
    "$here/legacy-src/com/example/legacy/Legacy.java"
jar --create --file "$out/lib/legacy.jar" -C "$out/legacy-classes" .

echo "[build] packing the class-path layer -> legacy.tar (contains legacy.jar)..."
COPYFILE_DISABLE=1 tar -cf "$out/legacy.tar" -C "$out/lib" .

echo "[build] done:"
echo "  main jar    -> $out/orders.jar"
echo "  module layer-> $out/mods.tar"
echo "  classpath layer-> $out/legacy.tar (mixed-form demo)"
jar --describe-module --file "$out/orders.jar" 2>/dev/null || true
