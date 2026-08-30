package dcs

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/cagriekin/pg-ha-agent/internal/kubecfg"
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

// NewK8sDCS builds a K8sDCS from the resolved apiserver config -- KUBECONFIG when
// set, the in-cluster ServiceAccount otherwise. The Lease lock has to travel the
// same route as the mutation client: reaching one but not the other elects a leader
// that cannot publish, or publishes without holding the lease (#317).
func NewK8sDCS(cfg K8sConfig) (*K8sDCS, error) {
	rc, err := kubecfg.RESTConfig()
	if err != nil {
		return nil, err
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
				// client-go runs THIS callback on its own goroutine (`go
				// OnStartedLeading(ctx)` in LeaderElector.Run) while OnStoppedLeading is a
				// plain defer, so the two stores are unordered: a Release that cancels the
				// election before this goroutine is scheduled lets it land AFTER the loop
				// below has already stored false and client-go has emptied the Lease --
				// latching isLeader true on a node that holds nothing. observe() feeds
				// IsLeader() straight into Observation.HoldLease, so the node would then
				// take a holder branch (Promote, read-write StartLocal) against a peer that
				// genuinely holds the Lease. Gate on the iteration's own context under the
				// same mutex the loop takes when it clears the flag: cancel() propagates to
				// this child synchronously, so the callback either wins the lock first (and
				// the loop's store(false) follows it) or sees c already cancelled.
				k.mu.Lock()
				live := c.Err() == nil
				if live {
					k.isLeader.Store(true)
				}
				k.mu.Unlock()
				if !live {
					return
				}
				if cb.OnAcquired != nil {
					cb.OnAcquired(c)
				}
			},
			OnStoppedLeading: func() {
				k.isLeader.Store(false)
				// Involuntary loss means client-go could not renew within RenewDeadline.
				// The two voluntary exits are distinguishable without new state: Release
				// arms the step-down cooldown BEFORE cancelling the election, and a
				// shutdown cancels the outer ctx (#298 review).
				if cb.OnRenewFailure != nil && ctx.Err() == nil {
					k.mu.Lock()
					voluntary := time.Now().Before(k.cooldownUntil)
					k.mu.Unlock()
					if !voluntary {
						cb.OnRenewFailure()
					}
				}
				if cb.OnLost != nil {
					cb.OnLost() // synchronous: must finish demoting before we re-contend
				}
			},
			OnNewLeader: func(id string) { k.leader.Store(id) },
		},
	}

	for ctx.Err() == nil {
		// A per-iteration context so Release can cancel just this election (releasing
		// the lease via ReleaseOnCancel) without tearing down the agent.
		//
		// Installed BEFORE the cooldown is consulted, not after (#298 review). The old
		// order left a window in which a step-down was silently dropped: the loop read
		// cooldownUntil (still zero), and a Release arriving before stepDown was assigned
		// armed the cooldown but found `cancel == nil` and cancelled nothing -- so this node
		// walked straight into le.Run and could re-win the very Lease it was handing over,
		// with the cooldown it had just armed already read and discarded. Assigning first
		// means every Release either cancels a live election or lands in the one remaining
		// gap (between clearing stepDown and the next iteration's assignment), which the
		// re-read below covers.
		elerCtx, cancel := context.WithCancel(ctx)
		k.mu.Lock()
		k.stepDown = cancel
		until := k.cooldownUntil
		k.mu.Unlock()
		// Respect a step-down cooldown so a peer wins a just-released lease before
		// this node re-contends. A Release DURING this wait cancels elerCtx, so le.Run
		// returns at once and the next iteration re-reads the newly armed cooldown.
		if d := time.Until(until); d > 0 {
			select {
			case <-ctx.Done():
				cancel()
				return
			case <-time.After(d):
			}
		}

		le, err := leaderelection.NewLeaderElector(lec)
		if err != nil {
			cancel()
			// Nothing to retry (the elector is only rejected for invalid config), but
			// never exit silently: this goroutine is fire-and-forget, so a silent return
			// left a healthy-looking agent that ticks forever yet never contends for
			// leadership, with no line anywhere naming the cause (#298 review).
			// config.Load pre-validates the client-go bounds (including the 1.2x jitter
			// rule), so reaching this is a should-never-happen.
			slog.Error("leader election disabled: client-go rejected the election config", "err", err)
			return
		}
		le.Run(elerCtx) // blocks: acquire -> lead -> lose (or Release cancels), then returns
		// cancel() FIRST, and clear the flag under the mutex: both halves pair with the
		// OnStartedLeading guard above. cancel() marks this iteration's context dead
		// (synchronously, including the child client-go handed the callback), so a
		// still-unscheduled OnStartedLeading can no longer set the flag; taking mu around
		// the store makes the two mutually exclusive rather than merely unlikely.
		cancel()
		k.mu.Lock()
		k.stepDown = nil
		k.isLeader.Store(false)
		k.mu.Unlock()

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
