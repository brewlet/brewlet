#!/usr/bin/env bash
# Tier 2 — local developer experience: the CLI + node-resident JVM path.
# Covers: push (OCI artifact, no Dockerfile), inspect, run (java -jar + live curl),
# bundle (resource->JVM/cgroup mapping in config.json), layered classpath, and
# modular (JPMS) apps (entry.mode=module + module layer -> java -p ... -m ...).
# Prereqs: go, java, python3

tier2_cli() {
  section "Tier 2 — local CLI + JVM (push / inspect / run / bundle)"
  if ! have go; then skip "tier2: CLI+JVM" "go not installed"; return 0; fi
  if ! have java; then skip "tier2: CLI+JVM" "java not installed"; return 0; fi
  if ! have python3; then skip "tier2: CLI+JVM" "python3 not installed"; return 0; fi

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
  local managed_private="$managed_dir/signing-key.pem"
  local managed_public="$managed_dir/signing-key.pub.pem"
  if "$FIXTURES_DIR/managed-dependency-app/build.sh" \
      >"$WORK/t2-managed-build.log" 2>&1; then
    pass "managed: build thin app and real dependency JAR"
  else
    fail "managed: build thin app and real dependency JAR" \
      "see $WORK/t2-managed-build.log"
    return 0
  fi
  mkdir -p "$managed_dir/lib"
  if "$bin" keygen --private "$managed_private" --public "$managed_public" \
      >"$WORK/t2-managed-keygen.log" 2>&1; then
    pass "managed: generate signing key pair"
  else
    fail "managed: generate signing key pair" "see $WORK/t2-managed-keygen.log"
    return 0
  fi
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
      --signing-key "$managed_private" \
      --signer-identity e2e-builder \
      --compatible-jdks 21,25 2>&1)"; then
    assert_contains "managed: publish approved dependency bundle" "$out" "pushed managed dependency bundle"
  else
    fail "managed: publish approved dependency bundle" "$(printf '%s' "$out" | tail -1)"
  fi
  if python3 "$E2E_DIR/validate-managed-oci.py" bundle \
        "$store" "$managed_ref" --require-signature \
        >"$WORK/t2-managed-wire.log" 2>&1; then
    pass "managed: bundle satisfies the normative OCI wire contract"
  else
    fail "managed: bundle satisfies the normative OCI wire contract" \
      "see $WORK/t2-managed-wire.log"
  fi
  local tamper_target tampered_store
  for tamper_target in descriptor config lock layer; do
    tampered_store="$WORK/t2-managed-tampered-$tamper_target"
    cp -R "$store" "$tampered_store"
    python3 "$E2E_DIR/tamper-managed-oci.py" \
      "$tampered_store" "$managed_ref" "$tamper_target"
    if "$bin" inspect "$managed_ref" --store "$tampered_store" \
        >"$WORK/t2-managed-tampered-$tamper_target.log" 2>&1; then
      fail "managed: reject tampered $tamper_target" \
        "inspection unexpectedly succeeded"
    else
      pass "managed: reject tampered $tamper_target"
    fi
  done

  local managed_jar="$FIXTURES_DIR/managed-dependency-app/target/app.jar"
  if "$bin" push "$managed_jar" "$managed_app1" --store "$store" \
      --dependency-bundle "$managed_ref" --dependency-lock "$managed_lock" \
      --trusted-public-key "$managed_public" --signing-key "$managed_private" \
      --trusted-signer-identity e2e-builder --builder-identity e2e-builder \
      --main-class com.example.ManagedApp \
      >"$WORK/t2-managed-one.log" 2>&1 \
      && "$bin" push "$managed_jar" "$managed_app2" --store "$store" \
      --dependency-bundle "$managed_ref" --dependency-lock "$managed_lock" \
      --trusted-public-key "$managed_public" \
      --trusted-signer-identity e2e-builder \
      --main-class com.example.ManagedApp \
      >"$WORK/t2-managed-two.log" 2>&1; then
    pass "managed: compose signed and unsigned applications from one bundle"
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
  if out="$("$bin" inspect "$managed_app1" --store "$store" \
      --trusted-public-key "$managed_public" \
      --trusted-signer-identity e2e-builder 2>&1)"; then
    assert_contains "managed: inspect verifies signed final-image attestation" \
      "$out" "managed dependency attestation (signed, verified)"
  else
    fail "managed: inspect verifies signed final-image attestation" \
      "$(printf '%s' "$out" | tail -1)"
  fi
  if "$bin" inspect "$managed_app1" --store "$store" \
      --trusted-public-key "$managed_public" \
      --trusted-signer-identity untrusted-builder \
      >"$WORK/t2-managed-wrong-identity.log" 2>&1; then
    fail "managed: reject incorrect attestation identity" \
      "verification unexpectedly succeeded"
  else
    pass "managed: reject incorrect attestation identity"
  fi
  if python3 "$E2E_DIR/validate-managed-oci.py" image \
      "$store" "$managed_app1" --require-signature \
      >"$WORK/t2-managed-image-wire.log" 2>&1 \
      && python3 "$E2E_DIR/validate-managed-oci.py" image \
      "$store" "$managed_app2" >"$WORK/t2-managed-unsigned-image-wire.log" 2>&1; then
    pass "managed: final image attestation satisfies the normative wire contract"
    pass "managed: unsigned final image satisfies the normative wire contract"
  else
    fail "managed: final image attestation satisfies the normative wire contract" \
      "see $WORK/t2-managed-image-wire.log"
  fi

  local mismatched_lock="$managed_dir/mismatched-lock.json"
  sed 's/"version":"2026.08"/"version":"2026.09"/' \
    "$managed_lock" >"$mismatched_lock"
  if "$bin" push "$managed_jar" demo/mismatched:1 --store "$store" \
      --dependency-bundle "$managed_ref" --dependency-lock "$mismatched_lock" \
      --trusted-public-key "$managed_public" --signing-key "$managed_private" \
      --trusted-signer-identity e2e-builder --builder-identity e2e-builder \
      --main-class com.example.ManagedApp \
      >"$WORK/t2-managed-mismatch.log" 2>&1; then
    fail "managed: reject application graph mismatch" \
      "publication unexpectedly succeeded"
  else
    pass "managed: reject application graph mismatch"
  fi

  local fat_jar="$managed_dir/fat-app.jar"
  cp "$managed_jar" "$fat_jar"
  jar uf "$fat_jar" -C "$managed_dir/lib" approved.jar
  if "$bin" push "$fat_jar" demo/fat:1 --store "$store" \
      --dependency-bundle "$managed_ref" --dependency-lock "$managed_lock" \
      --trusted-public-key "$managed_public" --signing-key "$managed_private" \
      --trusted-signer-identity e2e-builder --builder-identity e2e-builder \
      --main-class com.example.ManagedApp \
      >"$WORK/t2-managed-fat.log" 2>&1; then
    fail "managed: reject application JAR containing dependencies" \
      "publication unexpectedly succeeded"
  else
    pass "managed: reject application JAR containing dependencies"
  fi
  if out="$("$bin" run "$managed_app1" --store "$store" 2>&1)"; then
    assert_contains "managed: JVM loads class from approved dependency layer" \
      "$out" "MANAGED DEPENDENCY OK"
  else
    fail "managed: JVM loads class from approved dependency layer" \
      "$(printf '%s' "$out" | tail -1)"
  fi

  # --- managed dependencies: live OCI registry referrers --------------------
  if have docker && have mvn; then
    local registry_id registry_port registry_ref registry_log="$WORK/t2-registry.log"
    local maven_bom="com.example.platform:approved-bom:1.0.0"
    local maven_bundle_pom="$FIXTURES_DIR/managed-dependency-bundle/pom.xml"
    local maven_bundle_layout="$FIXTURES_DIR/managed-dependency-bundle/target/brewlet/dependency-bundle-oci"
    registry_id="$(docker run -d -P registry:3 2>>"$registry_log")"
    registry_port="$(docker port "$registry_id" 5000/tcp 2>>"$registry_log" \
      | head -1 | sed 's/.*://')"
    registry_ref="localhost:$registry_port"
    local registry_ready=false
    for _ in {1..50}; do
      if curl -fsS "http://$registry_ref/v2/" >/dev/null 2>&1; then
       registry_ready=true
       break
      fi
      sleep 0.2
    done
    if [[ "$registry_ready" == true ]] \
       && mvn -q -f "$MONOREPO_DIR/maven-plugin/pom.xml" install \
         >>"$registry_log" 2>&1 \
       && mvn -q -f "$FIXTURES_DIR/managed-dependency-bom/pom.xml" install \
         >>"$registry_log" 2>&1 \
       && mvn -q -f "$maven_bundle_pom" package \
         sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT:dependency-bundle \
         -Dbrewlet.dependencyBundleImage="$registry_ref/platform/approved:1" \
         -Dbrewlet.sourceBom="$maven_bom" \
         -Dbrewlet.signingKey="$managed_private" \
         -Dbrewlet.signerIdentity=platform-builder >>"$registry_log" 2>&1 \
       && "$bin" inspect "$registry_ref/platform/approved:1" \
         --store "$maven_bundle_layout" \
         --trusted-public-key "$managed_public" \
         --trusted-signer-identity=platform-builder >>"$registry_log" 2>&1 \
       && python3 "$E2E_DIR/validate-managed-oci.py" bundle \
         "$maven_bundle_layout" \
         "$registry_ref/platform/approved:1" --require-signature \
         >>"$registry_log" 2>&1 \
       && mvn -q -f "$FIXTURES_DIR/demo-app/pom.xml" \
         -Pmanaged-dependencies package \
         sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT:push \
         -Dbrewlet.image="$registry_ref/apps/demo:1" \
         -Dbrewlet.dependencyBundle="$registry_ref/platform/approved:1" \
         -Dbrewlet.mainClass=com.example.Hello \
         -Dbrewlet.signingKey="$managed_private" \
         -Dbrewlet.trustedPublicKey="$managed_public" \
         -Dbrewlet.trustedSignerIdentity=platform-builder \
         -Dbrewlet.builderIdentity=application-builder >>"$registry_log" 2>&1 \
       && mvn -q -f "$FIXTURES_DIR/demo-app/pom.xml" \
         -Pmanaged-dependencies package \
         sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT:push \
         -Dbrewlet.image="$registry_ref/apps/signed-bundle-unsigned-app:1" \
         -Dbrewlet.dependencyBundle="$registry_ref/platform/approved:1" \
         -Dbrewlet.mainClass=com.example.Hello \
         -Dbrewlet.trustedPublicKey="$managed_public" \
         -Dbrewlet.signerIdentity=platform-builder >>"$registry_log" 2>&1 \
       && python3 "$E2E_DIR/validate-managed-registry.py" \
         "$registry_ref" platform/approved 1 apps/demo 1 "$maven_bom" \
         org.apache.commons:commons-lang3:3.17.0 >>"$registry_log" 2>&1; then
      local bundle_tags app_tags
      bundle_tags="$(curl -fsS \
       "http://$registry_ref/v2/platform/approved/tags/list")"
      app_tags="$(curl -fsS "http://$registry_ref/v2/apps/demo/tags/list")"
      local bundle_referrer_tags
      bundle_referrer_tags="$(printf '%s' "$bundle_tags" | grep -o 'sha256-' | wc -l \
       | tr -d ' ')"
      assert_eq "managed registry: publishes SBOM and provenance fallback refs" \
       "$bundle_referrer_tags" "2"
      assert_contains "managed registry: publishes final-image attestation fallback ref" \
       "$app_tags" "sha256-"
      pass "managed registry: Go verifies Maven bundle signatures"
      pass "managed registry: Maven bundle satisfies the normative wire contract"
      pass "managed registry: bundle resolves dependency version from imported BOM"
      pass "managed registry: final image reuses the BOM-derived OCI layer descriptor"
      pass "managed registry: verifies bundle referrers before remote composition"
      pass "managed registry: legacy signer identity does not force application signing"
    else
      fail "managed registry: publish and consume signed referrers" \
       "see $registry_log"
    fi
    local unsigned_layout="$WORK/t2-unsigned-bundle"
    if mvn -q -f "$maven_bundle_pom" package \
        sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT:dependency-bundle \
        -Dbrewlet.dependencyBundleImage="$registry_ref/platform/unsigned:1" \
        -Dbrewlet.dependencyBundleOutputDirectory="$unsigned_layout" \
        -Dbrewlet.sourceBom="$maven_bom" \
        >>"$registry_log" 2>&1 \
        && python3 "$E2E_DIR/validate-managed-oci.py" bundle \
          "$unsigned_layout" "$registry_ref/platform/unsigned:1" \
          >>"$registry_log" 2>&1 \
        && mvn -q -f "$FIXTURES_DIR/demo-app/pom.xml" \
          -Pmanaged-dependencies package \
          sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT:push \
          -Dbrewlet.image="$registry_ref/apps/unsigned:1" \
          -Dbrewlet.dependencyBundle="$registry_ref/platform/unsigned:1" \
          -Dbrewlet.mainClass=com.example.Hello >>"$registry_log" 2>&1 \
        && mvn -q -f "$FIXTURES_DIR/demo-app/pom.xml" \
          -Pmanaged-dependencies package \
          sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT:push \
          -Dbrewlet.image="$registry_ref/apps/unsigned-bundle-signed-app:1" \
          -Dbrewlet.dependencyBundle="$registry_ref/platform/unsigned:1" \
          -Dbrewlet.mainClass=com.example.Hello \
          -Dbrewlet.signingKey="$managed_private" \
          -Dbrewlet.builderIdentity=application-builder >>"$registry_log" 2>&1; then
      pass "managed registry: unsigned bundle and application are consumable"
      pass "managed registry: unsigned bundle can produce a signed application"
    else
      fail "managed registry: consume unsigned bundle" \
        "see $registry_log"
    fi
    docker rm -f "$registry_id" >/dev/null 2>&1 || true
  else
    skip "managed registry referrers" "docker and mvn are required"
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
