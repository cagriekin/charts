package dcs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Drives the real client-go leaderelection against a fake clientset: a single
// agent must acquire the Lease, report IsLeader/Leader, and fire OnAcquired.
func TestK8sDCSAcquiresLeadership(t *testing.T) {
	cs := fake.NewSimpleClientset()
	k := NewK8sDCSWithClient(K8sConfig{
		Namespace:     "ns",
		LeaseName:     "pg-leader",
		LeaseDuration: 2 * time.Second,
		RenewDeadline: 1 * time.Second,
		RetryPeriod:   200 * time.Millisecond,
	}, cs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	acquired := make(chan struct{})
	var once sync.Once
	go k.Run(ctx, "pod-0", Callbacks{
		OnAcquired: func(context.Context) { once.Do(func() { close(acquired) }) },
	})

	select {
	case <-acquired:
	case <-time.After(15 * time.Second):
		t.Fatal("never acquired leadership")
	}
	if !k.IsLeader() {
		t.Error("IsLeader() = false after acquiring")
	}
	// OnNewLeader (which sets Leader()) is a separate callback that can lag the
	// OnAcquired close by a scheduling tick, so poll rather than read once.
	var got string
	for i := 0; i < 100; i++ {
		if got = k.Leader(); got == "pod-0" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got != "pod-0" {
		t.Errorf("Leader() = %q, want pod-0", got)
	}
}

// Release must hand leadership to a peer: with two agents contending, releasing
// the current holder lets the other acquire the freed Lease (the stale-winner /
// self-health step-down path). The cooldown keeps the releaser from re-winning.
func TestK8sDCSReleaseHandsOff(t *testing.T) {
	cs := fake.NewSimpleClientset()
	mk := func() *K8sDCS {
		return NewK8sDCSWithClient(K8sConfig{
			Namespace: "ns", LeaseName: "pg-leader",
			LeaseDuration: 2 * time.Second, RenewDeadline: 1 * time.Second, RetryPeriod: 200 * time.Millisecond,
			StepDownCooldown: 1 * time.Second,
		}, cs)
	}
	a, b := mk(), mk()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lost := make(chan string, 2)
	// A voluntary Release is a step-down, not an apiserver problem: it must never
	// count toward pg_ha_agent_renew_failures_total (#298 review).
	renewFailed := func() { t.Error("voluntary release fired OnRenewFailure") }
	go a.Run(ctx, "pod-0", Callbacks{OnLost: func() { lost <- "pod-0" }, OnRenewFailure: renewFailed})
	go b.Run(ctx, "pod-1", Callbacks{OnLost: func() { lost <- "pod-1" }, OnRenewFailure: renewFailed})

	leaderOf := func() *K8sDCS {
		for i := 0; i < 150; i++ {
			if a.IsLeader() {
				return a
			}
			if b.IsLeader() {
				return b
			}
			time.Sleep(100 * time.Millisecond)
		}
		return nil
	}

	leader := leaderOf()
	if leader == nil {
		t.Fatal("neither agent acquired leadership")
	}
	other := a
	if leader == a {
		other = b
	}

	leader.Release()

	// the released holder must lose leadership (OnLost fires) and the peer take over
	select {
	case <-lost:
	case <-time.After(10 * time.Second):
		t.Fatal("released holder never fired OnLost")
	}
	for i := 0; i < 150; i++ {
		if other.IsLeader() {
			return // handoff succeeded
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("peer did not take over after Release (a=%v b=%v)", a.IsLeader(), b.IsLeader())
}

// #298 review: the Lease is freed only when the agent says it is SAFE to free. Emptying it is
// what turns a step-down into a millisecond handoff, and that is safe on one condition -- the
// demote OnLost just ran actually completed. On the wedged-PV path it does not (SIGKILL cannot
// reach a process in uninterruptible sleep, so Stop returns an error and leaves the child
// supervised), OnLost keeps the node marked read-write because a writer may still be up, and an
// immediate handoff would let a peer promote beside it with no margin at all. Holding the Lease
// costs the peer only the LeaseDuration it would have waited anyway.
func TestK8sDCSHoldsTheLeaseWhenTheFenceDidNotComplete(t *testing.T) {
	for _, tc := range []struct {
		name      string
		safe      bool
		wantEmpty bool
	}{
		{"fence completed -> hand off now", true, true},
		{"fence did not complete -> hold to TTL", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			d := NewK8sDCSWithClient(K8sConfig{
				Namespace: "ns", LeaseName: "pg-leader",
				LeaseDuration: 2 * time.Second, RenewDeadline: 1 * time.Second, RetryPeriod: 200 * time.Millisecond,
				StepDownCooldown: 10 * time.Second,
			}, cs)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			lost := make(chan struct{}, 1)
			go d.Run(ctx, "pod-0", Callbacks{
				OnLost:        func() { lost <- struct{}{} },
				SafeToRelease: func() bool { return tc.safe },
			})
			for i := 0; !d.IsLeader() && i < 150; i++ {
				time.Sleep(100 * time.Millisecond)
			}
			if !d.IsLeader() {
				t.Fatal("never acquired leadership")
			}
			d.Release()
			select {
			case <-lost:
			case <-time.After(10 * time.Second):
				t.Fatal("OnLost never fired")
			}
			// releaseLease runs immediately after OnLost returns, inside the same goroutine.
			var holder string
			for i := 0; i < 100; i++ {
				l, err := cs.CoordinationV1().Leases("ns").Get(ctx, "pg-leader", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				holder = ""
				if l.Spec.HolderIdentity != nil {
					holder = *l.Spec.HolderIdentity
				}
				if (holder == "") == tc.wantEmpty {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if tc.wantEmpty && holder != "" {
				t.Errorf("a completed fence must free the Lease for an immediate handoff; holder is still %q", holder)
			}
			if !tc.wantEmpty && holder != "pod-0" {
				t.Errorf("an incomplete fence must leave the Lease held so the peer waits out the TTL; holder = %q", holder)
			}
		})
	}
}

func TestK8sDCSStartsAsFollower(t *testing.T) {
	k := NewK8sDCSWithClient(K8sConfig{Namespace: "ns", LeaseName: "x"}, fake.NewSimpleClientset())
	if k.IsLeader() {
		t.Error("IsLeader() should be false before Run")
	}
	if k.Leader() != "" {
		t.Errorf("Leader() = %q, want empty before Run", k.Leader())
	}
}

// #298 review: a Release that lands while Run is WAITING OUT a cooldown must still be
// honoured. The old order read cooldownUntil at the top of the loop and only then installed
// the cancel hook, so a Release arriving in between (the cooldown wait is the wide part of
// that window) armed a new cooldown that had already been read and discarded, and found no
// cancel to call -- so the node walked into the election and could re-win the very Lease it
// was handing over. Single node, so any acquisition is immediate absent a cooldown: if the
// second Release is dropped this node is leader as soon as the FIRST cooldown expires.
func TestK8sDCSReleaseDuringCooldownIsNotDropped(t *testing.T) {
	const cooldown = 2 * time.Second
	k := NewK8sDCSWithClient(K8sConfig{
		Namespace: "ns", LeaseName: "pg-leader",
		LeaseDuration: 2 * time.Second, RenewDeadline: 1 * time.Second, RetryPeriod: 200 * time.Millisecond,
		StepDownCooldown: cooldown,
	}, fake.NewSimpleClientset())

	// Arm cooldown #1 before Run, so the loop's first act is to wait it out.
	k.Release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	go k.Run(ctx, "pod-0", Callbacks{})

	// Land cooldown #2 halfway through cooldown #1's wait: it must extend the suppression.
	time.Sleep(cooldown / 2)
	k.Release()
	secondExpiry := time.Now().Add(cooldown)

	// After #1 expires but comfortably before #2 does, this node must still not be leader.
	time.Sleep(time.Until(start.Add(cooldown).Add(400 * time.Millisecond)))
	if k.IsLeader() {
		t.Fatalf("re-won leadership %v in, while the second cooldown had until %v: the Release during the wait was dropped",
			time.Since(start), time.Until(secondExpiry))
	}

	// And it must eventually contend again -- a suppression that never lifts is its own bug.
	for i := 0; i < 100; i++ {
		if k.IsLeader() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("never re-acquired leadership after both cooldowns expired")
}

// #298 review: the chart's PGHAAgentLeaseRenewFailing alert rates
// pg_ha_agent_renew_failures_total, and until OnRenewFailure was wired nothing ever
// incremented it -- the alert could not fire. An INVOLUNTARY loss (the apiserver
// refusing renew writes past RenewDeadline) must fire the callback.
func TestK8sDCSRenewFailureFiresOnInvoluntaryLoss(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var failing atomic.Bool
	cs.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failing.Load() {
			return true, nil, errors.New("apiserver unreachable (injected)")
		}
		return false, nil, nil
	})
	k := NewK8sDCSWithClient(K8sConfig{
		Namespace: "ns", LeaseName: "pg-leader",
		LeaseDuration: 2 * time.Second, RenewDeadline: 1 * time.Second, RetryPeriod: 200 * time.Millisecond,
	}, cs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	acquired := make(chan struct{}, 1)
	renewFailed := make(chan struct{}, 1)
	go k.Run(ctx, "pod-0", Callbacks{
		OnAcquired: func(context.Context) {
			select {
			case acquired <- struct{}{}:
			default:
			}
		},
		OnRenewFailure: func() {
			select {
			case renewFailed <- struct{}{}:
			default:
			}
		},
	})

	select {
	case <-acquired:
	case <-time.After(15 * time.Second):
		t.Fatal("never acquired leadership")
	}
	failing.Store(true) // every renew write now fails; RenewDeadline expires
	select {
	case <-renewFailed:
	case <-time.After(15 * time.Second):
		t.Fatal("involuntary lease loss never fired OnRenewFailure")
	}
}
