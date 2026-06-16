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
	defer sess.Close()
	el := concurrency.NewElection(sess, e.cfg.Prefix)

	// Observe the current leader for followers (Leader()), independent of whether
	// this node wins. Stops when ctx (the iteration) is cancelled.
	go e.observe(ctx, el)

	// Campaign blocks until this node is leader, or ctx is cancelled, or the session
	// is lost -- all of which return an error here (we did NOT become leader).
	if err := el.Campaign(ctx, identity); err != nil {
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
	if cb.OnLost != nil {
		cb.OnLost() // synchronous: demote before the Run loop re-contends
	}
	// Best-effort prompt key release if the session is still alive (Release/shutdown
	// path); on a lapsed session the key is already gone. Bounded, off ctx (which may
	// be cancelled), and never on the critical demote path (that already ran above).
	rc, rcancel := context.WithTimeout(context.Background(), e.retryPeriod())
	_ = el.Resign(rc)
	rcancel()
}

// observe updates the last-seen leader identity from the election until ctx ends.
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
