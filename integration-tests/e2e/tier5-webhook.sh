#!/usr/bin/env bash
# Tier 5 — admission/scheduling webhook, exercised for real through the API
# server with the webhook running ON THE HOST. The cluster reaches it via
# host.docker.internal (Docker Desktop). The assertions themselves live in
# webhook-cases.sh and are shared with Tier 6 (same webhook, in-cluster).
#
# This tier is BEST-EFFORT: if the cluster can't reach the host webhook it SKIPs
# (rather than fails) — e.g. on kind/Linux where host.docker.internal isn't
# resolvable from the node. Tier 6 gives the same coverage there.
# Prereqs: kubectl + cluster, go, openssl

T5_PID=""
T5_NS="brewlet-webhook-e2e"
T5_CFG="brewlet-admission-e2e"
T5_RC_CREATED=""

_t5_cleanup() {
  info "tier5: cleaning up"
  [[ -n "$T5_PID" ]] && kill "$T5_PID" 2>/dev/null || true
  kubectl delete mutatingwebhookconfiguration "$T5_CFG" --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete ns "$T5_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  [[ -n "$T5_RC_CREATED" ]] && kubectl delete runtimeclass brewlet --ignore-not-found >/dev/null 2>&1 || true
  if [[ -n "$WH_NODE" ]]; then
    kubectl label "$WH_NODE" brewlet.sh/runtime- >/dev/null 2>&1 || true
    kubectl annotate "$WH_NODE" brewlet.sh/jdks- brewlet.sh/launchers- >/dev/null 2>&1 || true
  fi
}

tier5_webhook() {
  section "Tier 5 — admission webhook via API server, host-bound (best-effort)"
  if ! have kubectl || ! k8s_reachable; then skip "tier5: admission webhook" "no reachable cluster"; return 0; fi
  if ! have go; then skip "tier5: admission webhook" "go not installed"; return 0; fi
  if ! have openssl; then skip "tier5: admission webhook" "openssl not installed"; return 0; fi

  WH_NODE=""
  trap _t5_cleanup RETURN

  # --- build admission binary ----------------------------------------------
  if ! ( cd "$BREWLET_KUBERNETES_DIR" && go build -o "$WORK/t5-admission" ./cmd/admission ) \
       >"$WORK/t5-build.log" 2>&1; then
    fail "webhook(host): build brewlet-admission" "see $WORK/t5-build.log"; return 0
  fi
  pass "webhook(host): build brewlet-admission"

  # --- self-signed serving cert (SAN host.docker.internal) -----------------
  local cdir="$WORK/webhook-certs"; mkdir -p "$cdir"
  if ! openssl req -x509 -newkey rsa:2048 -nodes \
        -keyout "$cdir/tls.key" -out "$cdir/tls.crt" -days 2 \
        -subj "/CN=host.docker.internal" \
        -addext "subjectAltName=DNS:host.docker.internal,DNS:localhost,IP:127.0.0.1" \
        >"$WORK/t5-cert.log" 2>&1; then
    skip "tier5: admission webhook" "cert generation failed (see $WORK/t5-cert.log)"; return 0
  fi
  local ca; ca="$(base64 < "$cdir/tls.crt" | tr -d '\n')"

  # --- start the webhook on the host ---------------------------------------
  local wport probe
  wport="$(free_port)"; probe="$(free_port)"
  "$WORK/t5-admission" \
      --cert-dir "$cdir" --webhook-port "$wport" \
      --metrics-bind-address 0 --health-probe-bind-address ":$probe" \
      >"$WORK/t5-admission.log" 2>&1 &
  T5_PID=$!
  if ! retry_curl "http://localhost:$probe/readyz" 40 0.5 >/dev/null; then
    skip "tier5: admission webhook" "webhook did not become ready (see $WORK/t5-admission.log)"; return 0
  fi
  pass "webhook(host): server started and healthy (readyz)"

  # --- prerequisites in the cluster ----------------------------------------
  # A brewlet RuntimeClass must exist so pods referencing it pass API validation.
  if ! kubectl get runtimeclass brewlet >/dev/null 2>&1; then
    kubectl create -f - >/dev/null 2>&1 <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: { name: brewlet }
handler: brewlet
YAML
    T5_RC_CREATED=1
  fi
  kubectl create namespace "$T5_NS" >/dev/null 2>&1 || true
  kubectl label --overwrite ns "$T5_NS" brewlet-webhook-e2e=true >/dev/null 2>&1

  # --- register the webhook (scoped to the test ns, failurePolicy Fail) -----
  if ! kubectl apply -f - >"$WORK/t5-cfg.log" 2>&1 <<YAML
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: $T5_CFG
webhooks:
  - name: pods.brewlet.e2e
    admissionReviewVersions: ["v1"]
    sideEffects: None
    failurePolicy: Fail
    timeoutSeconds: 5
    namespaceSelector:
      matchLabels: { brewlet-webhook-e2e: "true" }
    clientConfig:
      url: https://host.docker.internal:$wport/mutate-pods
      caBundle: $ca
    rules:
      - operations: ["CREATE"]
        apiGroups: [""]
        apiVersions: ["v1"]
        resources: ["pods"]
YAML
  then
    skip "tier5: admission webhook" "could not register webhook (see $WORK/t5-cfg.log)"; return 0
  fi

  # --- run the shared assertions -------------------------------------------
  webhook_cases "$T5_NS" "webhook(host)"
  if [[ $? -eq 2 ]]; then
    skip "tier5: admission webhook" "cluster cannot reach the host webhook (expected on kind/Linux; see Tier 6)"
  fi
}
