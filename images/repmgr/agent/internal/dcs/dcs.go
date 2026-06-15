// Package dcs is the leadership lock behind a backend-agnostic interface so the
// reconcile loop never depends on how leadership is decided. k8sDCS (client-go
// leaderelection against a coordination.k8s.io/v1 Lease) is the default impl; a
// future etcdDCS slots in here without touching reconcile.
package dcs

import "context"

// DCS is a leadership lock. Campaign blocks until the caller holds it; Leader
// reports the current holder so followers can find their upstream.
type DCS interface {
	// Campaign blocks until identity holds the lock or ctx is cancelled. The
	// returned Leadership stays valid until its Done channel closes.
	Campaign(ctx context.Context, identity string) (Leadership, error)
	// Leader returns the current holder identity, or "" if unknown/none.
	Leader(ctx context.Context) (string, error)
}

// Leadership is an acquired lock. Done closes when leadership is lost (lease
// expired, resigned, or backend error). Resign relinquishes it cleanly.
type Leadership interface {
	Done() <-chan struct{}
	Resign(ctx context.Context) error
}
