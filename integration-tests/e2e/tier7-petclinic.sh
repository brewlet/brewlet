#!/usr/bin/env bash
# Tier 7 — the real Spring Boot fat JAR (upstream Spring PetClinic) end-to-end.
# Proves the Brewlet model against a genuine, dependency-heavy application (not a
# toy HTTP server), on two fronts:
#
#   A) developer + node mechanism: build the real PetClinic fat JAR, `push` it as
#      an OCI artifact (ship ONLY the JAR), `inspect` it, then run it through the
#      shim -> runc path so `java -jar` serves the live app as PID 1 under real
#      cgroup limits with a node-resident JDK.
#   B) Kubernetes control plane: the brewlet-operator (out-of-cluster) reconciles
#      a PetClinic `JavaApplication` into a Deployment(+runtimeClassName: brewlet)
#      + Service on the live cluster.
#
# Everything SKIPs gracefully when a prerequisite is missing (no network to build
# PetClinic, no docker, no cluster), so the same command works on a laptop or CI.
# Prereqs: go, java, mvn/network (build); docker (runc); kubectl+cluster (operator).

T7_NS_OP="brewlet-petclinic"
T7_NS_APP="petclinic-e2e"
T7_MGR_PID=""
T7_JAR=""

_t7_cleanup() {
  info "tier7: cleaning up"
  [[ -n "$T7_MGR_PID" ]] && kill "$T7_MGR_PID" 2>/dev/null || true
  kubectl delete javaapplication petclinic -n "$T7_NS_APP" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete ns "$T7_NS_APP" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete ns "$T7_NS_OP" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete crd javaapplications.apps.brewlet.sh --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

_t7_dep_exists() { kubectl get deploy petclinic -n "$T7_NS_APP" >/dev/null 2>&1; }
_t7_svc_exists() { kubectl get svc petclinic -n "$T7_NS_APP" >/dev/null 2>&1; }
_t7_dep_gone()   { ! kubectl get deploy petclinic -n "$T7_NS_APP" >/dev/null 2>&1; }

tier7_petclinic() {
  section "Tier 7 — real Spring Boot fat JAR (Spring PetClinic)"
  if ! have go;   then skip "tier7: spring petclinic" "go not installed"; return 0; fi
  if ! have java; then skip "tier7: spring petclinic" "java not installed"; return 0; fi

  local jh
  jh="$(resolve_java_home)"
  export JAVA_HOME="$jh"
  export PATH="$JAVA_HOME/bin:$PATH"
  info "JAVA_HOME=$JAVA_HOME"

  trap _t7_cleanup RETURN

  local bin="$WORK/t7-brewlet"

  # --- build the brewlet CLI ------------------------------------------------
  if ( cd "$BREWLET_CORE_DIR" && go build -o "$bin" ./cmd/brewlet ) >"$WORK/t7-build.log" 2>&1; then
    pass "build brewlet CLI"
  else
    fail "build brewlet CLI" "see $WORK/t7-build.log"; return 0
  fi

  # === Part A: real fat JAR — build, push, inspect, run via shim -> runc =====

  # Build the real upstream Spring PetClinic fat JAR. This clones + `mvn package`,
  # so it needs network + a working Maven; SKIP the JAR-dependent parts if it fails.
  info "building the real Spring PetClinic fat JAR (clone + mvn package; ~1-3 min cold)"
  if "$FIXTURES_DIR/spring-petclinic/build.sh" >"$WORK/t7-petclinic-build.log" 2>&1 \
       && [[ -f "$FIXTURES_DIR/spring-petclinic/target/spring-petclinic.jar" ]]; then
    pass "build: real Spring PetClinic fat JAR (Spring Boot repackage)"
    T7_JAR="$FIXTURES_DIR/spring-petclinic/target/spring-petclinic.jar"
  else
    skip "tier7: fat-JAR path (push/inspect/runc)" "could not build PetClinic (network/maven?) — see $WORK/t7-petclinic-build.log"
  fi

  if [[ -n "$T7_JAR" ]]; then
    local store="$WORK/oci-pc" ref="demo/petclinic:1.0.0" out
    rm -rf "$store"

    # push: ship ONLY the Spring Boot fat JAR as an OCI artifact.
    if out="$("$bin" push "$T7_JAR" "$ref" --store "$store" --format=artifact 2>&1)"; then
      assert_contains "push: ships the PetClinic fat JAR as an OCI artifact" "$out" "pushed $ref"
      assert_contains "push: no Dockerfile involved" "$out" "no Dockerfile"
    else
      fail "push: PetClinic fat JAR" "$(printf '%s' "$out" | tail -1)"
    fi

    # inspect: the launch config records jar mode and the main jar. JDK selection
    # and ports live in the deployment descriptor / CRI metadata, not the artifact.
    if out="$("$bin" inspect "$ref" --store "$store" 2>&1)"; then
      assert_contains "inspect: records the PetClinic main jar" "$out" "spring-petclinic.jar"
      assert_contains "inspect: launches via java -jar (mode jar)" "$out" "\"mode\": \"jar\""
      assert_not_contains "inspect: artifact carries no JDK feature" "$out" "\"feature\""
      assert_not_contains "inspect: artifact carries no ports" "$out" "\"ports\""
    else
      fail "inspect: PetClinic artifact" "$(printf '%s' "$out" | tail -1)"
    fi

    # runc: the shim disassembles the artifact and runc runs the live Spring Boot
    # app as PID 1 under cgroups with a node JDK (the real node mechanism).
    if ! have docker; then
      skip "tier7: shim -> runc (Spring Boot under cgroups)" "docker not installed"
    elif ! docker info >/dev/null 2>&1; then
      skip "tier7: shim -> runc (Spring Boot under cgroups)" "docker daemon not reachable"
    else
      local arch
      case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; *) arch=amd64 ;; esac
      mkdir -p "$WORK/bin"
      if ! (cd "$BREWLET_CORE_DIR" && GOOS=linux GOARCH="$arch" \
          go build -o "$WORK/bin/shim-linux" ./shim/cmd/containerd-shim-brewlet-v2) \
          >>"$WORK/t7-build.log" 2>&1; then
        fail "runc: build Linux shim" "see $WORK/t7-build.log"
      elif rm -rf "$WORK/oci" && cp -R "$store" "$WORK/oci" \
        && out="$(docker run --rm --privileged --platform "linux/$arch" --cgroupns=private \
          --memory=768m --cpus=1 -v "$WORK:/work" \
          -v "$E2E_DIR/petclinic-runc.sh:/runner.sh:ro" \
          eclipse-temurin:21 bash /runner.sh 2>&1)"; then
        printf '%s\n' "$out" >"$WORK/t7-petclinic-runc.log"
        assert_contains "runc: shim built the OCI bundle for the Spring Boot app" "$out" "OCI runtime bundle"
        assert_contains "runc: Spring Boot answered /actuator/health = UP under runc" "$out" "\"status\":\"UP\""
        assert_contains "runc: the real PetClinic welcome page was served" "$out" "PetClinic :: a Spring Framework demonstration"
        assert_contains "runc: the JVM saw the sandbox cgroup CPU limit (1)" "$out" "\"value\":1.0"
        assert_contains "runc: end-to-end PetClinic run completed" "$out" "== petclinic done =="
      else
        printf '%s\n' "$out" >"$WORK/t7-petclinic-runc.log"
        fail "runc: PetClinic Linux harness" "see $WORK/t7-petclinic-runc.log"
      fi
    fi
  fi

  # === Part A2: layered classpath — split the fat JAR, push, prove dedup, run ===
  # Same real PetClinic, but shipped as a MULTI-LAYER classpath deployment: a thin
  # application JAR (BOOT-INF/classes) + separate dependency-layer tar(s). Rebuild
  # only the business code and just the small app-JAR layer changes; the ~63MB
  # dependency layer keeps its digest and is deduped. This is the "only business
  # code redeploys frequently" story, proven against a genuine app.
  #
  # The split (layered-build.sh) uses only GENERIC, framework-agnostic steps and does
  # NOT parse the fat JAR's BOOT-INF/layers.idx — this tier is the interop proof for
  # issue #66 (Spring layered output -> Brewlet's generic classpath layers). The
  # dependency-layer digest-reuse assertion below is exactly that dedup guarantee.
  if [[ -n "$T7_JAR" ]]; then
    info "splitting the PetClinic fat JAR into a layered classpath deployment"
    if "$FIXTURES_DIR/spring-petclinic/layered-build.sh" >"$WORK/t7-layered-build.log" 2>&1 \
         && [[ -f "$FIXTURES_DIR/spring-petclinic/target/layered/spring-petclinic-app.jar" ]] \
         && [[ -f "$FIXTURES_DIR/spring-petclinic/target/layered/deps-dependencies.tar" ]] \
         && [[ -f "$FIXTURES_DIR/spring-petclinic/target/layered/jvm-config.json" ]]; then
      pass "layered: split fat JAR -> thin app JAR + dependency layer(s) + config"
    else
      fail "layered: split the PetClinic fat JAR" "see $WORK/t7-layered-build.log"
    fi

    local L="$FIXTURES_DIR/spring-petclinic/target/layered"
    if [[ -f "$L/spring-petclinic-app.jar" ]]; then
      # The thin app JAR must be a small fraction of the fat JAR (classes only).
      local fatsz appsz
      fatsz="$(wc -c <"$T7_JAR")"; appsz="$(wc -c <"$L/spring-petclinic-app.jar")"
      if (( appsz * 10 < fatsz )); then
        pass "layered: thin app JAR is <10% of the fat JAR ($((appsz/1024))KB vs $((fatsz/1024/1024))MB)"
      else
        fail "layered: thin app JAR unexpectedly large" "$appsz bytes vs fat $fatsz"
      fi

      local lstore="$WORK/oci-pcl" lref="demo/petclinic-layered:1.0.0"
      rm -rf "$lstore"

      # push: ship the thin app JAR + attach the dependency layer.
      if out="$("$bin" push "$L/spring-petclinic-app.jar" "$lref" --store "$lstore" \
                 --config "$L/jvm-config.json" --classpath-layer "$L/deps-dependencies.tar" --format=artifact 2>&1)"; then
        assert_contains "layered: push attaches the dependency layer" "$out" "classpath layers: 1"
      else
        fail "layered: push layered artifact" "$(printf '%s' "$out" | tail -1)"
      fi

      # inspect: classpath mode, the application main class, and the two layer kinds.
      if out="$("$bin" inspect "$lref" --store "$lstore" 2>&1)"; then
        assert_contains "layered: inspect records mode=classpath" "$out" "\"mode\": \"classpath\""
        assert_contains "layered: inspect records the application main class" "$out" "org.springframework.samples.petclinic.PetClinicApplication"
        assert_contains "layered: classPath fans out to the unpacked lib dir" "$out" "\"lib/*\""
        assert_contains "layered: carries a thin app-JAR layer" "$out" "jar.layer.v1+jar"
        assert_contains "layered: carries a dependency (classpath) layer" "$out" "classpath.layer.v1+tar"
      else
        fail "layered: inspect layered artifact" "$(printf '%s' "$out" | tail -1)"
      fi

      # dedup: rebuild ONLY the business code and re-push; the dependency layer
      # digest must be reused while the app-JAR layer digest changes.
      local cp1 jar1 cp2 jar2
      _digcp() { "$bin" inspect "$lref" --store "$lstore" 2>/dev/null | awk '/classpath.layer.v1/{getline; print $2}' | tr -d '",'; }
      _digjar() { "$bin" inspect "$lref" --store "$lstore" 2>/dev/null | awk '/jar.layer.v1/{getline; print $2}' | tr -d '",'; }
      cp1="$(_digcp)"; jar1="$(_digjar)"
      # simulate a code change: add a resource and repack just the thin app JAR.
      ( cd "$L" && rm -rf _cls && mkdir _cls && ( cd _cls && jar xf ../spring-petclinic-app.jar ) \
          && date +%s >_cls/redeploy-marker.txt \
          && jar --create --file spring-petclinic-app.jar -C _cls . && rm -rf _cls )
      "$bin" push "$L/spring-petclinic-app.jar" "$lref" --store "$lstore" \
          --config "$L/jvm-config.json" --classpath-layer "$L/deps-dependencies.tar" --format=artifact >/dev/null 2>&1
      cp2="$(_digcp)"; jar2="$(_digjar)"
      if [[ -n "$cp1" && "$cp1" == "$cp2" ]]; then
        pass "layered: rebuilding business code REUSES the dependency layer (deduped, not re-pushed)"
      else
        fail "layered: dependency layer should be reused across rebuilds" "before=$cp1 after=$cp2"
      fi
      if [[ -n "$jar1" && "$jar1" != "$jar2" ]]; then
        pass "layered: only the thin app-JAR layer digest changed on redeploy"
      else
        fail "layered: app-JAR layer digest should change when code changes" "before=$jar1 after=$jar2"
      fi

      # runc: run the layered artifact as PID 1 (java -cp app.jar:lib/*) under cgroups.
      if ! have docker; then
        skip "tier7: layered shim -> runc" "docker not installed"
      elif ! docker info >/dev/null 2>&1; then
        skip "tier7: layered shim -> runc" "docker daemon not reachable"
      else
        rm -rf "$WORK/oci"
        cp -R "$lstore" "$WORK/oci"
        if out="$(docker run --rm --privileged --platform "linux/$arch" --cgroupns=private \
          --memory=768m --cpus=1 -v "$WORK:/work" \
          -v "$E2E_DIR/petclinic-layered-runc.sh:/runner.sh:ro" \
          eclipse-temurin:21 bash /runner.sh 2>&1)"; then
          printf '%s\n' "$out" >"$WORK/t7-petclinic-layered-runc.log"
          assert_contains "layered runc: shim unpacked deps + built the OCI bundle" "$out" "OCI runtime bundle"
          assert_contains "layered runc: launched via -cp app.jar:lib/*" "$out" "spring-petclinic-app.jar"
          assert_contains "layered runc: Spring Boot answered /actuator/health = UP" "$out" "\"status\":\"UP\""
          assert_contains "layered runc: the real PetClinic welcome page was served" "$out" "PetClinic :: a Spring Framework demonstration"
          assert_contains "layered runc: end-to-end layered PetClinic run completed" "$out" "== petclinic-layered done =="
        else
          printf '%s\n' "$out" >"$WORK/t7-petclinic-layered-runc.log"
          fail "layered runc: PetClinic Linux harness" "see $WORK/t7-petclinic-layered-runc.log"
        fi
      fi
    fi
  fi

  # === Part B: Kubernetes control plane — reconcile a PetClinic JavaApplication =
  if ! have kubectl; then skip "tier7: k8s reconcile" "kubectl not installed"; return 0; fi
  if ! k8s_reachable;  then skip "tier7: k8s reconcile" "no reachable cluster";  return 0; fi

  info "cluster context: $(kubectl config current-context 2>/dev/null)"

  kubectl create namespace "$T7_NS_OP" >/dev/null 2>&1 || true
  if kubectl apply -f "$BREWLET_KUBERNETES_DIR/deploy/javaapplication-crd.yaml" >"$WORK/t7-crd.log" 2>&1 \
     && kubectl wait --for=condition=Established --timeout=30s \
          crd/javaapplications.apps.brewlet.sh >>"$WORK/t7-crd.log" 2>&1; then
    pass "CRD: JavaApplication installed and Established"
  else
    fail "CRD: JavaApplication Established" "see $WORK/t7-crd.log"; return 0
  fi

  # Start the operator out-of-cluster (built binary + your kubeconfig), just like
  # tier 4 — no operator image build/load needed.
  local probe; probe="$(free_port)"
  if ( cd "$BREWLET_KUBERNETES_DIR" && go build -o "$WORK/t7-manager" ./cmd/manager ) >"$WORK/t7-op-build.log" 2>&1; then
    pass "build brewlet-operator manager"
  else
    fail "build brewlet-operator manager" "see $WORK/t7-op-build.log"; return 0
  fi
  "$WORK/t7-manager" \
      --namespace "$T7_NS_OP" \
      --provisioner-image "brewlet-e2e/nonexistent-provisioner:donotpull" \
      --jdks "temurin-17" --launchers "" \
      --leader-elect=false \
      --metrics-bind-address 0 \
      --health-probe-bind-address ":$probe" \
      >"$WORK/t7-manager.log" 2>&1 &
  T7_MGR_PID=$!
  if retry_curl "http://localhost:$probe/readyz" 40 0.5 >/dev/null; then
    pass "operator: manager started and healthy (readyz)"
  else
    fail "operator: manager readyz" "see $WORK/t7-manager.log"; return 0
  fi

  # Apply the PetClinic JavaApplication (the shipped deploy descriptor's shape,
  # pinned to our test namespace).
  kubectl create namespace "$T7_NS_APP" >/dev/null 2>&1 || true
  cat >"$WORK/t7-petclinic-japp.yaml" <<YAML
apiVersion: apps.brewlet.sh/v1alpha1
kind: JavaApplication
metadata:
  name: petclinic
  namespace: $T7_NS_APP
spec:
  artifact:
    image: registry.example.com/demo/petclinic:1.0.0
    pullPolicy: IfNotPresent
  replicas: 2
  resources:
    requests: { cpu: "500m", memory: "512Mi" }
    limits:   { cpu: "1",    memory: "768Mi" }
  jvm:
    version: 17
    args: ["-XX:MaxRAMPercentage=75.0"]
  env:
    - name: SPRING_PROFILES_ACTIVE
      value: default
  ports:
    - name: http
      containerPort: 8080
  service:
    enabled: true
    type: ClusterIP
  probes:
    readiness: { httpGet: { path: /actuator/health/readiness, port: 8080 } }
    liveness:  { httpGet: { path: /actuator/health/liveness,  port: 8080 } }
YAML
  if kubectl apply -f "$WORK/t7-petclinic-japp.yaml" >"$WORK/t7-apply.log" 2>&1; then
    pass "JavaApplication: PetClinic descriptor accepted by the API server"
  else
    fail "JavaApplication: apply PetClinic descriptor" "see $WORK/t7-apply.log"
  fi

  if wait_for _t7_dep_exists; then
    pass "controller: reconciled a managed Deployment for PetClinic"
    assert_eq "controller: Deployment routes to the brewlet runtime" \
      "$(kubectl get deploy petclinic -n "$T7_NS_APP" -o jsonpath='{.spec.template.spec.runtimeClassName}')" "brewlet"
    assert_eq "controller: Deployment ships the OCI artifact (not an image build)" \
      "$(kubectl get deploy petclinic -n "$T7_NS_APP" -o jsonpath='{.spec.template.spec.containers[0].image}')" \
      "registry.example.com/demo/petclinic:1.0.0"
    assert_eq "controller: Deployment carries the requested replicas" \
      "$(kubectl get deploy petclinic -n "$T7_NS_APP" -o jsonpath='{.spec.replicas}')" "2"
  else
    fail "controller: reconciled a managed Deployment for PetClinic" "see $WORK/t7-manager.log"
  fi

  if wait_for _t7_svc_exists; then
    pass "controller: reconciled a managed Service for PetClinic"
  else
    fail "controller: reconciled a managed Service for PetClinic"
  fi

  # Garbage collection on delete (owner references).
  kubectl delete javaapplication petclinic -n "$T7_NS_APP" --wait=false >/dev/null 2>&1
  if wait_for _t7_dep_gone; then
    pass "controller: deleting the JavaApplication GCs its Deployment"
  else
    fail "controller: child Deployment garbage-collected on delete"
  fi
}
