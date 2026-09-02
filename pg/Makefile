CLUSTER_NAME ?= pg-test
KIND_CONFIG := kind-config.yaml
TESTS_DIR := tests
MAKEFLAGS += --output-sync=target

.PHONY: test test-template test-cluster test-minimal test-full test-upgrade \
        test-agent test-agent-control test-agent-control-restore test-agent-etcd test-agent-etcd-tls test-tls test-pgpool-failover test-config-failover test-cascading-replication test-sync-replication-slots test-scaledown test-agent-rolling test-config test-backup-restore test-backup-concurrent test-pgbackrest-restore test-pgbackrest-restore-ha test-pgbackrest-bootstrap test-databases-roles test-special-chars test-migrate-native test-migrate-repmgrd byte-stable cluster-create cluster-delete cluster-exists clean help

help:
	@echo "Targets:"
	@echo "  test            - Run all tests (creates cluster, runs tests, deletes cluster)"
	@echo "  test-template   - Run helm template/lint tests (no cluster needed)"
	@echo "  test-cluster    - Run all cluster tests in parallel (assumes cluster exists)"
	@echo "  test-minimal    - Run minimal install tests"
	@echo "  test-full       - Run full install tests"
	@echo "  test-agent      - Run lease-based agent install + failover tests"
	@echo "  test-agent-control - Run agent control REST API tests (mTLS, pause, switchover)"
	@echo "  test-agent-control-restore - Run API-driven pgBackRest restore tests"
	@echo "  test-agent-etcd - Run agent etcd-DCS test (bundled etcd; standalone, opt-in)"
	@echo "  test-agent-etcd-tls - Run agent etcd-DCS test over mutual TLS + RBAC (standalone, opt-in)"
	@echo "  test-tls           - Run client-connection TLS test (PostgreSQL+PGPool+exporter, standalone, opt-in)"
	@echo "  test-pgpool-failover - Run PGPool failover + health-check test (standalone, opt-in)"
	@echo "  test-config-failover - Run config-persistence-across-failover test (standalone, opt-in)"
	@echo "  test-cascading-replication - Run cascading-replication chain + re-homing test (standalone, opt-in)"
	@echo "  test-sync-replication-slots - Run logical replication slot sync test (#308, standalone, opt-in)"
	@echo "  test-upgrade    - Run upgrade path tests"
	@echo "  test-migrate-native - Migrate a live repmgr cluster to native, in place (#292)"
	@echo "  test-migrate-repmgrd - Migrate a live failoverMode:repmgrd 1.x release to 2.0.0 (#298)"
	@echo "  test-config     - Run postgresql configuration tests (standalone)"
	@echo "  test-backup-restore - Run backup and restore integration tests"
	@echo "  test-backup-concurrent - Run concurrent pgbackrest backup + WAL archiving test"
	@echo "  test-pgbackrest-restore - Run pgbackrest PITR restore-validation + live restore Job test (#38/#226)"
	@echo "  test-pgbackrest-restore-ha - Run multi-replica pgbackrest restore + standby re-clone test (#226)"
	@echo "  test-pgbackrest-bootstrap - Run bootstrap-from-backup PVC-loss recovery test (#266)"
	@echo "  test-databases-roles - Run declarative databases/roles/grants test (#218)"
	@echo "  test-special-chars - Run special-character credential tests"
	@echo "  cluster-create  - Create Kind cluster"
	@echo "  cluster-delete  - Delete Kind cluster"
	@echo "  clean           - Delete cluster and test namespaces"

test: cluster-create test-template test-cluster cluster-delete

test-template:
	@bash $(TESTS_DIR)/test-template.sh

# Diff the default render of pg+pgvector vs a baseline ref (default origin/master) to
# confirm a change meant for one code path did not move the default one. Intended bumps
# show as a diff to review; not a hard gate. Usage: make byte-stable [REF=<ref>]
byte-stable:
	@bash ../scripts/check-byte-stable.sh $(REF)

# Run independent suites in parallel with -j
test-cluster: test-minimal test-agent test-full test-upgrade test-scaledown test-agent-rolling test-config test-backup-restore test-special-chars
	@echo "All test suites passed"

# Chain: agent install -> agent failover test (must be sequential; the failover
# suite reads the lease-elected primary/standby that the install suite records)
test-agent: cluster-exists
	@bash $(TESTS_DIR)/test-agent.sh
	@bash $(TESTS_DIR)/test-agent-failover.sh

# Agent control REST API (#276): mTLS enforcement, the read surface, and pause /
# switchover through the API (including that a switchover really moves leadership).
test-agent-control: cluster-exists
	@bash $(TESTS_DIR)/test-agent-control.sh

# API-driven pgBackRest restore (#276): proves the granted RBAC is sufficient, the
# cloned Job actually restores, and the outcome record survives the scale-down/up cycle.
test-agent-control-restore: cluster-exists
	@bash $(TESTS_DIR)/test-agent-control-restore.sh

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

# Logical replication slot sync (#308): wal_level (independent of pgbackrest.enabled),
# dbname patched into primary_conninfo, and synchronized_standby_slots reconciliation
# through install, a forced failover, and a scale-down. Standalone + opt-in, like
# test-cascading-replication.
test-sync-replication-slots: cluster-exists
	@bash $(TESTS_DIR)/test-sync-replication-slots.sh

# Scale replicaCount down and assert the primary unregisters the ghost repmgr.nodes
# record (#139). Runs in agent mode since #286; the standby ghost is deterministic
# because the StatefulSet always trims the top ordinal.
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

test-full: cluster-exists
	@bash $(TESTS_DIR)/test-full.sh

test-upgrade: cluster-exists
	@bash $(TESTS_DIR)/test-upgrade.sh

# Not in test-cluster: this suite pulls a RELEASED chart and a RELEASED image from the network,
# so it is the one suite that is not hermetic against the local tree (#292).
test-migrate-native: cluster-exists
	@bash $(TESTS_DIR)/test-migrate-native.sh

# Also not in test-cluster, same reason -- and this one additionally recreates a live
# StatefulSet with --cascade=orphan, which is the only 2.0.0 upgrade path that does (#298).
test-migrate-repmgrd: cluster-exists
	@bash $(TESTS_DIR)/test-migrate-repmgrd.sh

test-config: cluster-exists
	@bash $(TESTS_DIR)/test-config.sh

test-backup-restore: cluster-exists
	@bash $(TESTS_DIR)/test-backup-restore.sh

test-backup-concurrent: cluster-exists
	@bash $(TESTS_DIR)/test-backup-concurrent.sh

test-pgbackrest-restore: cluster-exists
	@bash $(TESTS_DIR)/test-pgbackrest-restore.sh

test-pgbackrest-restore-ha: cluster-exists
	@bash $(TESTS_DIR)/test-pgbackrest-restore-ha.sh

test-pgbackrest-bootstrap: cluster-exists
	@bash $(TESTS_DIR)/test-pgbackrest-bootstrap.sh

test-databases-roles: cluster-exists
	@bash $(TESTS_DIR)/test-databases-roles.sh

test-special-chars: cluster-exists
	@bash $(TESTS_DIR)/test-special-chars.sh

clean: cluster-delete
	@echo "Cleaned up"
