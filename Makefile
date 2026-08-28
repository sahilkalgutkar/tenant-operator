# Every target here is one I actually run: there is no target in this file that
# exists only because a scaffolder generated it.

SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

IMG ?= ghcr.io/sahilkalgutkar/tenant-operator:dev
ENVTEST_K8S_VERSION ?= 1.32.0
LOCALBIN := $(shell pwd)/bin

CONTROLLER_GEN := $(LOCALBIN)/controller-gen
SETUP_ENVTEST := $(LOCALBIN)/setup-envtest
CONTROLLER_TOOLS_VERSION ?= v0.17.2
ENVTEST_VERSION ?= release-0.20

.PHONY: help
help: ## Show this help.
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

$(CONTROLLER_GEN): $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

$(SETUP_ENVTEST): $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Regenerate the deepcopy functions.
	$(CONTROLLER_GEN) object:headerFile=/dev/null paths=./api/...

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Regenerate the CRD, RBAC and webhook manifests.
	$(CONTROLLER_GEN) crd rbac:roleName=manager-role webhook paths=./... \
		output:crd:artifacts:config=config/crd \
		output:rbac:artifacts:config=config/rbac \
		output:webhook:artifacts:config=config/webhook

.PHONY: fmt
fmt: ## Format the code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: test
test: generate manifests fmt vet $(SETUP_ENVTEST) ## Run every test, including the ones that need an API server.
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
		go test -race -count=1 -timeout 10m ./...

.PHONY: cover
cover: $(SETUP_ENVTEST) ## Run the tests and report coverage.
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
		go test -covermode=atomic -coverpkg=./... -coverprofile=cover.out -timeout 10m ./...
	go tool cover -func=cover.out | tail -1

.PHONY: build
build: ## Build the manager binary.
	go build -o bin/manager ./cmd/manager

.PHONY: docker-build
docker-build: ## Build the container image.
	docker build -t $(IMG) .

.PHONY: render
render: manifests ## Render the full install manifest to stdout.
	kubectl kustomize config/default

.PHONY: install
install: manifests ## Install just the CRD into the current cluster.
	kubectl kustomize config/crd | kubectl apply -f -

.PHONY: deploy
deploy: manifests ## Install the CRD, RBAC, webhooks and the operator itself.
	kubectl kustomize config/default | kubectl apply -f -

.PHONY: undeploy
undeploy: ## Remove everything this operator installed.
	kubectl kustomize config/default | kubectl delete --ignore-not-found -f -

.PHONY: run
run: manifests generate ## Run the operator locally against the current kubecontext, without webhooks.
	go run ./cmd/manager --leader-elect=false --enable-webhooks=false --metrics-bind-address=0 --health-probe-bind-address=:8081
