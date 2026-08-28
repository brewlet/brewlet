#!/usr/bin/env bash
# Tier 2 — local developer experience: the CLI + node-resident JVM path.
# Covers: push (OCI artifact, no Dockerfile), inspect, run (java -jar + live curl),
# bundle (resource->JVM/cgroup mapping in config.json), layered classpath, and
# modular (JPMS) apps (entry.mode=module + module layer -> java -p ... -m ...).
# Prereqs: go, java

tier2_cli() {
  section "Tier 2 — local CLI + JVM (push / inspect / run / bundle)"
  if ! have go; then skip "tier2: CLI+JVM" "go not installed"; return 0; fi
  if ! have java; then skip "tier2: CLI+JVM" "java not installed"; return 0; fi

  local jh
  jh="$(resolve_java_home)"
  export JAVA_HOME="$jh"
  export PATH="$JAVA_HOME/bin:$PATH"
  info "JAVA_HOME=$JAVA_HOME"

  local store="$WORK/oci" bin="$WORK/bin/brewlet" ref="demo/hello:1.0.0"
  rm -rf "$store"
  mkdir -p "$WORK/bin"

  # --- build the CLI + shim -------------------------------------------------
  if ( cd "$BREWLET_CORE_DIR" && go build -o "$WORK/bin/brewlet" ./cmd/brewlet \
       && go build -o "$WORK/bin/containerd-shim-brewlet-v2" ./shim/cmd/containerd-shim-brewlet-v2 ) \
       >"$WORK/t2-build.log" 2>&1; then
    pass "build brewlet CLI + containerd shim"
  else
    fail "build brewlet CLI + containerd shim" "see $WORK/t2-build.log"; return 0
  fi

  # --- build the demo app.jar ----------------------------------------------
  if "$FIXTURES_DIR/demo-app/build.sh" >"$WORK/t2-app.log" 2>&1 \
       && [[ -f "$FIXTURES_DIR/demo-app/target/app.jar" ]]; then
    pass "build demo app.jar (JDK only, no Maven/Gradle)"
  else
    fail "build demo app.jar" "see $WORK/t2-app.log"; return 0
  fi
  local jar="$FIXTURES_DIR/demo-app/target/app.jar"

  # --- push: ship ONLY the JAR as an OCI artifact --------------------------
  local out
  if out="$("$bin" push "$jar" "$ref" --store "$store" --format=artifact 2>&1)"; then
    assert_contains "push: reports pushed artifact" "$out" "pushed $ref"
    assert_contains "push: advertises OCI artifactType" "$out" "artifactType:"
    assert_contains "push: ships only the JAR (no Dockerfile)" "$out" "no Dockerfile"
  else
    fail "push JAR as OCI artifact" "$(printf '%s' "$out" | tail -1)"
  fi
  assert_file "push: OCI layout index.json written" "$store/index.json"
  [[ -d "$store/blobs/sha256" ]] && pass "push: content-addressed blobs written" \
    || fail "push: content-addressed blobs written" "no $store/blobs/sha256"

  # --- inspect: manifest + JVM launch config -------------------------------
  if out="$("$bin" inspect "$ref" --store "$store" 2>&1)"; then
    assert_contains "inspect: shows manifest section" "$out" "== manifest =="
    assert_contains "inspect: shows jvm config section" "$out" "== jvm config =="
    assert_contains "inspect: records main jar" "$out" "app.jar"
    # The artifact is deployment-agnostic: JDK feature/distribution and launcher
    # live in the deployment descriptor, never in jvm-config.json.
    assert_not_contains "inspect: artifact carries no JDK feature" "$out" "\"feature\""
    assert_not_contains "inspect: artifact carries no launcher" "$out" "\"launcher\""
    assert_not_contains "inspect: artifact carries no ports" "$out" "\"ports\""
  else
    fail "inspect artifact" "$(printf '%s' "$out" | tail -1)"
  fi

  # --- run: node JVM executes the artifact straight from the JAR -----------
  # The listen port is a framework concern, not a JVM or artifact concern: the
  # demo app reads -Dserver.port, so the bind port is passed as an extra JVM arg.
  # Brewlet injects nothing here.
  local port=8080 body
  "$bin" run "$ref" --store "$store" -- -Dserver.port=$port >"$WORK/t2-run.log" 2>&1 &
  local run_pid=$!
  if body="$(retry_curl "http://localhost:$port/healthz" 40 0.5)"; then
    pass "run: JVM launched from artifact and answers /healthz"
    body="$(curl -s "http://localhost:$port/hello" 2>/dev/null)"
    assert_contains "run: /hello served by the live JVM" "$body" "Hello"
    body="$(curl -s "http://localhost:$port/info" 2>/dev/null)"
    assert_contains "run: /info reports JVM runtime details" "$body" "java"
  else
    fail "run: JVM answers /healthz" "see $WORK/t2-run.log"
  fi
  kill "$run_pid" 2>/dev/null || true
  wait "$run_pid" 2>/dev/null || true

  # --- bundle: OCI runc bundle + resource->JVM/cgroup mapping --------------
  local bdir="$WORK/bundle"
  rm -rf "$bdir"
  if "$bin" bundle "$ref" --store "$store" --cpu 2 --memory 512Mi --out "$bdir" \
       >"$WORK/t2-bundle.log" 2>&1; then
    pass "bundle: emit OCI runtime bundle for the shim/runc path"
  else
    fail "bundle: emit OCI runtime bundle" "see $WORK/t2-bundle.log"
  fi
  assert_file "bundle: config.json written" "$bdir/config.json"
  if [[ -f "$bdir/config.json" ]]; then
    local cfg; cfg="$(cat "$bdir/config.json")"
    # 512Mi -> 536870912 bytes; 2 cpus -> quota 200000 / period 100000.
    assert_contains "bundle: memory limit maps to cgroup bytes (512Mi)" "$cfg" "536870912"
    assert_contains "bundle: cpu limit maps to cgroup quota (2 cores)" "$cfg" "200000"
    assert_contains "bundle: launches java -jar in the sandbox" "$cfg" "/app/app.jar"
    assert_contains "bundle: mounts node JDK read-only at /opt/jdk" "$cfg" "/opt/jdk"
    if have python3 && python3 -c "import json,sys; json.load(open('$bdir/config.json'))" 2>/dev/null; then
      pass "bundle: config.json is valid OCI runtime JSON"
    else
      fail "bundle: config.json is valid JSON"
    fi
  fi

  # --- bundle: --launcher overrides argv[0] (local launcher selection) ------
  # The launcher lives only in the deployment descriptor / CLI, never the
  # artifact. `--launcher jaz` must make the launcher the process entrypoint
  # (argv[0]) in place of vanilla `java`, independent of any launcher layer.
  local ldir="$WORK/bundle-launcher"
  rm -rf "$ldir"
  if "$bin" bundle "$ref" --store "$store" --launcher jaz --out "$ldir" \
       >"$WORK/t2-bundle-launcher.log" 2>&1 && [[ -f "$ldir/config.json" ]]; then
    if have python3; then
      local argv0
      argv0="$(python3 -c "import json; print(json.load(open('$ldir/config.json'))['process']['args'][0])" 2>/dev/null)"
      assert_eq "bundle: --launcher sets argv[0] to the requested launcher" "$argv0" "jaz"
    else
      assert_contains "bundle: --launcher sets argv[0] to the requested launcher" \
        "$(cat "$ldir/config.json")" '"jaz"'
    fi
  else
    fail "bundle: --launcher jaz" "see $WORK/t2-bundle-launcher.log"
  fi


  local libtar="$WORK/dep-layer.tar" ref2="demo/hello-layered:1.0.0"
  ( cd "$WORK" && mkdir -p _lib && echo dummy > _lib/dep.jar && tar -cf "$libtar" -C _lib . )
  if out="$("$bin" push "$jar" "$ref2" --store "$store" --classpath-layer "$libtar" --format=artifact 2>&1)"; then
    assert_contains "classpath: push attaches a dependency layer" "$out" "classpath layers: 1"
  else
    fail "classpath: push with --classpath-layer" "$(printf '%s' "$out" | tail -1)"
  fi

  # --- managed dependencies: two apps reuse one approved OCI layer ----------
  local managed_dir="$WORK/managed-dependencies"
  local managed_tar="$managed_dir/dependencies.tar"
  local managed_lock="$managed_dir/dependency-lock.json"
  local managed_ref="platform/approved:2026.08"
  local managed_app1="demo/managed-one:1.0.0"
  local managed_app2="demo/managed-two:1.0.0"
  if "$FIXTURES_DIR/managed-dependency-app/build.sh" \
      >"$WORK/t2-managed-build.log" 2>&1; then
    pass "managed: build thin app and real dependency JAR"
  else
    fail "managed: build thin app and real dependency JAR" \
      "see $WORK/t2-managed-build.log"
    return 0
  fi
  mkdir -p "$managed_dir/lib"
  cp "$FIXTURES_DIR/managed-dependency-app/target/approved.jar" \
    "$managed_dir/lib/approved.jar"
  COPYFILE_DISABLE=1 tar -cf "$managed_tar" -C "$managed_dir/lib" approved.jar
  local dependency_sha
  if have sha256sum; then
    dependency_sha="$(sha256sum "$managed_dir/lib/approved.jar" | awk '{print $1}')"
  else
    dependency_sha="$(shasum -a 256 "$managed_dir/lib/approved.jar" | awk '{print $1}')"
  fi
  printf '{"schemaVersion":1,"artifacts":[{"groupId":"com.example.platform","artifactId":"approved","version":"2026.08","type":"jar","scope":"runtime","fileName":"approved.jar","sha256":"%s"}]}\n' \
    "$dependency_sha" >"$managed_lock"

  if out="$("$bin" dependency-bundle "$managed_tar" "$managed_ref" \
      --store "$store" \
      --name approved \
      --version 2026.08 \
      --source-bom com.example.platform:approved-spring-boot-bom:2026.08 \
      --lock "$managed_lock" \
      --compatible-jdks 21,25 2>&1)"; then
    assert_contains "managed: publish approved dependency bundle" "$out" "pushed managed dependency bundle"
  else
    fail "managed: publish approved dependency bundle" "$(printf '%s' "$out" | tail -1)"
  fi

  local managed_jar="$FIXTURES_DIR/managed-dependency-app/target/app.jar"
  if "$bin" push "$managed_jar" "$managed_app1" --store "$store" \
      --dependency-bundle "$managed_ref" --dependency-lock "$managed_lock" \
      --main-class com.example.ManagedApp \
      >"$WORK/t2-managed-one.log" 2>&1 \
      && "$bin" push "$managed_jar" "$managed_app2" --store "$store" \
      --dependency-bundle "$managed_ref" --dependency-lock "$managed_lock" \
      --main-class com.example.ManagedApp \
      >"$WORK/t2-managed-two.log" 2>&1; then
    pass "managed: compose two applications from one bundle"
  else
    fail "managed: compose two applications from one bundle" "see $WORK/t2-managed-*.log"
  fi

  local managed_out1 managed_out2 layer1 layer2
  managed_out1="$("$bin" inspect "$managed_app1" --store "$store" 2>&1)"
  managed_out2="$("$bin" inspect "$managed_app2" --store "$store" 2>&1)"
  assert_contains "managed: inspect records approved source BOM" "$managed_out1" \
    '"sourceBom": "com.example.platform:approved-spring-boot-bom:2026.08"'
  layer1="$(printf '%s' "$managed_out1" | sed -n 's/.*"dependencyLayerDigest": "\(sha256:[0-9a-f]*\)".*/\1/p' | head -1)"
  layer2="$(printf '%s' "$managed_out2" | sed -n 's/.*"dependencyLayerDigest": "\(sha256:[0-9a-f]*\)".*/\1/p' | head -1)"
  if [[ -n "$layer1" ]]; then
    assert_eq "managed: applications reuse exact dependency layer digest" "$layer2" "$layer1"
  else
    fail "managed: applications reuse exact dependency layer digest" "evidence did not expose a layer digest"
  fi
  if out="$("$bin" run "$managed_app1" --store "$store" 2>&1)"; then
    assert_contains "managed: JVM loads class from approved dependency layer" \
      "$out" "MANAGED DEPENDENCY OK"
  else
    fail "managed: JVM loads class from approved dependency layer" \
      "$(printf '%s' "$out" | tail -1)"
  fi

  # --- modular (JPMS) app: entry.mode=module + module layer -----------------
  # Build a two-module app (main module com.example.orders requires the library
  # module com.example.greeter, shipped in a module layer) and drive it through
  # the same push/inspect/run/bundle path to prove `java -p ... -m ...`.
  if "$FIXTURES_DIR/demo-module-app/build.sh" >"$WORK/t2-module-app.log" 2>&1 \
       && [[ -f "$FIXTURES_DIR/demo-module-app/target/orders.jar" ]] \
       && [[ -f "$FIXTURES_DIR/demo-module-app/target/mods.tar" ]]; then
    pass "module: build modular demo (orders.jar + mods.tar, JDK only)"
  else
    fail "module: build modular demo app" "see $WORK/t2-module-app.log"; return 0
  fi
  local mjar="$FIXTURES_DIR/demo-module-app/target/orders.jar"
  local mtar="$FIXTURES_DIR/demo-module-app/target/mods.tar"
  local mref="demo/orders:1.0.0"
  # Use a distinct port: `brewlet run` execs the JVM as a child, so killing the
  # run process orphans the JVM briefly; a separate port keeps the modular run
  # from colliding with the jar run above.
  local mport=8090

  # push: auto-detect the modular JAR and attach the library-module layer.
  if out="$("$bin" push "$mjar" "$mref" --store "$store" --module-layer "$mtar" --format=artifact 2>&1)"; then
    assert_contains "module: push auto-detects entry.mode=module" "$out" "entry.mode: module (module=com.example.orders)"
    assert_contains "module: push attaches a module layer" "$out" "modulepath layers: 1"
  else
    fail "module: push modular JAR" "$(printf '%s' "$out" | tail -1)"
  fi

  # inspect: the launch config records module mode + module path.
  if out="$("$bin" inspect "$mref" --store "$store" 2>&1)"; then
    assert_contains "module: inspect records mode=module" "$out" "\"mode\": \"module\""
    assert_contains "module: inspect records the module name" "$out" "\"module\": \"com.example.orders\""
    assert_contains "module: inspect records the /app/mods module path" "$out" "\"mods\""
  else
    fail "module: inspect modular artifact" "$(printf '%s' "$out" | tail -1)"
  fi

  # run: the node JVM launches on the module path and the cross-module call works.
  "$bin" run "$mref" --store "$store" -- -Dserver.port=$mport >"$WORK/t2-module-run.log" 2>&1 &
  local mrun_pid=$!
  if body="$(retry_curl "http://localhost:$mport/healthz" 40 0.5)"; then
    assert_contains "module: launched via java -p ... -m ..." "$(cat "$WORK/t2-module-run.log")" "-m com.example.orders/com.example.orders.OrdersApp"
    body="$(curl -s "http://localhost:$mport/hello" 2>/dev/null)"
    assert_contains "module: /hello served by the library module on the module path" "$body" "MODULAR"
    body="$(curl -s "http://localhost:$mport/info" 2>/dev/null)"
    assert_contains "module: /info confirms the library module resolved" "$body" "greeter.module     = com.example.greeter"
  else
    fail "module: modular JVM answers /healthz" "see $WORK/t2-module-run.log"
  fi
  kill "$mrun_pid" 2>/dev/null || true
  wait "$mrun_pid" 2>/dev/null || true

  # bundle: the runc config.json launches on the module path and mounts /app/mods.
  local mbdir="$WORK/bundle-module"
  rm -rf "$mbdir"
  if "$bin" bundle "$mref" --store "$store" --cpu 2 --memory 512Mi --out "$mbdir" \
       >"$WORK/t2-module-bundle.log" 2>&1; then
    pass "module: emit OCI runtime bundle for the modular app"
  else
    fail "module: emit OCI runtime bundle" "see $WORK/t2-module-bundle.log"
  fi
  if [[ -f "$mbdir/config.json" ]]; then
    local mcfg; mcfg="$(cat "$mbdir/config.json")"
    assert_contains "module: bundle launches java on the module path" "$mcfg" "/app/orders.jar:/app/mods"
    assert_contains "module: bundle targets module/mainClass" "$mcfg" "com.example.orders/com.example.orders.OrdersApp"
    assert_contains "module: bundle mounts the module layer at /app/mods" "$mcfg" "\"/app/mods\""
  fi

}
