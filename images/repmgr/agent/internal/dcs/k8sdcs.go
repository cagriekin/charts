package dcs

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// K8sConfig parameterizes the Lease-backed lock. The timings map directly to
// client-go leaderelection; cloud presets widen them (see plan E4).
type K8sConfig struct {
	Namespace     string
	LeaseName     string
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
	// StepDownCooldown is how long a node suppresses re-contention after a
	// voluntary Release, so a peer wins the freed lease instead of the stepping-down
	// node immediately re-acquiring it. Defaults to 3*RetryPeriod when zero.
	StepDownCooldown time.Duration
}

// K8sDCS implements DCS against a coordination.k8s.io/v1 Lease via client-go
// leaderelection. It never hand-rolls the lock — leaderelection provides the
// atomic acquire/renew (resourceVersion CAS) and TTL semantics.
type K8sDCS struct {
	cfg      K8sConfig
	client   kubernetes.Interface
	isLeader atomic.Bool
	leader   atomic.Value // string

	mu            sync.Mutex
	stepDown      context.CancelFunc // cancels the current election iteration (set while contending/leading)
	cooldownUntil time.Time          // suppress re-contention until this time (set by Release)
}

// NewK8sDCS builds a K8sDCS using the in-cluster config and ServiceAccount.
func NewK8sDCS(cfg K8sConfig) (*K8sDCS, error) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return NewK8sDCSWithClient(cfg, cs), nil
}

// NewK8sDCSWithClient builds a K8sDCS with an injected clientset (for tests).
func NewK8sDCSWithClient(cfg K8sConfig, client kubernetes.Interface) *K8sDCS {
	k := &K8sDCS{cfg: cfg, client: client}
	k.leader.Store("")
	return k
}

func (k *K8sDCS) IsLeader() bool { return k.isLeader.Load() }

func (k *K8sDCS) Leader() string {
	s, _ := k.leader.Load().(string)
	return s
}

// Run drives leadership until ctx is cancelled. leaderelection.Run returns when
// leadership is lost; we re-contend in a loop so the agent keeps trying to lead.
// OnStoppedLeading runs synchronously inside client-go before Run returns, so the
// OnLost demote completes before any re-acquire — the fence-ordering guarantee.
func (k *K8sDCS) Run(ctx context.Context, identity string, cb Callbacks) {
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: k.cfg.LeaseName, Namespace: k.cfg.Namespace},
		Client:     k.client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
	}
	lec := leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   k.cfg.LeaseDuration,
		RenewDeadline:   k.cfg.RenewDeadline,
		RetryPeriod:     k.cfg.RetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(c context.Context) {
				k.isLeader.Store(true)
				if cb.OnAcquired != nil {
					cb.OnAcquired(c)
				}
			},
			OnStoppedLeading: func() {
				k.isLeader.Store(false)
				if cb.OnLost != nil {
					cb.OnLost() // synchronous: must finish demoting before we re-contend
				}
			},
			OnNewLeader: func(id string) { k.leader.Store(id) },
		},
	}

	for ctx.Err() == nil {
		// Respect a step-down cooldown so a peer wins a just-released lease before
		// this node re-contends.
		k.mu.Lock()
		until := k.cooldownUntil
		k.mu.Unlock()
		if d := time.Until(until); d > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
		}

		// A per-iteration context so Release can cancel just this election (releasing
		// the lease via ReleaseOnCancel) without tearing down the agent.
		elerCtx, cancel := context.WithCancel(ctx)
		k.mu.Lock()
		k.stepDown = cancel
		k.mu.Unlock()

		le, err := leaderelection.NewLeaderElector(lec)
		if err != nil {
			cancel()
			return // config error (timings invalid); nothing to retry
		}
		le.Run(elerCtx) // blocks: acquire -> lead -> lose (or Release cancels), then returns
		cancel()
		k.mu.Lock()
		k.stepDown = nil
		k.mu.Unlock()
		k.isLeader.Store(false)

		select {
		case <-ctx.Done():
			return
		case <-time.After(k.cfg.RetryPeriod):
		}
	}
}

// Release voluntarily steps down: it cancels the current election so the Lease is
// released (client-go ReleaseOnCancel), and suppresses re-contention for the
// step-down cooldown so a peer acquires the freed Lease instead of this node
// immediately re-winning it. It is non-blocking; OnStoppedLeading (the synchronous
// demote) runs in the Run goroutine as the election unwinds. Safe to call when not
// leading (it still arms the cooldown). Used by the self-health and stale-winner
// step-down paths.
func (k *K8sDCS) Release() {
	cd := k.cfg.StepDownCooldown
	if cd <= 0 {
		cd = 3 * k.cfg.RetryPeriod
	}
	k.mu.Lock()
	k.cooldownUntil = time.Now().Add(cd)
	cancel := k.stepDown
	k.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
