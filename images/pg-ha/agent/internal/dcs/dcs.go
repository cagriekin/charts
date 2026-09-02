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
	// OnRenewFailure fires, before OnLost, when leadership is lost INVOLUNTARILY --
	// the K8s Lease could not be renewed within RenewDeadline, or the etcd session
	// lease lapsed -- and not on a voluntary Release or a shutdown. It exists to feed
	// pg_ha_agent_renew_failures_total: the chart alerts on that counter's rate, and
	// with nothing incrementing it the alert could never fire (#298 review). Optional.
	OnRenewFailure func()
	// SafeToRelease is consulted by the backend AFTER OnLost has returned and BEFORE it
	// frees the lock, and a false answer means "hold it" (#298 review).
	//
	// Freeing the lock is what makes a step-down hand off in milliseconds instead of at TTL
	// expiry, and that is safe on exactly one condition: the demote OnLost just performed
	// actually completed. When it did not -- `Stop` cannot reach a postmaster in
	// uninterruptible sleep on a wedged PV, so it returns an error and deliberately leaves
	// the child supervised -- OnLost keeps this node marked read-write precisely because a
	// writer may still be up. Handing the lock over in that state gives a peer immediate
	// permission to promote beside it, with none of the LeaseDuration/TTL margin that
	// expiry-based handoff would have left. So the backends ask before releasing.
	//
	// Consulted only for an iteration that actually HELD the lock. Both backends also unwind
	// iterations this node spent as a follower -- a shutdown or Release landing while it was
	// still trying to acquire -- and there is no lock of ours to hold in that case: K8sDCS
	// finds a different HolderIdentity, and EtcdDCS's session key is a queued candidate whose
	// survival fences nothing while blocking every peer behind it in the create-revision order
	// (#298 review).
	//
	// Optional: nil means "always safe", which is the right default for a caller that does
	// not fence (and keeps every existing test honest).
	SafeToRelease func() bool
}
