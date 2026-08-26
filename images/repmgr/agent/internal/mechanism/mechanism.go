// Package mechanism is the swappable HA mechanism the agent drives. The reconcile
// loop never imports repmgr directly — only this interface — so repmgr can later be
// replaced by native pg_* calls without touching policy. Today the only
// implementation is repmgr-backed (Repmgr).
package mechanism

import (
	"context"
	"errors"
	"time"
)

// ErrRewindDiverged is returned by RejoinForceRewind when pg_rewind cannot
// proceed; the caller falls back to ReclonePreserving (the #175 data-safe path).
var ErrRewindDiverged = errors.New("mechanism: rewind diverged, reclone required")

// Conn is how to reach a peer PostgreSQL node for clone/follow/rejoin. Password is
// passed via PGPASSWORD, never on the command line or in logged argv.
//
// (NodeID was dropped in #294 with mechanism.Repmgr -- nothing addresses a peer by id.)
// The peer's address, carried so Follow takes one
// parameter that either mechanism can read the parts it needs from: repmgr addresses an
// upstream by node_id (it resolves the conninfo from repmgr.nodes), while a native
// mechanism must write an actual host into primary_conninfo and has no use for the id.
// Zero when the caller has no id to supply -- only repmgr requires it.
type Conn struct {
	Host           string
	Port           int
	User           string
	DB             string
	Password       string
	ConnectTimeout time.Duration
}

// NodeIdentity describes the local node for config generation.
type NodeIdentity struct {
	NodeName     string // pod hostname
	FQDN         string // <pod>.<headless> — the conninfo host
	DataDir      string // PGDATA
	PGBindir     string // /usr/lib/postgresql/<major>/bin
	ReplUser     string
	ReplDB       string
	ReplPassword string
}

// ConfigOpts are the agent-mode knobs for the generated config.
type ConfigOpts struct {
	Failover            string // "manual" in agent mode (repmgrd off)
	UseReplicationSlots bool
}

// Mechanism performs the Postgres replication mechanics. Each method is its own
// scoped operation with its own wrapped error (no monolithic catch). The caller
// (reconcile) has already decided, via the Lease and the timeline/LSN rules, that
// the action is legitimate.
type Mechanism interface {
	// GenerateConfig writes the mechanism config idempotently (native: the agent-owned
	// fragment inside PGDATA).
	GenerateConfig(ctx context.Context, n NodeIdentity, o ConfigOpts) error
	// Promote turns the local standby into a read-write primary on a new timeline.
	Promote(ctx context.Context) error
	// Follow points the local standby at upstream and restarts replication. The whole Conn
	// is passed rather than a bare name: the mechanism needs upstream.Host to write
	// primary_conninfo, and deriving the pod FQDN inside the mechanism would push the
	// StatefulSet naming convention into a layer whose entire purpose is to know nothing
	// about Kubernetes.
	Follow(ctx context.Context, upstream Conn) error
	// Clone builds the local PGDATA fresh from source (caller guarantees PGDATA is
	// empty or moved aside).
	Clone(ctx context.Context, source Conn) error
	// RejoinForceRewind rewinds the diverged local node forward onto target via
	// pg_rewind, then leaves it dormant for the supervisor to start as a standby.
	// Returns ErrRewindDiverged when pg_rewind cannot proceed.
	RejoinForceRewind(ctx context.Context, target Conn) error
	// ReclonePreserving renames PGDATA aside to .diverged.<ts>, clones from source,
	// and drops the backup only on success (#175 — never rm -rf before clone succeeds).
	ReclonePreserving(ctx context.Context, source Conn) error
}
