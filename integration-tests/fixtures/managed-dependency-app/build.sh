#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")" && pwd)"
target="$root/target"
rm -rf "$target"
mkdir -p "$target/dependency-classes" "$target/app-classes"

javac -d "$target/dependency-classes" \
  "$root/src/com/example/approved/Greeting.java"
jar --create --file "$target/approved.jar" \
  -C "$target/dependency-classes" .

javac -cp "$target/approved.jar" -d "$target/app-classes" \
  "$root/src/com/example/ManagedApp.java"
jar --create --file "$target/app.jar" \
  --main-class com.example.ManagedApp \
  -C "$target/app-classes" .
