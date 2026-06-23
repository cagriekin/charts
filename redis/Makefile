CLUSTER_NAME ?= redis-test
KIND_CONFIG := kind-config.yaml
TESTS_DIR := tests
MAKEFLAGS += --output-sync=target

.PHONY: test test-template test-cluster test-minimal test-full test-persistence test-replication test-failover test-ha test-tls test-acl cluster-create cluster-delete cluster-exists clean help

help:
	@echo "Targets:"
	@echo "  test            - Run all tests (creates cluster, runs tests, deletes cluster)"
	@echo "  test-template   - Run helm template/lint tests (no cluster needed)"
	@echo "  test-cluster    - Run all cluster tests in parallel (assumes cluster exists)"
	@echo "  test-minimal    - Run minimal install tests (standalone)"
	@echo "  test-full       - Run full install tests (standalone + exporter)"
	@echo "  test-persistence - Run AOF persistence restart tests (standalone)"
	@echo "  test-ha         - Run replication + Sentinel failover tests (sequential)"
	@echo "  test-replication - Run replication topology tests"
	@echo "  test-failover   - Run Sentinel failover tests (needs an existing replication release)"
	@echo "  test-tls        - Run replication-over-TLS smoke (opt-in; generates a self-signed cert)"
	@echo "  test-acl        - Run ACL tests (standalone restricted user + replication locked default)"
	@echo "  cluster-create  - Create Kind cluster"
	@echo "  cluster-delete  - Delete Kind cluster"
	@echo "  clean           - Delete cluster and test namespaces"

test: cluster-create test-template test-cluster cluster-delete

test-template:
	@bash $(TESTS_DIR)/test-template.sh

test-cluster: test-minimal test-full test-persistence test-ha test-acl
	@echo "All test suites passed"

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

test-full: cluster-exists
	@bash $(TESTS_DIR)/test-full.sh

test-persistence: cluster-exists
	@bash $(TESTS_DIR)/test-persistence.sh

# Failover reuses the replication release, so run them in order within one recipe.
test-ha: cluster-exists
	@bash $(TESTS_DIR)/test-replication.sh
	@bash $(TESTS_DIR)/test-failover.sh

test-replication: cluster-exists
	@bash $(TESTS_DIR)/test-replication.sh

test-failover: cluster-exists
	@bash $(TESTS_DIR)/test-failover.sh

test-tls: cluster-exists
	@bash $(TESTS_DIR)/test-tls.sh

test-acl: cluster-exists
	@bash $(TESTS_DIR)/test-acl.sh

clean: cluster-delete
	@echo "Cleaned up"
