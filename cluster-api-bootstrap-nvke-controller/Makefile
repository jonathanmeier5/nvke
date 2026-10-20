# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

LOCALBIN ?= $(CURDIR)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
include ../make/tool-versions.mk

.DEFAULT_GOAL := help
.PHONY: help build test fmt vet controller-gen generate manifests generate-check

help: ## Show the available targets.
	@awk 'BEGIN { FS = ":.*## " } /^[a-zA-Z0-9_-]+:.*## / { printf "  %-18s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Build the manager binary.
	go build -o bin/manager ./cmd/manager

test: ## Run this module's tests.
	go test ./...

fmt: ## Format the source.
	go fmt ./...

vet: ## Vet the source.
	go vet ./...

controller-gen: ## Install the pinned controller-gen binary.
	@if [ "$$($(CONTROLLER_GEN) --version 2>/dev/null)" != "Version: $(CONTROLLER_TOOLS_VERSION)" ]; then \
		mkdir -p "$(LOCALBIN)"; \
		GOBIN="$(LOCALBIN)" go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION); \
	fi

generate: controller-gen ## Generate DeepCopy implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

manifests: controller-gen ## Generate CRDs and controller RBAC.
	"$(CONTROLLER_GEN)" \
		crd:crdVersions=v1,headerFile="hack/boilerplate.yaml.txt" \
		paths="./api/..." \
		output:crd:artifacts:config=config/crd/bases
	"$(CONTROLLER_GEN)" \
		rbac:roleName=nvke-bootstrap-controller-manager,fileName=role.yaml,headerFile="hack/boilerplate.yaml.txt" \
		paths="./internal/..." \
		output:rbac:artifacts:config=config/rbac
	@# Cluster API resolves a provider's API version from this CRD label; without
	@# it the Machine controller cannot tell which contract the CRD speaks and
	@# falls back to the deprecated status.ready gate. controller-gen does not
	@# emit it, so it is stamped on here rather than hand-edited after every run.
	@for f in config/crd/bases/*.yaml; do \
		grep -q 'cluster.x-k8s.io/v1beta2' "$$f" || \
			python3 -c "import sys,io; p=sys.argv[1]; s=io.open(p).read(); i=s.index('metadata:\n'); io.open(p,'w').write(s[:i]+'metadata:\n  labels:\n    cluster.x-k8s.io/v1beta2: v1alpha1\n'+s[i+10:])" "$$f"; \
	done

generate-check: generate manifests ## Fail when the generated output is stale.
	@git diff --exit-code -- api config || { echo "generated output is stale; run 'make generate manifests'"; exit 1; }
