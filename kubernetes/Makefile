GO ?= go
GOFMT ?= gofmt
HELM ?= helm
ENVTEST_K8S_VERSION ?= 1.31.0
SETUP_ENVTEST ?= $(shell $(GO) env GOPATH)/bin/setup-envtest

.PHONY: all fmt-check vet build test test-envtest ci operator-build admission-build operator-image admission-image helm-lint helm-template helm-check clean

all: build

fmt-check:
	@unformatted="$$($(GOFMT) -l $$($(GO) list -f '{{.Dir}}' ./...))"; \
	if [ -n "$$unformatted" ]; then \
		echo "The following files are not gofmt-formatted:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

vet:
	$(GO) vet ./...

build:
	$(GO) build ./...

test:
	$(GO) test -race ./...

$(SETUP_ENVTEST):
	$(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.19

test-envtest: $(SETUP_ENVTEST)
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" $(GO) test -race ./...

ci: fmt-check vet build test-envtest helm-check

operator-build:
	$(GO) build -o bin/operator ./cmd/manager

admission-build:
	$(GO) build -o bin/admission ./cmd/admission

operator-image:
	docker build -t ghcr.io/brewlet/operator:dev .

admission-image:
	docker build -t ghcr.io/brewlet/admission:dev . --build-arg CMD=admission

helm-lint:
	$(HELM) lint charts/brewlet

helm-template:
	$(HELM) template brewlet charts/brewlet >/dev/null

helm-check: helm-lint helm-template

clean:
	rm -rf bin
