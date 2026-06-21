CLUSTER_NAME ?= pg-test
KIND_CONFIG := kind-config.yaml
TESTS_DIR := tests
MAKEFLAGS += --output-sync=target

.PHONY: test test-template test-cluster test-minimal test-repmgr test-repmgr-chaos test-full test-failover test-upgrade \
        test-repmgr-failover test-agent test-agent-etcd test-agent-etcd-tls test-tls test-pgpool-failover test-config-failover test-cascading-replication test-migrate-agent test-scaledown test-agent-rolling test-config test-config-repmgr test-backup-restore test-backup-concurrent test-special-chars byte-stable cluster-create cluster-delete cluster-exists clean help

help:
	@echo "Targets:"
	@echo "  test            - Run all tests (creates cluster, runs tests, deletes cluster)"
	@echo "  test-template   - Run helm template/lint tests (no cluster needed)"
	@echo "  test-cluster    - Run all cluster tests in parallel (assumes cluster exists)"
	@echo "  test-minimal    - Run minimal install tests"
	@echo "  test-repmgr     - Run repmgr install tests"
	@echo "  test-repmgr-chaos - Run standby pod chaos restart regression test"
	@echo "  test-full       - Run full install tests"
	@echo "  test-failover   - Run failover tests (requires test-repmgr first)"
	@echo "  test-agent      - Run lease-based agent install + failover tests"
	@echo "  test-agent-etcd - Run agent etcd-DCS test (bundled etcd; standalone, opt-in)"
	@echo "  test-agent-etcd-tls - Run agent etcd-DCS test over mutual TLS + RBAC (standalone, opt-in)"
	@echo "  test-tls           - Run client-connection TLS test (PostgreSQL+PGPool+exporter, standalone, opt-in)"
	@echo "  test-pgpool-failover - Run PGPool failover + health-check test (standalone, opt-in)"
	@echo "  test-config-failover - Run config-persistence-across-failover test (standalone, opt-in)"
	@echo "  test-cascading-replication - Run cascading-replication chain + re-homing test (standalone, opt-in)"
	@echo "  test-migrate-agent - Run repmgrd->agent migration test (standalone, long)"
	@echo "  test-upgrade    - Run upgrade path tests"
	@echo "  test-config     - Run postgresql configuration tests (standalone)"
	@echo "  test-config-repmgr - Run postgresql configuration tests (repmgr)"
	@echo "  test-backup-restore - Run backup and restore integration tests"
	@echo "  test-backup-concurrent - Run concurrent pgbackrest backup + WAL archiving test"
	@echo "  test-special-chars - Run special-character credential tests"
	@echo "  cluster-create  - Create Kind cluster"
	@echo "  cluster-delete  - Delete Kind cluster"
	@echo "  clean           - Delete cluster and test namespaces"

test: cluster-create test-template test-cluster cluster-delete

test-template:
	@bash $(TESTS_DIR)/test-template.sh

# Diff the repmgrd (default) render of pg+pgvector vs a baseline ref (default
# origin/master) to confirm the default path stays byte-stable. Intended bumps show
# as a diff to review; not a hard gate. Usage: make byte-stable [REF=<ref>]
byte-stable:
	@bash ../scripts/check-repmgrd-byte-stable.sh $(REF)

# Run independent suites in parallel with -j
# test-repmgr-failover is sequential (repmgr then failover)
# test-minimal, test-repmgr-failover, test-full, test-upgrade run in parallel
test-cluster: test-minimal test-repmgr-failover test-agent test-repmgr-chaos test-full test-upgrade test-scaledown test-agent-rolling test-config test-config-repmgr test-backup-restore test-special-chars
	@echo "All test suites passed"

# Chain: repmgr install -> failover test (must be sequential)
test-repmgr-failover: cluster-exists
	@bash $(TESTS_DIR)/test-repmgr.sh
	@bash $(TESTS_DIR)/test-failover.sh

# Chain: agent install -> agent failover test (must be sequential; the failover
# suite reads the lease-elected primary/standby that the install suite records)
test-agent: cluster-exists
	@bash $(TESTS_DIR)/test-agent.sh
	@bash $(TESTS_DIR)/test-agent-failover.sh

# etcd DCS backend (bundled etcd). Standalone + opt-in: deploys 3 extra etcd pods
# and is not yet validated in CI, so it is NOT part of test-cluster. Run explicitly
# in a clean cluster once verified, then wire into CI.
test-agent-etcd: cluster-exists
	@bash $(TESTS_DIR)/test-agent-etcd.sh

test-agent-etcd-tls: cluster-exists
	@bash $(TESTS_DIR)/test-agent-etcd-tls.sh

test-tls: cluster-exists
	@bash $(TESTS_DIR)/test-tls.sh

test-pgpool-failover: cluster-exists
	@bash $(TESTS_DIR)/test-pgpool-failover.sh

test-config-failover: cluster-exists
	@bash $(TESTS_DIR)/test-config-failover.sh

test-cascading-replication: cluster-exists
	@bash $(TESTS_DIR)/test-cascading-replication.sh

# repmgrd -> agent migration (--cascade=orphan recreate). Standalone + long-running
# (installs repmgrd, then rolls to agent), so not in test-cluster; run explicitly.
test-migrate-agent: cluster-exists
	@bash $(TESTS_DIR)/test-migrate-agent.sh

# Scale replicaCount down and assert the primary unregisters the ghost repmgr.nodes
# record (#139). Uses repmgrd mode for a deterministic standby ghost.
test-scaledown: cluster-exists
	@bash $(TESTS_DIR)/test-scaledown.sh

# Rolling-restart a 2-node agent cluster and assert it converges to a single writable
# primary with the standby re-streaming, no deadlock/manual intervention (#186).
test-agent-rolling: cluster-exists
	@bash $(TESTS_DIR)/test-agent-rolling.sh

cluster-exists:
	@kind get clusters 2>/dev/null | grep -q "^$(CLUSTER_NAME)$$" || (echo "Cluster '$(CLUSTER_NAME)' not found. Run: make cluster-create" && exit 1)
	@kubectl config use-context kind-$(CLUSTER_NAME) >/dev/null 2>&1

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

test-repmgr-chaos: cluster-exists
	@bash $(TESTS_DIR)/test-repmgr-chaos.sh

test-full: cluster-exists
	@bash $(TESTS_DIR)/test-full.sh

test-failover: cluster-exists
	@bash $(TESTS_DIR)/test-failover.sh

test-upgrade: cluster-exists
	@bash $(TESTS_DIR)/test-upgrade.sh

test-config: cluster-exists
	@bash $(TESTS_DIR)/test-config.sh

test-config-repmgr: cluster-exists
	@bash $(TESTS_DIR)/test-config-repmgr.sh

test-backup-restore: cluster-exists
	@bash $(TESTS_DIR)/test-backup-restore.sh

test-backup-concurrent: cluster-exists
	@bash $(TESTS_DIR)/test-backup-concurrent.sh

test-special-chars: cluster-exists
	@bash $(TESTS_DIR)/test-special-chars.sh

clean: cluster-delete
	@echo "Cleaned up"
