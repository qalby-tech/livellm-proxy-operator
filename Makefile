IMG ?= livellm-proxy-operator:latest

# Tool versions
CONTROLLER_GEN_VERSION ?= v0.20.1

# Local tool binaries
LOCALBIN ?= $(shell pwd)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen

.PHONY: build run docker-build deploy undeploy fmt vet tidy generate manifests install-crd uninstall-crd controller-gen

## ────────────────────────────────────────────────────────
## Build
## ────────────────────────────────────────────────────────

build: fmt vet
	go build -o bin/operator .

run: fmt vet
	go run . --leader-elect=false

fmt:
	go fmt ./...

vet:
	go vet ./...

tidy:
	go mod tidy

## ────────────────────────────────────────────────────────
## Code generation  (run after changing api/ types)
## ────────────────────────────────────────────────────────

# Generate DeepCopy implementations  →  api/v1/zz_generated.deepcopy.go
generate: controller-gen
	$(CONTROLLER_GEN) object paths="./api/..."

# Generate CRD manifests from Go types  →  deploy/crd.yaml
manifests: controller-gen
	@rm -rf /tmp/crd-staging && mkdir -p /tmp/crd-staging
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:artifacts:config=/tmp/crd-staging
	@# controller-gen produces one file per CRD — merge into a single crd.yaml
	@cat /tmp/crd-staging/*.yaml > deploy/crd.yaml
	@rm -rf /tmp/crd-staging

# Shorthand: regenerate everything
gen: generate manifests
	@echo "✓  DeepCopy + CRD regenerated"

## ────────────────────────────────────────────────────────
## Docker
## ────────────────────────────────────────────────────────

docker-build:
	docker build -t $(IMG) .

## ────────────────────────────────────────────────────────
## Deploy
## ────────────────────────────────────────────────────────

deploy:
	kubectl apply -k deploy/

undeploy:
	kubectl delete -k deploy/ --ignore-not-found

install-crd:
	kubectl apply -f deploy/crd.yaml

uninstall-crd:
	kubectl delete -f deploy/crd.yaml --ignore-not-found

## ────────────────────────────────────────────────────────
## Tool installation
## ────────────────────────────────────────────────────────

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

controller-gen: $(LOCALBIN)
	@test -s $(CONTROLLER_GEN) || \
		GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)