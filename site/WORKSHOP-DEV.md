# Part 2: Build and deploy a Brewlet workload

**Audience:** Java application developers.

**Goal:** build a JAR, publish it as a runnable OCI image, and deploy it to a
Brewlet-enabled Kubernetes cluster without creating a Dockerfile or bundling a
JDK.

## 1. Receive the platform handoff

Ask the Ops participant for:

```bash
export BREWLET_CONTEXT="<kubernetes-context>"
export BREWLET_NAMESPACE="<developer-namespace>"
export BREWLET_JDK="21"
export BREWLET_VERSION="0.1.0"
export BREWLET_REGISTRY="<registry-host>/<team>"
```

You also need JDK 21+, Maven 3.9+, `kubectl`, `curl`, registry push credentials,
and the example source from the matching Brewlet release:

```bash
git clone --depth 1 --branch "v${BREWLET_VERSION}" \
  https://github.com/brewlet/brewlet.git
cd brewlet
```

The registry repository must be readable by the cluster nodes.

Select the provided context and confirm your access:

```bash
kubectl config use-context "$BREWLET_CONTEXT"
kubectl auth can-i create deployments -n "$BREWLET_NAMESPACE"
kubectl auth can-i create javaapplications.apps.brewlet.sh -n "$BREWLET_NAMESPACE"
kubectl get runtimeclass brewlet
```

Install the released CLI and run the same readiness check used by Ops:

```bash
curl -fsSL https://brewlet.sh/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"

brewlet doctor \
  --context "$BREWLET_CONTEXT" \
  --namespace "$BREWLET_NAMESPACE"
```

If your RBAC permits reading nodes, you can also inspect the available JDKs:

```bash
kubectl get nodes -l brewlet.sh/runtime=ready \
  -o custom-columns=NAME:.metadata.name,JDKS:.metadata.annotations.brewlet\\.sh/jdks
```

## 2. Build the example JAR

The included dependency-free application exposes `/hello`, `/healthz`, and
`/info`.

```bash
mvn -f integration-tests/fixtures/demo-app/pom.xml clean package
jar --describe-module \
  --file integration-tests/fixtures/demo-app/target/app.jar
```

The output is an ordinary executable JAR. It contains neither Linux nor a JDK.

## 3. Install and exercise the released Maven plugin

The plugin is attached to the GitHub release while Maven Central publication is
being established. Install the released JAR and POM into your local Maven
repository:

```bash
mkdir -p target/brewlet-release
curl -fL \
  -o target/brewlet-release/brewlet-maven-plugin.jar \
  "https://github.com/brewlet/brewlet/releases/download/v${BREWLET_VERSION}/brewlet-maven-plugin-${BREWLET_VERSION}.jar"
curl -fL \
  -o target/brewlet-release/brewlet-maven-plugin.pom \
  "https://github.com/brewlet/brewlet/releases/download/v${BREWLET_VERSION}/brewlet-maven-plugin-${BREWLET_VERSION}.pom"

mvn org.apache.maven.plugins:maven-install-plugin:3.1.4:install-file \
  -Dfile=target/brewlet-release/brewlet-maven-plugin.jar \
  -DpomFile=target/brewlet-release/brewlet-maven-plugin.pom
```

Build a registry-free runnable OCI layout first:

```bash
mvn -f integration-tests/fixtures/demo-app/pom.xml \
  "sh.brewlet:brewlet-maven-plugin:${BREWLET_VERSION}:config" \
  "sh.brewlet:brewlet-maven-plugin:${BREWLET_VERSION}:build" \
  -Dbrewlet.image=demo/hello:workshop

test -f integration-tests/fixtures/demo-app/target/brewlet/jvm-config.json
test -f integration-tests/fixtures/demo-app/target/brewlet/oci/index.json
```

The image layout contains standard OCI layers for `amd64` and `arm64`. Its image
config carries a Brewlet launch contract; it does not contain a base image.

## 4. Publish the application

Choose a unique tag and push it to the registry supplied by Ops:

```bash
export IMAGE="$BREWLET_REGISTRY/hello:$(date +%Y%m%d%H%M%S)"

mvn -f integration-tests/fixtures/demo-app/pom.xml \
  "sh.brewlet:brewlet-maven-plugin:${BREWLET_VERSION}:push" \
  -Dbrewlet.image="$IMAGE"
```

Use your normal registry login mechanism before this command. Private registry
authentication can be configured with `spec.artifact.pullSecrets`; this
workshop assumes the nodes can read the selected repository directly.

## 5. Deploy with `JavaApplication`

```bash
cat <<EOF | kubectl apply -n "$BREWLET_NAMESPACE" -f -
apiVersion: apps.brewlet.sh/v1alpha1
kind: JavaApplication
metadata:
  name: hello
spec:
  artifact:
    image: ${IMAGE}
  jvm:
    version: ${BREWLET_JDK}
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
    limits:
      cpu: "1"
      memory: 256Mi
  ports:
    - name: http
      containerPort: 8080
  service:
    enabled: true
    type: ClusterIP
  probes:
    readiness:
      httpGet:
        path: /healthz
        port: 8080
EOF
```

Watch the higher-level resource and the Kubernetes objects it owns:

```bash
kubectl get javaapplication,deployment,pod,service -n "$BREWLET_NAMESPACE"
kubectl rollout status deployment/hello -n "$BREWLET_NAMESPACE" --timeout=5m
kubectl logs deployment/hello -n "$BREWLET_NAMESPACE"
```

The generated pod uses `runtimeClassName: brewlet`. The node-resident JDK
launches the JAR directly under the pod's CPU and memory cgroups.

## 6. Call the application

In one terminal:

```bash
kubectl port-forward service/hello 8080:80 -n "$BREWLET_NAMESPACE"
```

In another:

```bash
curl http://127.0.0.1:8080/hello
curl http://127.0.0.1:8080/info
```

The `/info` response shows the selected Java runtime and the CPU and memory
limits observed by the JVM.

## 7. Make and deploy a change

Change the response in
`integration-tests/fixtures/demo-app/src/com/example/Hello.java`, choose a new
tag, then repeat the package, push, and apply steps. Kubernetes rolls out the new
artifact like any other application update.

## Cleanup

```bash
kubectl delete javaapplication hello -n "$BREWLET_NAMESPACE"
```

This removes the generated Deployment and Service but leaves the shared Brewlet
platform intact. See [Building and publishing](docs/building-and-publishing.md)
and [Deploying workloads](docs/deploying-workloads.md) for production options.
