# Makefile for sds-object e2e tests.

.PHONY: help
help: ## Show help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-28s\033[0m %s\n", $$1, $$2}'

.PHONY: deps
deps: ## Download dependencies
	go mod download
	go mod tidy

.PHONY: test
test: ## Run the full nested-cluster E2E suite
	go test -v -count=1 -timeout 120m ./tests/ -ginkgo.timeout 120m

.PHONY: test-focus
test-focus: ## Run a focused Ginkgo spec (make test-focus FOCUS="reaches Ready")
	@if [ -z "$(FOCUS)" ]; then \
		echo "Error: specify FOCUS=<Ginkgo focus string>"; \
		exit 1; \
	fi
	go test -v -count=1 -timeout 120m ./tests/ -ginkgo.timeout 120m -ginkgo.focus "$(FOCUS)"

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: build
build: ## Compile-check the suite (no cluster)
	go build ./...
	go test -c -o /dev/null ./tests/

.PHONY: lint
lint: ## golangci-lint (if installed)
	golangci-lint run ./...

.PHONY: check-env
check-env: ## Print relevant env vars for the nested flow
	@echo "=== Nested cluster (storage-e2e) ==="
	@echo "TEST_CLUSTER_CREATE_MODE:        $${TEST_CLUSTER_CREATE_MODE}  # alwaysCreateNew | alwaysUseExisting | commander"
	@echo "TEST_CLUSTER_CLEANUP:            $${TEST_CLUSTER_CLEANUP}      # set true to delete VMs after the run"
	@echo "TEST_CLUSTER_NAMESPACE:          $${TEST_CLUSTER_NAMESPACE}    # also the in-cluster namespace for OB/Secret/probe"
	@echo "TEST_CLUSTER_STORAGE_CLASS:      $${TEST_CLUSTER_STORAGE_CLASS}  # base-cluster SC for the VM OS disks"
	@echo "SSH_HOST:                        $${SSH_HOST}"
	@echo "SSH_USER:                        $${SSH_USER}"
	@echo "SSH_PRIVATE_KEY:                 $${SSH_PRIVATE_KEY}"
	@echo "SSH_PUBLIC_KEY:                  $${SSH_PUBLIC_KEY}  # required for alwaysCreateNew (VM authorized key)"
	@echo "SSH_VM_USER:                     $${SSH_VM_USER}  # required for alwaysCreateNew (SSH user inside the VMs)"
	@echo "DKP_LICENSE_KEY:                 $${DKP_LICENSE_KEY:+<set>}"
	@echo "REGISTRY_DOCKER_CFG:             $${REGISTRY_DOCKER_CFG:+<set>}"
	@echo "SDS_OBJECT_MODULE_PULL_OVERRIDE: $${SDS_OBJECT_MODULE_PULL_OVERRIDE}  # overrides modulePullOverride (default main)"
	@echo "YAML_CONFIG_FILENAME:            $${YAML_CONFIG_FILENAME}  # default cluster_config.yml"
	@echo ""
	@echo "=== sds-object suite knobs ==="
	@echo "E2E_OSC_NAME:                    $${E2E_OSC_NAME}"
	@echo "E2E_OSC_TYPE:                    $${E2E_OSC_TYPE}  # System | Lightweight | Full | Heavy (default System)"
	@echo "E2E_REDUNDANCY:                  $${E2E_REDUNDANCY}  # Single | Replicated | HighRedundancy"
	@echo "E2E_STORAGE_CLASS:               $${E2E_STORAGE_CLASS}  # required for Lightweight/Full"
	@echo "E2E_OSC_SIZE:                    $${E2E_OSC_SIZE}"
	@echo "E2E_ELASTIC_CLUSTER_REF:         $${E2E_ELASTIC_CLUSTER_REF}  # required for Heavy"
	@echo "E2E_BUCKET_NAME:                 $${E2E_BUCKET_NAME}"
	@echo "E2E_OSC_READY_TIMEOUT:           $${E2E_OSC_READY_TIMEOUT}"
	@echo "E2E_OB_READY_TIMEOUT:            $${E2E_OB_READY_TIMEOUT}"
	@echo "E2E_MODULE_READY_TIMEOUT:        $${E2E_MODULE_READY_TIMEOUT}"
	@echo "E2E_PROBE_IMAGE:                 $${E2E_PROBE_IMAGE}  # image with mc for the S3 round-trip job"
	@echo "E2E_PROBE_JOB_TIMEOUT:           $${E2E_PROBE_JOB_TIMEOUT}"
	@echo "E2E_KEEP_CLUSTER_ON_FAILURE:     $${E2E_KEEP_CLUSTER_ON_FAILURE}  # true|1|yes -> skip teardown if a spec failed"

.DEFAULT_GOAL := help
