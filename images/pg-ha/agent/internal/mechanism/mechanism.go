// Package mechanism is the swappable HA mechanism the agent drives. The reconcile loop
// never imports an implementation — only this interface — which is what made replacing
// repmgr with native pg_* calls survivable one method at a time (#287).
//
// Since #294 there is exactly ONE implementation: Native (pg_ctl / pg_basebackup /
// pg_rewind, plus primary_conninfo + standby.signal). Repmgr is deleted, and config.Load
// rejects MECHANISM=repmgr outright. The interface stays because the seam — policy in
// reconcile, mechanics here — is what keeps the failover rules testable without a cluster.
package mechanism

import (
	"context"
	"errors"
	"time"
)

// ErrRewindDiverged is returned by RejoinForceRewind when pg_rewind cannot
// proceed; the caller falls back to ReclonePreserving (the #175 data-safe path).
var ErrRewindDiverged = errors.New("mechanism: rewind diverged, reclone required")

// ErrRewindUnreachable is returned by RejoinForceRewind when pg_rewind could not CONNECT to
// the target at all. It is a strictly narrower claim than "not divergence": the rewind never
// examined this node's history, so nothing is known about it either way.
//
// The caller must not count it toward the re-clone backstop (#298 review). That backstop
// exists for a refusal that is permanent AND local -- pg_rewind wanting a configuration the
// target does not have, say -- which would otherwise retry forever and leave the node out of
// the cluster; escalating converges. Unreachability is the one class where escalating
// converges on NOTHING: ReclonePreserving connects to the same target with the same
// credentials, so it cannot succeed where the rewind just failed to connect. All the
// escalation buys is a PGDATA rename, a failed pg_basebackup, and an unreaped
// `.diverged.<ts>` copy on the PVC -- for a target at max_connections, a rotated credential,
// or a primary that is still starting up. Retrying is not a stall here: the node stays a
// stopped standby and rejoins on the first tick that can reach the target.
var ErrRewindUnreachable = errors.New("mechanism: rewind target unreachable, retry")

// Conn is how to reach a peer PostgreSQL node for clone/follow/rejoin: the address parts a
// mechanism needs to write primary_conninfo or point a pg_basebackup at an upstream.
//
// The credential is NOT here. It lives on the mechanism itself (Native.Password) and travels
// via PGPASSWORD, never on a command line or in logged argv. Conn carried a Password field
// until #294 that nothing ever set or read; it went with the NodeID field, which existed only
// because repmgr addressed an upstream by node_id out of repmgr.nodes.
type Conn struct {
	Host           string
	Port           int
	User           string
	DB             string
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
