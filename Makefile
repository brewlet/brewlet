SHELL := /bin/bash

HOST_ARCH := $(shell uname -m)
ifneq (,$(filter $(HOST_ARCH),arm64 aarch64))
  ARCH ?= arm64
else
  ARCH ?= amd64
endif

REGISTRY ?= ghcr.io/brewlet
TAG ?= latest
PROVISIONER_IMAGE ?= $(REGISTRY)/node-provisioner:$(TAG)

.PHONY: build binaries test vet fmt-check check appcds-verify provisioner-image provisioner-image-push clean

build: ## Build every package for the current platform
	go build ./...

binaries: ## Build the CLI and containerd shim into bin/
	mkdir -p bin
	go build -o ./bin/brewlet ./cmd/brewlet
	go build -o ./bin/containerd-shim-brewlet-v2 ./shim/cmd/containerd-shim-brewlet-v2

test: ## Run all tests with the race detector
	go test -race ./...
	bash provisioner/entrypoint_test.sh

vet: ## Run Go static analysis
	go vet ./...

fmt-check: ## Fail when tracked Go source is not gofmt-formatted
	@unformatted="$$(gofmt -l $$(go list -f '{{.Dir}}' ./...))"; \
	if [[ -n "$$unformatted" ]]; then \
		echo "The following files are not gofmt-formatted:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

check: fmt-check vet build test ## Run all CI checks

appcds-verify: ## Run the AppCDS JDK integration test (requires a full JDK 17+)
	go test -v -run TestAppCDSTrainThenMapIntegration ./internal/runtime/

provisioner-image: ## Build the node-provisioner image for the host architecture
	docker build --platform linux/$(ARCH) -t $(PROVISIONER_IMAGE) -f provisioner/Dockerfile .

provisioner-image-push: ## Build and push the multi-architecture node-provisioner image
	docker buildx build --platform linux/amd64,linux/arm64 \
		-t $(PROVISIONER_IMAGE) -f provisioner/Dockerfile --push .

clean: ## Remove local build and runtime outputs
	rm -rf bin oci bundle
