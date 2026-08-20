# brewlet-operator / brewlet-admission image.
#
# Multi-stage: build one of the module's binaries for the target arch, then drop
# it into a distroless static base. Which command is built is selected by the CMD
# build-arg (default: manager). Build context is the repository root.
#
#   docker build -t ghcr.io/brewlet/operator:dev .
#   docker build -t ghcr.io/brewlet/admission:dev . --build-arg CMD=admission

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
ARG TARGETARCH
ARG CMD=manager
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' -o /out/entry ./cmd/${CMD}

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /out/entry /entry
USER 65532:65532
ENTRYPOINT ["/entry"]
