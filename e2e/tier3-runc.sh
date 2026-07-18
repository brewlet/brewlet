#!/usr/bin/env bash
# Tier 3 — real Linux mechanism: the shim disassembles the artifact into an OCI
# bundle and runc runs the JVM as PID 1 under real cgroup limits with a
# node-resident JDK. This is exactly what the containerd shim does on a node.
# Exercised for both a `java -jar` app and a modular (JPMS) `java -p ... -m ...`
# app whose library module is delivered in a module layer mounted at /app/mods.
# Prereq: docker (the e2e runs inside a privileged eclipse-temurin container).

tier3_runc() {
  section "Tier 3 — shim -> runc -> java under cgroups"
  if ! have docker; then skip "tier3: runc/Linux e2e" "docker not installed"; return 0; fi
  if ! docker info >/dev/null 2>&1; then skip "tier3: runc/Linux e2e" "docker daemon not reachable"; return 0; fi
  if ! have go; then skip "tier3: runc/Linux e2e" "go not installed"; return 0; fi

  local jh arch
  jh="$(resolve_java_home)"
  case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; *) arch=amd64 ;; esac

  local out
  mkdir -p "$WORK/bin"
  if ! "$FIXTURES_DIR/demo-app/build.sh" >"$WORK/t3-build.log" 2>&1 \
     || ! "$FIXTURES_DIR/demo-module-app/build.sh" >>"$WORK/t3-build.log" 2>&1 \
     || ! (cd "$BREWLET_CORE_DIR" && GOOS=linux GOARCH="$arch" go build -o "$WORK/bin/shim-linux" ./shim/cmd/containerd-shim-brewlet-v2 \
       && go build -o "$WORK/bin/brewlet" ./cmd/brewlet) >>"$WORK/t3-build.log" 2>&1; then
    fail "runc: build fixtures and Brewlet binaries" "see $WORK/t3-build.log"
    return 0
  fi
  if ! "$WORK/bin/brewlet" push "$FIXTURES_DIR/demo-app/target/app.jar" demo/hello:1.0.0 \
      --store "$WORK/oci" --format=artifact >>"$WORK/t3-build.log" 2>&1 \
    || ! "$WORK/bin/brewlet" push "$FIXTURES_DIR/demo-module-app/target/orders.jar" demo/orders:1.0.0 \
      --store "$WORK/oci" --module-layer "$FIXTURES_DIR/demo-module-app/target/mods.tar" \
      --format=artifact >>"$WORK/t3-build.log" 2>&1 \
    || ! "$WORK/bin/brewlet" push "$FIXTURES_DIR/demo-module-app/target/orders.jar" demo/orders-mixed:1.0.0 \
      --store "$WORK/oci" --module-layer "$FIXTURES_DIR/demo-module-app/target/mods.tar" \
      --classpath-layer "$FIXTURES_DIR/demo-module-app/target/legacy.tar" \
      --format=artifact >>"$WORK/t3-build.log" 2>&1; then
    fail "runc: prepare test artifacts" "see $WORK/t3-build.log"
    return 0
  fi

  info "running the Linux runc harness (pulls eclipse-temurin:21; ~1-3 min)"
  if out="$(docker run --rm -i --privileged --platform "linux/$arch" --cgroupns=private \
      --memory=384m --cpus=1 -v "$WORK:/work" \
      eclipse-temurin:21 bash -s <"$E2E_DIR/e2e-linux.sh" 2>&1)"; then
    printf '%s\n' "$out" >"$WORK/t3-e2e-linux.log"
    assert_contains "runc: shim built the OCI bundle on the node" "$out" "OCI runtime bundle"
    assert_contains "runc: JVM answered /hello under runc" "$out" "Hello"
    assert_contains "runc: JVM saw the sandbox cgroup limits (/info)" "$out" "cgroup"
    assert_contains "runc: end-to-end run completed" "$out" "== done =="
    # modular (JPMS) scenario: java -p <module-path> -m <module> under runc.
    assert_contains "runc: modular JVM answered /hello under runc (module path resolved)" "$out" "MODULAR"
    assert_contains "runc: modular JVM resolved the library module" "$out" "greeter.module     = com.example.greeter"
    assert_contains "runc: modular end-to-end run completed" "$out" "== modular done =="
  else
    printf '%s\n' "$out" >"$WORK/t3-e2e-linux.log"
    fail "runc: Linux harness" "see $WORK/t3-e2e-linux.log"
  fi

  if out="$(docker run --rm -i --privileged --platform "linux/$arch" --cgroupns=private \
      --memory=384m --cpus=1 -v "$WORK:/work" \
      eclipse-temurin:21 bash -s <"$E2E_DIR/mixed-runc.sh" 2>&1)"; then
    printf '%s\n' "$out" >"$WORK/t3-mixed-runc.log"
    assert_contains "runc: mixed class-path + module-path app served /hello" "$out" "MIXED"
    assert_contains "runc: mixed app resolved the legacy class-path helper" "$out" "legacy"
    assert_contains "runc: mixed end-to-end run completed" "$out" "== mixed done =="
  else
    printf '%s\n' "$out" >"$WORK/t3-mixed-runc.log"
    fail "runc: mixed Linux harness" "see $WORK/t3-mixed-runc.log"
  fi
}
