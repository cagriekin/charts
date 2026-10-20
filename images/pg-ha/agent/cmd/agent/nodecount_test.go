package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/cagriekin/pg-ha-agent/internal/config"
	"github.com/cagriekin/pg-ha-agent/internal/k8s"
	"github.com/cagriekin/pg-ha-agent/internal/observe"
)

func nodeCountAgent(t *testing.T, objs ...runtime.Object) *agent {
	t.Helper()
	cs := k8sfake.NewSimpleClientset(objs...)
	return &agent{
		cfg:  &config.Config{PodName: "pg-0", LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second},
		base: "pg",
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		kube: k8s.NewWithClient(cs, "ns"),
		metr: observe.New(),
	}
}

func sts(replicas int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: "ns"},
		Spec:       appsv1.StatefulSetSpec{Replicas: &replicas},
	}
}

// #373: the node count is spec.replicas of the agent's own StatefulSet, read each tick, so a
// scale changes what the agent probes and pre-creates without any pod rolling.
func TestRefreshNodeCountFollowsSpecReplicas(t *testing.T) {
	a := nodeCountAgent(t, sts(3))
	a.cfg.NodeCount = 2 // a stale env fallback must lose to the live read
	a.refreshNodeCount(context.Background())
	if got := a.expectedNodes(); got != 3 {
		t.Fatalf("expectedNodes = %d, want spec.replicas 3", got)
	}
	// Scale up 3 -> 5: the next tick sees it.
	a.kube = k8s.NewWithClient(k8sfake.NewSimpleClientset(sts(5)), "ns")
	a.refreshNodeCount(context.Background())
	if got := a.expectedNodes(); got != 5 {
		t.Fatalf("expectedNodes after scale = %d, want 5", got)
	}
}

// Before the first successful read the env is the fallback; after a read, a failed Get
// keeps the last value rather than dropping to the env or to zero.
func TestExpectedNodesFallsBackToTheEnvThenHoldsTheLastRead(t *testing.T) {
	a := nodeCountAgent(t) // no StatefulSet: every Get fails
	a.cfg.NodeCount = 2
	a.refreshNodeCount(context.Background())
	if got := a.expectedNodes(); got != 2 {
		t.Fatalf("expectedNodes with no readable StatefulSet = %d, want the env 2", got)
	}
	if !a.nodeCountWarned {
		t.Fatal("a failed read must latch the warning")
	}
	a.kube = k8s.NewWithClient(k8sfake.NewSimpleClientset(sts(4)), "ns")
	a.refreshNodeCount(context.Background())
	if got := a.expectedNodes(); got != 4 || a.nodeCountWarned {
		t.Fatalf("expectedNodes = %d warned=%v, want 4 and the latch cleared", got, a.nodeCountWarned)
	}
	a.kube = k8s.NewWithClient(k8sfake.NewSimpleClientset(), "ns")
	a.refreshNodeCount(context.Background())
	if got := a.expectedNodes(); got != 4 {
		t.Fatalf("expectedNodes after a failed re-read = %d, want the last read 4", got)
	}
}

// No env and no readable StatefulSet: nothing to probe, and never a negative length (#298).
func TestExpectedNodesIsNeverNegative(t *testing.T) {
	a := nodeCountAgent(t)
	a.cfg.NodeCount = -1
	if got := a.expectedNodes(); got != 0 {
		t.Fatalf("expectedNodes = %d, want 0", got)
	}
}

// refreshNodeCount's result is "is a node count known from any source": a failed first read
// with no env is false, so tick() skips rather than planning on zero peers -- which the
// planner would read as a single-node cluster, not as unknown (#373 review).
func TestRefreshNodeCountReportsWhetherACountIsKnown(t *testing.T) {
	a := nodeCountAgent(t) // no StatefulSet: the Get fails
	if a.refreshNodeCount(context.Background()) {
		t.Fatal("no StatefulSet and no env must report the count as unknown")
	}
	a.cfg.NodeCount = 2
	if !a.refreshNodeCount(context.Background()) {
		t.Fatal("the env fallback is a known count")
	}
	a.cfg.NodeCount = 0
	a.kube = k8s.NewWithClient(k8sfake.NewSimpleClientset(sts(3)), "ns")
	if !a.refreshNodeCount(context.Background()) {
		t.Fatal("a successful read is a known count")
	}
	a.kube = k8s.NewWithClient(k8sfake.NewSimpleClientset(), "ns")
	if !a.refreshNodeCount(context.Background()) || a.expectedNodes() != 3 {
		t.Fatalf("a failed re-read keeps the last count known; expectedNodes=%d", a.expectedNodes())
	}
}
