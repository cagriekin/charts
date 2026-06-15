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
	if got := k.Leader(); got != "pod-0" {
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
