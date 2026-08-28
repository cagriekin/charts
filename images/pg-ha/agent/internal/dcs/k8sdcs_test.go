package dcs

import (
	"context"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
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
	go a.Run(ctx, "pod-0", Callbacks{OnLost: func() { lost <- "pod-0" }})
	go b.Run(ctx, "pod-1", Callbacks{OnLost: func() { lost <- "pod-1" }})

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
