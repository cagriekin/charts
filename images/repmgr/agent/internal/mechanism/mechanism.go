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

// ErrUpstreamUnknown is returned by Follow when the mechanism cannot resolve the
// upstream at all, as opposed to failing to attach to it. With repmgr that means the
// upstream has no repmgr.nodes record in the copy this node can read.
//
// It happens on a scale-up: a new pod can win the Lease and promote before it has ever
// registered as a standby, so the surviving nodes' metadata -- only as fresh as their
// replication -- never learned about it. repmgr resolves the upstream and "the primary"
// from that local copy (its connection flags describe the LOCAL node, not the primary),
// so no amount of addressing helps: the record simply is not there, and both follow and
// register fail on every tick forever.
//
// The caller escalates to the rejoin path, which rewinds (or re-clones) onto the current
// primary and thereby obtains correct data AND correct metadata in one step -- the same
// recovery a post-failover divergence already uses (#286).
var ErrUpstreamUnknown = errors.New("mechanism: upstream not resolvable from local metadata")

// Conn is how to reach a peer PostgreSQL node for clone/follow/rejoin. Password is
// passed via PGPASSWORD, never on the command line or in logged argv.
type Conn struct {
	Host     string
	Port     int
	User     string
	DB       string
	Password string
	ConnectTimeout time.Duration
}

// NodeIdentity describes the local node for config generation.
type NodeIdentity struct {
	NodeID   int    // ordinal+1000, stable across restarts
	NodeName string // pod hostname
	FQDN     string // <pod>.<headless> — the conninfo host
	DataDir  string // PGDATA
	PGBindir string // /usr/lib/postgresql/<major>/bin
	ReplUser string
	ReplDB   string
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
	// GenerateConfig writes the mechanism config (repmgr.conf) idempotently.
	GenerateConfig(ctx context.Context, n NodeIdentity, o ConfigOpts) error
	// Promote turns the local standby into a read-write primary on a new timeline.
	Promote(ctx context.Context) error
	// Follow points the local standby at the upstream node and restarts replication.
	Follow(ctx context.Context, upstreamNodeID int) error
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
	// RegisterPrimary / RegisterStandby reconcile repmgr.nodes toward the
	// Lease-derived role.
	RegisterPrimary(ctx context.Context) error
	RegisterStandby(ctx context.Context, upstreamNodeID int) error
	// Unregister removes a node's repmgr.nodes record (repmgr standby unregister
	// --node-id), to clean up the ghost rows a replicaCount scale-down leaves
	// behind (#139). Run on the primary for nodes the StatefulSet no longer hosts.
	Unregister(ctx context.Context, nodeID int) error
}
