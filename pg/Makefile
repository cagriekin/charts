CLUSTER_NAME ?= pg-test
KIND_CONFIG := kind-config.yaml
TESTS_DIR := tests

.PHONY: test test-template test-cluster test-minimal test-repmgr test-full test-failover test-upgrade \
        cluster-create cluster-delete cluster-exists clean help

help:
	@echo "Targets:"
	@echo "  test            - Run all tests (creates cluster, runs tests, deletes cluster)"
	@echo "  test-template   - Run helm template/lint tests (no cluster needed)"
	@echo "  test-cluster    - Run all cluster tests (assumes cluster exists)"
	@echo "  test-minimal    - Run minimal install tests"
	@echo "  test-repmgr     - Run repmgr install tests"
	@echo "  test-full       - Run full install tests"
	@echo "  test-failover   - Run failover tests (requires test-repmgr first)"
	@echo "  test-upgrade    - Run upgrade path tests"
	@echo "  cluster-create  - Create Kind cluster"
	@echo "  cluster-delete  - Delete Kind cluster"
	@echo "  clean           - Delete cluster and test namespaces"

test: cluster-create test-template test-cluster cluster-delete

test-template:
	@bash $(TESTS_DIR)/test-template.sh

test-cluster: test-minimal test-repmgr test-failover test-full test-upgrade

cluster-exists:
	@kind get clusters 2>/dev/null | grep -q "^$(CLUSTER_NAME)$$" || (echo "Cluster '$(CLUSTER_NAME)' not found. Run: make cluster-create" && exit 1)

cluster-create:
	@if kind get clusters 2>/dev/null | grep -q "^$(CLUSTER_NAME)$$"; then \
		if kubectl cluster-info --context kind-$(CLUSTER_NAME) >/dev/null 2>&1; then \
			echo "Cluster '$(CLUSTER_NAME)' already exists and is reachable"; \
		else \
			echo "Cluster '$(CLUSTER_NAME)' exists but is unreachable, recreating..."; \
			kind delete cluster --name $(CLUSTER_NAME); \
			kind create cluster --name $(CLUSTER_NAME) --config $(KIND_CONFIG); \
		fi; \
	else \
		kind create cluster --name $(CLUSTER_NAME) --config $(KIND_CONFIG); \
	fi
	@kubectl cluster-info --context kind-$(CLUSTER_NAME)

cluster-delete:
	@kind delete cluster --name $(CLUSTER_NAME) 2>/dev/null || true

test-minimal: cluster-exists
	@bash $(TESTS_DIR)/test-minimal.sh

test-repmgr: cluster-exists
	@bash $(TESTS_DIR)/test-repmgr.sh

test-full: cluster-exists
	@bash $(TESTS_DIR)/test-full.sh

test-failover: cluster-exists
	@bash $(TESTS_DIR)/test-failover.sh

test-upgrade: cluster-exists
	@bash $(TESTS_DIR)/test-upgrade.sh

clean: cluster-delete
	@echo "Cleaned up"
