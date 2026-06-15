package dcs

import (
	"context"
	"fmt"
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
}

// K8sDCS implements DCS against a coordination.k8s.io/v1 Lease via client-go
// leaderelection. It never hand-rolls the lock — leaderelection provides the
// atomic acquire/renew (resourceVersion CAS) and TTL semantics.
type K8sDCS struct {
	cfg      K8sConfig
	client   kubernetes.Interface
	isLeader atomic.Bool
	leader   atomic.Value // string
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
		le, err := leaderelection.NewLeaderElector(lec)
		if err != nil {
			return // config error (timings invalid); nothing to retry
		}
		le.Run(ctx) // blocks: acquire -> lead -> lose, then returns
		k.isLeader.Store(false)
		select {
		case <-ctx.Done():
		case <-time.After(k.cfg.RetryPeriod):
		}
	}
}
