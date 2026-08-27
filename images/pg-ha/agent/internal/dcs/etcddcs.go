package dcs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

// EtcdConfig parameterizes the etcd-backed lock. TTLSeconds maps to LeaseDuration
// (the session lease TTL; etcd renews it at TTL/3, so loss is detected within ~TTL
// of the last successful keepalive -- the same time-based soft-fence window as the
// Kubernetes backend). Unlike the K8s backend, leadership lives in etcd, so a
// Kubernetes control-plane outage does not by itself demote the primary (Part G5).
//
// Soft-fence margin note: the K8s backend self-demotes at RenewDeadline (an explicit
// pre-expiry margin) while a challenger waits the full LeaseDuration. The etcd
// session has no RenewDeadline analog -- the holder self-fences at ~TTL (its
// client-side deadline) and a challenger acquires at ~TTL (server lease expiry), so
// the demote-before-acquire ordering still holds (the holder's deadline runs off its
// last keepalive, <=TTL/3 stale vs the server), but the margin is implicit, not the
// explicit RenewDeadline slack. It is comfortable at the shipped 15s; the config
// validator enforces LeaseDuration >= 5s so a tiny TTL cannot collapse it. A future
// enhancement (deferred to the live etcd-mode test) can add an explicit early
// self-demote by managing the lease keepalive directly instead of via Session.
type EtcdConfig struct {
	Endpoints   []string
	Prefix      string // election key prefix, e.g. /pg-ha/<release>/leader
	TTLSeconds  int    // session lease TTL (from LeaseDuration), >=1
	DialTimeout time.Duration
	RetryPeriod time.Duration
	// StepDownCooldown suppresses re-contention after a voluntary Release so a peer
	// wins the freed key. Defaults to 3*RetryPeriod when zero.
	StepDownCooldown time.Duration
	// TLS (optional): client cert/key + CA for a mutually-authenticated etcd.
	CertFile, KeyFile, CAFile string
}

// EtcdDCS implements DCS against etcd via the concurrency (Session + Election)
// primitives. It is the dcs.DCS contract's second backend; the reconcile loop is
// backend-agnostic and never knows which one is wired.
type EtcdDCS struct {
	cfg      EtcdConfig
	client   *clientv3.Client
	isLeader atomic.Bool
	leader   atomic.Value // string

	mu            sync.Mutex
	resign        context.CancelFunc // cancels the current election iteration (Release/shutdown)
	cooldownUntil time.Time
}

// tlsConfig builds the client TLS config from the configured files. All three
// (cert, key, CA) must be set together for mutual TLS; none set means plaintext.
func (c EtcdConfig) tlsConfig() (*tls.Config, error) {
	if c.CertFile == "" && c.KeyFile == "" && c.CAFile == "" {
		return nil, nil
	}
	if c.CertFile == "" || c.KeyFile == "" || c.CAFile == "" {
		return nil, fmt.Errorf("etcd TLS needs cert, key, and CA together (cert=%q key=%q ca=%q)", c.CertFile, c.KeyFile, c.CAFile)
	}
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("etcd client keypair: %w", err)
	}
	ca, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, fmt.Errorf("etcd CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("etcd CA %s: no certificates parsed", c.CAFile)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// NewEtcdDCS dials etcd and returns an EtcdDCS. It validates the config (endpoints,
// prefix, TTL, TLS triplet) fail-fast so a misconfig terminates at boot.
func NewEtcdDCS(cfg EtcdConfig) (*EtcdDCS, error) {
	if len(cfg.Endpoints) == 0 {
		return nil, fmt.Errorf("etcd DCS: no endpoints")
	}
	if cfg.Prefix == "" {
		return nil, fmt.Errorf("etcd DCS: no key prefix")
	}
	if cfg.TTLSeconds < 1 {
		return nil, fmt.Errorf("etcd DCS: TTLSeconds must be >= 1, got %d", cfg.TTLSeconds)
	}
	tlsCfg, err := cfg.tlsConfig()
	if err != nil {
		return nil, err
	}
	dt := cfg.DialTimeout
	if dt <= 0 {
		dt = 5 * time.Second
	}
	cli, err := clientv3.New(clientv3.Config{Endpoints: cfg.Endpoints, DialTimeout: dt, TLS: tlsCfg})
	if err != nil {
		return nil, fmt.Errorf("etcd client: %w", err)
	}
	e := &EtcdDCS{cfg: cfg, client: cli}
	e.leader.Store("")
	return e, nil
}

func (e *EtcdDCS) IsLeader() bool { return e.isLeader.Load() }

func (e *EtcdDCS) Leader() string {
	s, _ := e.leader.Load().(string)
	return s
}

// Run contends for and holds leadership until ctx is cancelled, re-contending in a
// loop. Each iteration creates a session (lease + keepalive), campaigns, and -- on
// becoming leader -- waits for loss (session expiry or a Release/shutdown cancel),
// running OnLost synchronously BEFORE the next iteration so the demote completes
// before any re-acquire (the fence-ordering guarantee, symmetric with K8sDCS).
func (e *EtcdDCS) Run(ctx context.Context, identity string, cb Callbacks) {
	for ctx.Err() == nil {
		// Respect a step-down cooldown so a peer wins a just-released key.
		e.mu.Lock()
		until := e.cooldownUntil
		e.mu.Unlock()
		if d := time.Until(until); d > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
		}

		iterCtx, cancel := context.WithCancel(ctx)
		e.mu.Lock()
		e.resign = cancel
		e.mu.Unlock()

		e.runElection(iterCtx, identity, cb)

		cancel()
		e.mu.Lock()
		e.resign = nil
		e.mu.Unlock()
		e.isLeader.Store(false)

		select {
		case <-ctx.Done():
			return
		case <-time.After(e.retryPeriod()):
		}
	}
}

// runElection is one acquire->lead->lose cycle. A session create failure (etcd
// unreachable) returns so the Run loop retries after the retry period.
func (e *EtcdDCS) runElection(ctx context.Context, identity string, cb Callbacks) {
	sess, err := concurrency.NewSession(e.client, concurrency.WithTTL(e.cfg.TTLSeconds), concurrency.WithContext(ctx))
	if err != nil {
		return // etcd unreachable; retry next iteration (leadership unchanged)
	}
	defer e.releaseSession(sess) // frees the lease+key on the way out (runs after OnLost)
	el := concurrency.NewElection(sess, e.cfg.Prefix)

	// Observe the current leader for followers (Leader()), independent of whether
	// this node wins. Stops when ctx (the iteration) is cancelled.
	go e.observe(ctx, el)

	// Campaign blocks until this node is leader or ctx is cancelled. Session loss is
	// NOT one of its exits, so it must be guarded explicitly -- see campaignGuarded
	// for why an unguarded call yields zombie candidates and phantom leadership
	// (#326).
	if !campaignGuarded(ctx, sess.Done(), func(c context.Context) error {
		return el.Campaign(c, identity)
	}) {
		return
	}
	e.isLeader.Store(true)
	e.leader.Store(identity)
	if cb.OnAcquired != nil {
		cb.OnAcquired(ctx)
	}

	// Hold until leadership ends: the session lease lapses (etcd unreachable past
	// TTL) or a Release/shutdown cancels the iteration.
	select {
	case <-sess.Done():
	case <-ctx.Done():
	}
	e.isLeader.Store(false)
	// No longer leader. observe() stops when the iteration ctx cancels, so on a
	// voluntary Release it would not catch a successor and Leader() would keep
	// reporting self through the step-down cooldown. Clear the cached identity (only
	// if it still names us, so a successor observe() did catch is preserved) so
	// Leader() reports "unknown" rather than stale-self -- symmetric with K8sDCS.
	e.leader.CompareAndSwap(identity, "")
	if cb.OnLost != nil {
		cb.OnLost() // synchronous: demote before the Run loop re-contends (and before releaseSession)
	}
}

// releaseSession tears the session down on the way out of an election iteration. It
// orphans the keepalive and revokes the lease, which deletes the lease-bound
// election key in one step so a peer's Campaign proceeds immediately on a voluntary
// step-down. The revoke runs off context.Background() because the iteration ctx is
// cancelled on Release/shutdown -- the session's own ctx-bound Close() would then
// fail to revoke and leak the lease until TTL expiry. Best-effort: on a lapsed
// session (etcd unreachable) the lease is already gone and the revoke just times out
// (bounded), with TTL expiry as the backstop. Never on the critical demote path
// (OnLost already ran), so it cannot delay a fence.
func (e *EtcdDCS) releaseSession(sess *concurrency.Session) {
	sess.Orphan() // stop the keepalive refresh (Close would also revoke on the dead ctx)
	rc, rcancel := context.WithTimeout(context.Background(), e.retryPeriod())
	_, _ = e.client.Revoke(rc, sess.Lease())
	rcancel()
}

// observe updates the last-seen leader identity from the election until ctx ends.
//
// NOTE: two known gaps here, both tracked on #326 and both needing an etcd-backed
// test, so neither is addressed in the campaign-guard change.
//
//  1. It cannot clear the cache when the leader key is deleted, so Leader() can keep
//     naming a node that is already gone. Election.Observe never reports a deletion:
//     both of its send sites construct exactly one KV, and on a DELETE event it sets
//     keyDeleted, re-Gets, finds nothing, and blocks in a PUT-only watch without
//     sending. Clearing needs a different source -- a WithPrefix watch on cfg.Prefix
//     for DELETE events, or polling el.Leader(ctx) and treating ErrElectionNoLeader
//     as "clear".
//  2. This loop never restarts. Election.observe does `defer close(ch)` and returns
//     on ANY client.Get error, and its watch loop returns on channel closure without
//     even checking wr.Err() -- so a single disruption (e.g. "required revision has
//     been compacted") ends the range below permanently. A standby that never wins
//     stays inside one runElection iteration indefinitely, so e.leader then freezes
//     for the life of the process: with 3+ replicas, a node whose watch broke keeps
//     reporting a leader that lost the lease hours ago and never re-targets. The fix
//     is to re-enter Observe until ctx ends, not to exit on first closure.
func (e *EtcdDCS) observe(ctx context.Context, el *concurrency.Election) {
	for resp := range el.Observe(ctx) {
		if len(resp.Kvs) > 0 {
			e.leader.Store(string(resp.Kvs[0].Value))
		}
	}
}

// Release voluntarily steps down: it cancels the current election iteration (the
// session closes, revoking the lease so the key frees) and suppresses re-contention
// for the cooldown so a peer acquires the freed key. Non-blocking; OnLost (the
// synchronous demote) runs in the Run goroutine as the iteration unwinds. Safe when
// not leading (still arms the cooldown). Symmetric with K8sDCS.Release.
func (e *EtcdDCS) Release() {
	cd := e.cfg.StepDownCooldown
	if cd <= 0 {
		cd = 3 * e.retryPeriod()
	}
	e.mu.Lock()
	e.cooldownUntil = time.Now().Add(cd)
	cancel := e.resign
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Close releases the etcd client (call on agent shutdown).
func (e *EtcdDCS) Close() error { return e.client.Close() }

func (e *EtcdDCS) retryPeriod() time.Duration {
	if e.cfg.RetryPeriod <= 0 {
		return 2 * time.Second
	}
	return e.cfg.RetryPeriod
}

// campaignGuarded runs campaign and reports whether leadership was genuinely won,
// i.e. won while the session lease was still alive.
//
// etcd's Election.Campaign does NOT abort when the session lapses: it puts a
// lease-bound candidate key, then blocks in waitDeletes() watching only keys with a
// LOWER revision, returning on ctx cancel or once those keys are gone. Session expiry
// is not a termination condition (documented upstream, and identical in v3.5.12 and
// the vendored v3.5.31 -- there is no upstream fix to adopt). Two failures follow
// from calling it unguarded (#326):
//
//   - The lease expires mid-campaign, so etcd deletes this node's candidate key while
//     Campaign stays blocked forever. The node becomes a zombie candidate -- no lease,
//     no key, absent from the election -- and the Run loop never re-contends.
//   - When the leader's key is eventually deleted, waitDeletes returns nil, so
//     Campaign returns nil ("you are leader") while this node holds no lease and no
//     key. Acting on that is phantom leadership, concurrent with the peer that holds
//     the real lease and is correctly leader.
//
// So: stop waiting on the campaign the moment the session lapses, and re-check
// liveness before reporting a win.
//
// The lapse path deliberately does NOT wait for the campaign to unwind. Cancelling
// campCtx makes Campaign's waitDeletes return a ctx error, but Campaign then calls
// Resign on the CLIENT context -- which carries no deadline and is only cancelled by
// Client.Close() -- and clientv3 defaults to WaitForReady(true), so that Txn blocks
// for the entire remainder of an etcd outage. Since the lapse is normally *caused* by
// that outage, waiting would keep runElection from returning and defeat the very
// guarantee this guard exists to provide. Returning immediately lets the Run loop
// re-contend; the campaign goroutine unwinds on its own once etcd is reachable, and
// its result goes to a buffered channel so it cannot block or leak.
func campaignGuarded(ctx context.Context, sessDone <-chan struct{}, campaign func(context.Context) error) bool {
	campCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- campaign(campCtx) }()

	select {
	case <-sessDone:
		return false // lease gone: not leader, and do not block on the unwind
	case err := <-done:
		if err != nil {
			return false
		}
	}

	// Campaign reported a win, but it can do so after the lease is already gone.
	// Leadership is valid only while the session still holds the lock.
	select {
	case <-sessDone:
		return false
	default:
		return true
	}
}
