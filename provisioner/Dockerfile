# brewlet-node-provisioner image.
#
# Multi-stage build:
#   1. Compile the containerd-shim-brewlet-v2 binary from the core module for the
#      target architecture (so the shim installed on the node always matches the
#      node arch — the JAR is arch-neutral, the shim is not).
#   2. Assemble a small runtime image containing the entrypoint script plus the
#      host-mutation tooling it needs: bash, curl, kubectl, and the containerd
#      `ctr` client (used for copy-from-image against the host containerd socket).
#
# Build (single arch, native):   docker build -t ghcr.io/brewlet/node-provisioner:dev -f provisioner/Dockerfile .
# Build (multi-arch, buildx):    docker buildx build --platform linux/amd64,linux/arm64 ...
#
# The build context is the repository root, where the Go module lives.

# ---- stage 1: build the shim for $TARGETARCH -------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS shim
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' \
    -o /out/containerd-shim-brewlet-v2 ./shim/cmd/containerd-shim-brewlet-v2

# ---- stage 2: runtime image ------------------------------------------------
FROM debian:bookworm-slim
ARG TARGETARCH
ARG KUBECTL_VERSION=v1.31.0
ARG CONTAINERD_VERSION=2.0.5

# Host-mutation tooling: ca-certificates for TLS, curl to fetch kubectl at build
# time, kubectl to label the node, and the containerd `ctr` client used to copy
# JDK/launcher trees out of vendor images via the host containerd socket
# (copy-from-image). The socket is bind-mounted from the host at runtime; the
# `ctr` client itself is baked in here so it is always present and version-matched.
RUN apt-get update \
 && apt-get install -y --no-install-recommends bash curl ca-certificates procps \
 && curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
      -o /usr/local/bin/kubectl \
 && chmod 0755 /usr/local/bin/kubectl \
 && curl -fsSL "https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-${TARGETARCH}.tar.gz" \
      | tar -xz -C /usr/local bin/ctr \
 && chmod 0755 /usr/local/bin/ctr \
 && apt-get clean && rm -rf /var/lib/apt/lists/*

# The shim binary the provisioner installs onto the node.
COPY --from=shim /out/containerd-shim-brewlet-v2 /opt/brewlet-dist/containerd-shim-brewlet-v2

COPY provisioner/entrypoint.sh /usr/local/bin/brewlet-provision
RUN chmod 0755 /usr/local/bin/brewlet-provision

ENTRYPOINT ["/usr/local/bin/brewlet-provision"]
