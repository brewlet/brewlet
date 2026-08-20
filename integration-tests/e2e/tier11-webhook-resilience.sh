#!/usr/bin/env bash
# Tier 11 — admission webhook RESILIENCE: failurePolicy under a webhook outage.
#
# Tiers 5/6 only exercise the webhook while it is UP (admit/deny logic). A webhook
# outage is a real production concern with a deliberate design choice (§8/§14):
# the chart ships failurePolicy: Ignore so an outage never blocks workloads (the
# shim still enforces JDK/launcher compatibility at runtime). This tier proves
# both stances end to end, in-cluster, against the real admission binary:
#
#   A) fail-open  (Ignore) + webhook scaled to 0  -> brewlet pod is ADMITTED but
#      UNMUTATED (no brewlet.sh/artifact-ref stamped, no nodeAffinity) — the shim
#      is the backstop.
#   B) fail-closed (Fail)  + webhook still down    -> brewlet pod is REJECTED
#      (API server can't call the webhook).
#   C) recovery: webhook back up (still Fail)       -> brewlet pod is ADMITTED
#      and MUTATED again (artifact-ref stamped) — mutation resumes cleanly.
#
# Deploys the same admission image as tier 6, behind a namespace-scoped webhook
# config so it only ever intercepts this tier's probe namespace. SKIPs gracefully
# unless: kubectl + cluster, docker, openssl, and nodes loadable via `ctr`.
# Prereqs: kubectl + cluster, docker, openssl, go (build image).

T11_IMG="brewlet.local/admission:e2e-res"
T11_SYS_NS="brewlet-webhook-res-sys"    # webhook Deployment/Service/Secret/RBAC
T11_NS="brewlet-webhook-res"            # labelled probe namespace
T11_SVC="brewlet-admission-res"
T11_CFG="brewlet-admission-res"
T11_RC_CREATED=""
declare -a T11_LOADED_NODES=()

_t11_cleanup() {
  info "tier11: cleaning up"
  kubectl delete mutatingwebhookconfiguration "$T11_CFG" --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete ns "$T11_NS" "$T11_SYS_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete clusterrole "$T11_SVC" --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete clusterrolebinding "$T11_SVC" --ignore-not-found >/dev/null 2>&1 || true
  [[ -n "$T11_RC_CREATED" ]] && kubectl delete runtimeclass brewlet --ignore-not-found >/dev/null 2>&1 || true
  for n in "${T11_LOADED_NODES[@]}"; do
    docker exec "$n" ctr -n k8s.io images rm "$T11_IMG" >/dev/null 2>&1 || true
  done
  docker rmi "$T11_IMG" >/dev/null 2>&1 || true
}

# _t11_set_policy POLICY -> patch the webhook config's failurePolicy in place.
_t11_set_policy() {
  kubectl patch mutatingwebhookconfiguration "$T11_CFG" --type=json \
    -p "[{\"op\":\"replace\",\"path\":\"/webhooks/0/failurePolicy\",\"value\":\"$1\"}]" \
    >/dev/null 2>&1
}

# _t11_scale N -> scale the webhook Deployment and wait for the replica count.
_t11_scale() {
  kubectl scale deploy/"$T11_SVC" -n "$T11_SYS_NS" --replicas="$1" >/dev/null 2>&1 || true
}

# _t11_brewlet_pod NAME -> create a brewlet pod in the probe ns; echo apply output.
_t11_brewlet_pod() {
  kubectl apply -n "$T11_NS" -f - 2>&1 <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $1
spec:
  runtimeClassName: brewlet
  containers:
    - name: app
      image: registry.example.com/team/orders:1.0.0
YAML
}

# _t11_artifact_ref NAME -> the stamped brewlet.sh/artifact-ref annotation ("" if none).
_t11_artifact_ref() {
  kubectl get pod "$1" -n "$T11_NS" \
    -o jsonpath='{.metadata.annotations.brewlet\.sh/artifact-ref}' 2>/dev/null
}

tier11_webhook_resilience() {
  section "Tier 11 — admission webhook resilience (failurePolicy under outage)"
  if ! have kubectl || ! k8s_reachable; then skip "tier11: webhook resilience" "no reachable cluster"; return 0; fi
  if ! have docker || ! docker info >/dev/null 2>&1; then skip "tier11: webhook resilience" "docker daemon not available"; return 0; fi
  if ! have openssl; then skip "tier11: webhook resilience" "openssl not installed"; return 0; fi
  if ! have go; then skip "tier11: webhook resilience" "go not installed (needed to build image)"; return 0; fi

  local nodes n
  nodes="$(kubectl get nodes -o name 2>/dev/null | sed 's#node/##')"
  if [[ -z "$nodes" ]]; then skip "tier11: webhook resilience" "no nodes"; return 0; fi
  for n in $nodes; do
    if ! docker inspect "$n" >/dev/null 2>&1 || ! docker exec "$n" ctr --version >/dev/null 2>&1; then
      skip "tier11: webhook resilience" "node '$n' is not a local containerd docker container (can't side-load image)"
      return 0
    fi
  done

  trap _t11_cleanup RETURN

  # --- build + side-load the admission image --------------------------------
  info "tier11: building $T11_IMG (docker build, may take a minute)"
  if ! docker build --provenance=false --build-arg CMD=admission -t "$T11_IMG" \
        -f "$BREWLET_KUBERNETES_DIR/Dockerfile" "$BREWLET_KUBERNETES_DIR" \
        >"$WORK/t11-build.log" 2>&1; then
    if ! docker build --build-arg CMD=admission -t "$T11_IMG" \
          -f "$BREWLET_KUBERNETES_DIR/Dockerfile" "$BREWLET_KUBERNETES_DIR" \
          >>"$WORK/t11-build.log" 2>&1; then
      fail "webhook(resilience): build $T11_IMG" "see $WORK/t11-build.log"; return 0
    fi
  fi
  local tarball="$WORK/t11-admission.tar"
  if ! docker save "$T11_IMG" -o "$tarball" 2>>"$WORK/t11-load.log"; then
    fail "webhook(resilience): docker save image" "see $WORK/t11-load.log"; return 0
  fi
  for n in $nodes; do
    if ! docker exec -i "$n" ctr -n k8s.io images import - <"$tarball" >>"$WORK/t11-load.log" 2>&1; then
      skip "tier11: webhook resilience" "could not import image into node '$n' (see $WORK/t11-load.log)"; return 0
    fi
    T11_LOADED_NODES+=("$n")
  done
  pass "webhook(resilience): built + side-loaded admission image"

  # --- RuntimeClass + namespaces --------------------------------------------
  if ! kubectl get runtimeclass brewlet >/dev/null 2>&1; then
    kubectl create -f - >/dev/null 2>&1 <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: { name: brewlet }
handler: brewlet
YAML
    T11_RC_CREATED=1
  fi
  kubectl create namespace "$T11_SYS_NS" >/dev/null 2>&1 || true
  kubectl create namespace "$T11_NS" >/dev/null 2>&1 || true
  kubectl label --overwrite ns "$T11_NS" brewlet-webhook-res=true >/dev/null 2>&1

  # --- serving cert ---------------------------------------------------------
  local cdir="$WORK/t11-certs"; mkdir -p "$cdir"
  if ! openssl req -x509 -newkey rsa:2048 -nodes \
        -keyout "$cdir/tls.key" -out "$cdir/tls.crt" -days 2 \
        -subj "/CN=$T11_SVC.$T11_SYS_NS.svc" \
        -addext "subjectAltName=DNS:$T11_SVC.$T11_SYS_NS.svc,DNS:$T11_SVC.$T11_SYS_NS.svc.cluster.local" \
        >"$WORK/t11-cert.log" 2>&1; then
    skip "tier11: webhook resilience" "cert generation failed (see $WORK/t11-cert.log)"; return 0
  fi
  local ca; ca="$(base64 < "$cdir/tls.crt" | tr -d '\n')"
  kubectl create secret tls "$T11_SVC-cert" -n "$T11_SYS_NS" \
    --cert="$cdir/tls.crt" --key="$cdir/tls.key" >/dev/null 2>&1 || \
    kubectl create secret tls "$T11_SVC-cert" -n "$T11_SYS_NS" \
      --cert="$cdir/tls.crt" --key="$cdir/tls.key" --dry-run=client -o yaml | kubectl apply -f - >/dev/null 2>&1

  # --- RBAC + Deployment + Service ------------------------------------------
  if ! kubectl apply -f - >"$WORK/t11-deploy.log" 2>&1 <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: { name: $T11_SVC, namespace: $T11_SYS_NS }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: { name: $T11_SVC }
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: { name: $T11_SVC }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: $T11_SVC }
subjects:
  - { kind: ServiceAccount, name: $T11_SVC, namespace: $T11_SYS_NS }
---
apiVersion: v1
kind: Service
metadata: { name: $T11_SVC, namespace: $T11_SYS_NS }
spec:
  selector: { app: $T11_SVC }
  ports:
    - { name: https, port: 443, targetPort: 9443 }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: $T11_SVC, namespace: $T11_SYS_NS }
spec:
  replicas: 1
  selector: { matchLabels: { app: $T11_SVC } }
  template:
    metadata: { labels: { app: $T11_SVC } }
    spec:
      serviceAccountName: $T11_SVC
      securityContext: { runAsNonRoot: true }
      containers:
        - name: webhook
          image: $T11_IMG
          imagePullPolicy: IfNotPresent
          args:
            - --webhook-port=9443
            - --cert-dir=/tmp/k8s-webhook-server/serving-certs
            - --health-probe-bind-address=:8081
            - --metrics-bind-address=0
          ports:
            - { name: https, containerPort: 9443 }
          readinessProbe:
            httpGet: { path: /readyz, port: 8081 }
            initialDelaySeconds: 3
            periodSeconds: 5
          livenessProbe:
            httpGet: { path: /healthz, port: 8081 }
            initialDelaySeconds: 10
            periodSeconds: 20
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }
          volumeMounts:
            - { name: serving-certs, mountPath: /tmp/k8s-webhook-server/serving-certs, readOnly: true }
      volumes:
        - name: serving-certs
          secret: { secretName: $T11_SVC-cert }
YAML
  then
    fail "webhook(resilience): apply Deployment/Service/RBAC" "see $WORK/t11-deploy.log"; return 0
  fi

  if ! kubectl rollout status deploy/"$T11_SVC" -n "$T11_SYS_NS" --timeout=120s >"$WORK/t11-rollout.log" 2>&1; then
    kubectl describe deploy/"$T11_SVC" -n "$T11_SYS_NS" >>"$WORK/t11-rollout.log" 2>&1 || true
    fail "webhook(resilience): Deployment became ready" "see $WORK/t11-rollout.log"; return 0
  fi

  # --- register the webhook (ns-scoped) — start fail-open (Ignore) ----------
  if ! kubectl apply -f - >"$WORK/t11-cfg.log" 2>&1 <<YAML
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: $T11_CFG
webhooks:
  - name: pods.brewlet.res
    admissionReviewVersions: ["v1"]
    sideEffects: None
    failurePolicy: Ignore
    timeoutSeconds: 5
    namespaceSelector:
      matchLabels: { brewlet-webhook-res: "true" }
    clientConfig:
      service:
        name: $T11_SVC
        namespace: $T11_SYS_NS
        path: /mutate-pods
        port: 443
      caBundle: $ca
    rules:
      - operations: ["CREATE"]
        apiGroups: [""]
        apiVersions: ["v1"]
        resources: ["pods"]
        scope: Namespaced
YAML
  then
    fail "webhook(resilience): register MutatingWebhookConfiguration" "see $WORK/t11-cfg.log"; return 0
  fi
  sleep 3

  # --- sanity: while UP + fail-open, a brewlet pod IS mutated ----------------
  local out ref
  out="$(_t11_brewlet_pod res-up)"
  if _wh_unreachable "$out"; then
    kubectl delete pod res-up -n "$T11_NS" --ignore-not-found >/dev/null 2>&1 || true
    fail "webhook(resilience): API server could not reach the webhook" "see $WORK/t11-cfg.log"; return 0
  fi
  ref="$(_t11_artifact_ref res-up)"
  kubectl delete pod res-up -n "$T11_NS" --ignore-not-found >/dev/null 2>&1 || true
  assert_contains "webhook(resilience): while up, a brewlet pod is mutated (baseline)" \
    "$ref" "registry.example.com/team/orders:1.0.0"

  # --- A) fail-open (Ignore) + webhook DOWN -> admitted, UNMUTATED ----------
  info "tier11: scaling the webhook to 0 (simulating an outage)"
  _t11_scale 0
  wait_for bash -c "[ \"\$(kubectl get deploy $T11_SVC -n $T11_SYS_NS -o jsonpath='{.status.replicas}' 2>/dev/null)\" = \"\" ] || [ \"\$(kubectl get deploy $T11_SVC -n $T11_SYS_NS -o jsonpath='{.status.readyReplicas}' 2>/dev/null)\" = \"\" ]"
  # Endpoints can linger briefly; give kube-apiserver a moment to see 0 endpoints.
  sleep 5
  out="$(_t11_brewlet_pod res-open)"
  if [[ "$out" == *created* || "$out" == *configured* || "$out" == *unchanged* ]]; then
    pass "webhook(resilience): fail-open (Ignore) admits a brewlet pod during an outage"
  else
    fail "webhook(resilience): fail-open admits during an outage" "got: $out"
  fi
  ref="$(_t11_artifact_ref res-open)"
  kubectl delete pod res-open -n "$T11_NS" --ignore-not-found >/dev/null 2>&1 || true
  assert_eq "webhook(resilience): the admitted pod was NOT mutated (shim is the backstop)" "${ref:-<none>}" "<none>"

  # --- B) fail-closed (Fail) + webhook DOWN -> REJECTED ---------------------
  info "tier11: switching failurePolicy to Fail (webhook still down)"
  _t11_set_policy Fail
  sleep 3
  out="$(_t11_brewlet_pod res-closed)"
  kubectl delete pod res-closed -n "$T11_NS" --ignore-not-found >/dev/null 2>&1 || true
  if _wh_unreachable "$out" || [[ "$out" == *"failurePolicy"* ]]; then
    pass "webhook(resilience): fail-closed (Fail) rejects a brewlet pod during an outage"
  else
    fail "webhook(resilience): fail-closed rejects during an outage" "got: $out"
  fi

  # --- C) recovery: webhook back up (still Fail) -> admitted + MUTATED -------
  info "tier11: restoring the webhook (scale back to 1)"
  _t11_scale 1
  if ! kubectl rollout status deploy/"$T11_SVC" -n "$T11_SYS_NS" --timeout=120s >>"$WORK/t11-rollout.log" 2>&1; then
    fail "webhook(resilience): webhook recovered (rollout)" "see $WORK/t11-rollout.log"; return 0
  fi
  sleep 3
  ref=""
  for _ in 1 2 3 4 5 6 7 8; do
    out="$(_t11_brewlet_pod res-recover)"
    if _wh_unreachable "$out"; then
      kubectl delete pod res-recover -n "$T11_NS" --ignore-not-found >/dev/null 2>&1 || true
      sleep 2; continue
    fi
    ref="$(_t11_artifact_ref res-recover)"
    kubectl delete pod res-recover -n "$T11_NS" --ignore-not-found >/dev/null 2>&1 || true
    [[ -n "$ref" ]] && break
    sleep 2
  done
  assert_contains "webhook(resilience): after recovery, mutation resumes (artifact-ref stamped)" \
    "$ref" "registry.example.com/team/orders:1.0.0"
}
