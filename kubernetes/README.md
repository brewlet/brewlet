# Brewlet Kubernetes platform

[![CI](https://github.com/brewlet/brewlet/actions/workflows/ci.yml/badge.svg)](https://github.com/brewlet/brewlet/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/brewlet/brewlet)](./LICENSE)

This directory contains the Kubernetes-facing components of Brewlet:

- the node lifecycle and `JavaApplication` controllers;
- the pod and `NodeProfile` admission webhook;
- the `NodeProfile` and `JavaApplication` API types and CRDs;
- raw Kubernetes deployment manifests; and
- the Brewlet Helm chart.

The runtime shim and node provisioner source live at the monorepo root.
Architecture and API specifications live in [`specs/`](../specs), and the user
documentation lives in [`brewlet/site`](https://github.com/brewlet/site).

## Install with Helm

```bash
helm install brewlet ./kubernetes/charts/brewlet \
  --set provisioner.jdks="temurin-21,microsoft-25" \
  --set provisioner.launchers="jaz"
```

The chart installs the CRDs, operator, admission webhook, and the RBAC used by
the operator-managed node provisioner. The default `NodeProfile` targets every
unclaimed node. Configure named profiles when provisioning should be restricted
to platform-owned node pools.

```bash
kubectl get nodeprofiles
kubectl get nodes -L brewlet.sh/runtime
```

See the [Brewlet installation guide](https://github.com/brewlet/site) for
cluster prerequisites and production configuration.

### Custom JDK distributions

`temurin` and `microsoft` use built-in image mappings. For another distribution,
provide its fully qualified OCI image and Java home directly in the
`NodeProfile`. For example, Azul Zulu 21:

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: zulu
spec:
  nodePool:
    names: ["zulu-workers"]
  jdks:
    - distribution: zulu
      feature: 21
      source:
        image: docker.io/library/azul-zulu:21
        javaHome: /usr/lib/jvm/zulu21
```

Use a digest-pinned image in production. The image must support every
architecture in the selected pool, contain `<javaHome>/bin/java`, and provide the
runtime's required userland libraries. `javaHome` may point to a centrally built
jlink runtime; Brewlet installs it once per node pool rather than placing it in
each application artifact.

## Install raw manifests

```bash
kubectl apply -f kubernetes/deploy/nodeprofile-crd.yaml
kubectl apply -f kubernetes/deploy/javaapplication-crd.yaml
kubectl apply -f kubernetes/deploy/node-provisioner.yaml
kubectl apply -f kubernetes/config/operator.yaml
kubectl apply -f kubernetes/deploy/sample-nodeprofile.yaml
```

The raw manifests use these images:

| Component | Image |
|---|---|
| Operator | `ghcr.io/brewlet/operator` |
| Admission webhook | `ghcr.io/brewlet/admission` |
| Node provisioner | `ghcr.io/brewlet/node-provisioner` |

## Build and test

Run component checks from the monorepo root:

```bash
make -C kubernetes build
make -C kubernetes test
make -C kubernetes test-envtest
make -C kubernetes helm-check
```

Build the component images with `kubernetes/` as the Docker context:

```bash
docker build -t ghcr.io/brewlet/operator:dev kubernetes
docker build -t ghcr.io/brewlet/admission:dev kubernetes --build-arg CMD=admission
```

## Component layout

```text
.
├── api/                 NodeProfile and JavaApplication API types
├── charts/brewlet/      Helm chart and packaged CRDs
├── cmd/manager/         Kubernetes operator
├── cmd/admission/       Admission webhook
├── config/              Raw operator RBAC and Deployment
├── deploy/              CRDs, samples, and raw manifests
└── internal/            Controllers, admission logic, and tests
```

## License

[MIT](./LICENSE)
