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

func TestK8sDCSStartsAsFollower(t *testing.T) {
	k := NewK8sDCSWithClient(K8sConfig{Namespace: "ns", LeaseName: "x"}, fake.NewSimpleClientset())
	if k.IsLeader() {
		t.Error("IsLeader() should be false before Run")
	}
	if k.Leader() != "" {
		t.Errorf("Leader() = %q, want empty before Run", k.Leader())
	}
}
