#!/usr/bin/env bash
# Tier 6 — admission/scheduling webhook run IN-CLUSTER, exactly as shipped by the
# Helm chart: the brewlet-admission image is built, loaded into the cluster's
# containerd, and deployed behind a Service + MutatingWebhookConfiguration that
# the API server reaches over the in-cluster network. It then runs the SAME
# assertions as Tier 5 (webhook-cases.sh).
#
# Unlike Tier 5 (host.docker.internal), this works anywhere the nodes are local
# containerd-backed docker containers — Docker Desktop AND kind/CI — so it is the
# real webhook coverage on Linux/CI. It SKIPs when the image can't be side-loaded
# (e.g. a remote cluster whose nodes aren't local docker containers).
# Prereqs: kubectl + cluster, docker, openssl, nodes loadable via `ctr`.

T6_IMG="brewlet.local/admission:e2e"
T6_SYS_NS="brewlet-webhook-ic-sys"   # webhook Deployment/Service/Secret/RBAC
T6_NS="brewlet-webhook-ic"           # labelled test namespace probe pods land in
T6_SVC="brewlet-admission-ic"
T6_CFG="brewlet-admission-ic"
T6_RC_CREATED=""
declare -a T6_LOADED_NODES=()

_t6_cleanup() {
  info "tier6: cleaning up"
  kubectl delete mutatingwebhookconfiguration "$T6_CFG" --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete ns "$T6_NS" "$T6_SYS_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete clusterrole "$T6_SVC" --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete clusterrolebinding "$T6_SVC" --ignore-not-found >/dev/null 2>&1 || true
  [[ -n "$T6_RC_CREATED" ]] && kubectl delete runtimeclass brewlet --ignore-not-found >/dev/null 2>&1 || true
  if [[ -n "$WH_NODE" ]]; then
    kubectl label "$WH_NODE" brewlet.sh/runtime- >/dev/null 2>&1 || true
    kubectl annotate "$WH_NODE" brewlet.sh/jdks- brewlet.sh/launchers- >/dev/null 2>&1 || true
  fi
  # Best-effort: drop the side-loaded image from each node and the host.
  for n in "${T6_LOADED_NODES[@]}"; do
    docker exec "$n" ctr -n k8s.io images rm "$T6_IMG" >/dev/null 2>&1 || true
  done
  docker rmi "$T6_IMG" >/dev/null 2>&1 || true
}

tier6_webhook_incluster() {
  section "Tier 6 — admission webhook in-cluster (real Deployment + Service)"
  if ! have kubectl || ! k8s_reachable; then skip "tier6: in-cluster webhook" "no reachable cluster"; return 0; fi
  if ! have docker || ! docker info >/dev/null 2>&1; then skip "tier6: in-cluster webhook" "docker daemon not available"; return 0; fi
  if ! have openssl; then skip "tier6: in-cluster webhook" "openssl not installed"; return 0; fi

  WH_NODE=""

  # --- every node must be a local docker container we can side-load into ----
  local nodes n
  nodes="$(kubectl get nodes -o name 2>/dev/null | sed 's#node/##')"
  if [[ -z "$nodes" ]]; then skip "tier6: in-cluster webhook" "no nodes"; return 0; fi
  for n in $nodes; do
    if ! docker inspect "$n" >/dev/null 2>&1 || ! docker exec "$n" ctr --version >/dev/null 2>&1; then
      skip "tier6: in-cluster webhook" "node '$n' is not a local containerd docker container (can't side-load image)"
      return 0
    fi
  done

  trap _t6_cleanup RETURN

  # --- build the admission image -------------------------------------------
  # --provenance=false keeps BuildKit from wrapping the image in an attestation
  # index the node's CRI can't resolve (it would otherwise try to pull). Fall
  # back to a plain build on Dockers too old to know the flag.
  info "tier6: building $T6_IMG (docker build, may take a minute)"
  if ! docker build --provenance=false --build-arg CMD=admission -t "$T6_IMG" \
        -f "$BREWLET_KUBERNETES_DIR/Dockerfile" "$BREWLET_KUBERNETES_DIR" \
        >"$WORK/t6-build.log" 2>&1; then
    if ! docker build --build-arg CMD=admission -t "$T6_IMG" \
          -f "$BREWLET_KUBERNETES_DIR/Dockerfile" "$BREWLET_KUBERNETES_DIR" \
          >>"$WORK/t6-build.log" 2>&1; then
      fail "webhook(in-cluster): build $T6_IMG" "see $WORK/t6-build.log"; return 0
    fi
  fi
  pass "webhook(in-cluster): built admission image"

  # --- side-load the image into every node's containerd (k8s.io namespace) --
  local tarball="$WORK/t6-admission.tar"
  if ! docker save "$T6_IMG" -o "$tarball" 2>>"$WORK/t6-load.log"; then
    fail "webhook(in-cluster): docker save image" "see $WORK/t6-load.log"; return 0
  fi
  for n in $nodes; do
    if ! docker exec -i "$n" ctr -n k8s.io images import - <"$tarball" >>"$WORK/t6-load.log" 2>&1; then
      skip "tier6: in-cluster webhook" "could not import image into node '$n' (see $WORK/t6-load.log)"; return 0
    fi
    T6_LOADED_NODES+=("$n")
  done
  pass "webhook(in-cluster): loaded image into $(echo "$nodes" | wc -w | tr -d ' ') node(s)"

  # --- namespaces + RuntimeClass -------------------------------------------
  if ! kubectl get runtimeclass brewlet >/dev/null 2>&1; then
    kubectl create -f - >/dev/null 2>&1 <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: { name: brewlet }
handler: brewlet
YAML
    T6_RC_CREATED=1
  fi
  kubectl create namespace "$T6_SYS_NS" >/dev/null 2>&1 || true
  kubectl create namespace "$T6_NS" >/dev/null 2>&1 || true
  kubectl label --overwrite ns "$T6_NS" brewlet-webhook-ic=true >/dev/null 2>&1

  # --- serving cert (SAN = <svc>.<sys-ns>.svc[.cluster.local]) --------------
  local cdir="$WORK/t6-certs"; mkdir -p "$cdir"
  if ! openssl req -x509 -newkey rsa:2048 -nodes \
        -keyout "$cdir/tls.key" -out "$cdir/tls.crt" -days 2 \
        -subj "/CN=$T6_SVC.$T6_SYS_NS.svc" \
        -addext "subjectAltName=DNS:$T6_SVC.$T6_SYS_NS.svc,DNS:$T6_SVC.$T6_SYS_NS.svc.cluster.local" \
        >"$WORK/t6-cert.log" 2>&1; then
    skip "tier6: in-cluster webhook" "cert generation failed (see $WORK/t6-cert.log)"; return 0
  fi
  local ca; ca="$(base64 < "$cdir/tls.crt" | tr -d '\n')"
  kubectl create secret tls "$T6_SVC-cert" -n "$T6_SYS_NS" \
    --cert="$cdir/tls.crt" --key="$cdir/tls.key" >/dev/null 2>&1 || \
    kubectl create secret tls "$T6_SVC-cert" -n "$T6_SYS_NS" \
      --cert="$cdir/tls.crt" --key="$cdir/tls.key" --dry-run=client -o yaml | kubectl apply -f - >/dev/null 2>&1

  # --- RBAC + Deployment + Service -----------------------------------------
  if ! kubectl apply -f - >"$WORK/t6-deploy.log" 2>&1 <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: { name: $T6_SVC, namespace: $T6_SYS_NS }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: { name: $T6_SVC }
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: { name: $T6_SVC }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: $T6_SVC }
subjects:
  - { kind: ServiceAccount, name: $T6_SVC, namespace: $T6_SYS_NS }
---
apiVersion: v1
kind: Service
metadata: { name: $T6_SVC, namespace: $T6_SYS_NS }
spec:
  selector: { app: $T6_SVC }
  ports:
    - { name: https, port: 443, targetPort: 9443 }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: $T6_SVC, namespace: $T6_SYS_NS }
spec:
  replicas: 1
  selector: { matchLabels: { app: $T6_SVC } }
  template:
    metadata: { labels: { app: $T6_SVC } }
    spec:
      serviceAccountName: $T6_SVC
      securityContext: { runAsNonRoot: true }
      containers:
        - name: webhook
          image: $T6_IMG
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
          secret: { secretName: $T6_SVC-cert }
YAML
  then
    fail "webhook(in-cluster): apply Deployment/Service/RBAC" "see $WORK/t6-deploy.log"; return 0
  fi

  info "tier6: waiting for the webhook Deployment to become available"
  if ! kubectl rollout status deploy/"$T6_SVC" -n "$T6_SYS_NS" --timeout=120s >"$WORK/t6-rollout.log" 2>&1; then
    kubectl describe deploy/"$T6_SVC" -n "$T6_SYS_NS" >>"$WORK/t6-rollout.log" 2>&1 || true
    kubectl logs -n "$T6_SYS_NS" -l app="$T6_SVC" --tail=50 >>"$WORK/t6-rollout.log" 2>&1 || true
    fail "webhook(in-cluster): Deployment became ready" "see $WORK/t6-rollout.log"; return 0
  fi
  pass "webhook(in-cluster): Deployment is available (readyz passing)"

  # --- register the webhook (in-cluster Service, scoped to the test ns) -----
  if ! kubectl apply -f - >"$WORK/t6-cfg.log" 2>&1 <<YAML
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: $T6_CFG
webhooks:
  - name: pods.brewlet.ic
    admissionReviewVersions: ["v1"]
    sideEffects: None
    failurePolicy: Fail
    timeoutSeconds: 10
    namespaceSelector:
      matchLabels: { brewlet-webhook-ic: "true" }
    clientConfig:
      service:
        name: $T6_SVC
        namespace: $T6_SYS_NS
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
    fail "webhook(in-cluster): register MutatingWebhookConfiguration" "see $WORK/t6-cfg.log"; return 0
  fi
  pass "webhook(in-cluster): MutatingWebhookConfiguration registered"

  # Endpoints/webhook plumbing can lag a beat behind rollout; give it a moment.
  sleep 3

  # --- run the shared assertions -------------------------------------------
  webhook_cases "$T6_NS" "webhook(in-cluster)"
  if [[ $? -eq 2 ]]; then
    fail "webhook(in-cluster): API server could not reach the in-cluster webhook" \
      "unexpected — see $WORK/t6-rollout.log / t6-cfg.log"
  fi
}
