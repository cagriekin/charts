// Package dcs is the leadership lock behind a backend-agnostic interface so the
// reconcile loop never depends on how leadership is decided. k8sDCS (client-go
// leaderelection against a coordination.k8s.io/v1 Lease) is the default impl; a
// future etcdDCS slots in here without touching reconcile.
package dcs

import "context"

// DCS is a leadership lock backend. Run contends for and maintains leadership for
// the agent until ctx is cancelled; the reconcile loop reads IsLeader/Leader each
// tick, and Callbacks fire on transitions.
type DCS interface {
	// Run blocks, contending for and holding the lock until ctx is cancelled. Run
	// it in a goroutine; on cancel it releases the lock (best effort).
	Run(ctx context.Context, identity string, cb Callbacks)
	// IsLeader reports whether this agent currently holds the lock.
	IsLeader() bool
	// Leader returns the last-observed holder identity (for followers), or "".
	Leader() string
	// Release voluntarily steps down from leadership (releasing the lock) and
	// suppresses re-contention briefly so a peer can take over. Non-blocking; a
	// no-op-safe call when not currently the leader. Backs the self-health and
	// stale-winner step-down paths.
	Release()
}

// Callbacks fire on leadership transitions. OnLost MUST complete its work
// (demote Postgres) synchronously: it runs before the lock can be re-acquired by
// anyone, which is the fence-ordering guarantee that prevents two writers.
type Callbacks struct {
	OnAcquired func(ctx context.Context)
	OnLost     func()
}
