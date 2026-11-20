package main

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cagriekin/pg-ha-agent/internal/config"
	"github.com/cagriekin/pg-ha-agent/internal/k8s"
	"github.com/cagriekin/pg-ha-agent/internal/reconcile"
)

// Chart-default timing: settle 15s, window 50s.
func handoffTestAgent(t *testing.T, self string, anns map[string]string) (*agent, *fakePostmaster, *fakeDCS, *k8sfake.Clientset) {
	t.Helper()
	pm := &fakePostmaster{}
	d := &fakeDCS{leader: true}
	a := newTestAgent(t, pm, d)
	a.cfg.PodName = self
	a.cfg.MarkerName = "pg-primary"
	a.cfg.ReconcileInterval = 5 * time.Second
	a.cfg.LeaseDuration = 15 * time.Second
	a.cfg.RenewDeadline = 10 * time.Second
	a.cfg.RetryPeriod = 2 * time.Second
	cs := k8sfake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-primary", Namespace: "ns", Annotations: anns},
		Data:       map[string]string{"primary": "pg-0", "timeline": "1"},
	})
	a.kube = k8s.NewWithClient(cs, "ns")
	return a, pm, d, cs
}

func markerAnn(t *testing.T, cs *k8sfake.Clientset, key string) (string, bool) {
	t.Helper()
	cm, err := cs.CoreV1().ConfigMaps("ns").Get(context.Background(), "pg-primary", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	v, ok := cm.Annotations[key]
	return v, ok
}

var t0 = time.Unix(1_700_000_000, 0)

func TestHandoffWindowOnChartDefaults(t *testing.T) {
	a, _, _, _ := handoffTestAgent(t, "pg-0", nil)
	if a.handoffSettle() != 15*time.Second || a.handoffWindow() != 50*time.Second {
		t.Fatalf("settle=%v window=%v, want 15s/50s", a.handoffSettle(), a.handoffWindow())
	}
}

// #389: a fresh handoff to another pod parks every member but its target; a stale, future-
// dated or unparseable one parks nobody.
func TestHandoffExcludes(t *testing.T) {
	a, _, _, _ := handoffTestAgent(t, "pg-2", nil)
	a.handoffNow = func() time.Time { return t0 }
	for _, c := range []struct {
		name string
		h    k8s.Handoff
		node string
		want bool
	}{
		{"no stamp", k8s.Handoff{}, "pg-2", false},
		{"the target contends", k8s.Handoff{Target: "pg-1", Start: t0, OK: true, Present: true}, "pg-1", false},
		{"another member parks", k8s.Handoff{Target: "pg-1", Start: t0.Add(-10 * time.Second), OK: true, Present: true}, "pg-2", true},
		{"the old primary parks too", k8s.Handoff{Target: "pg-1", Start: t0.Add(-30 * time.Second), OK: true, Present: true}, "pg-0", true},
		{"expired", k8s.Handoff{Target: "pg-1", Start: t0.Add(-50 * time.Second), OK: true, Present: true}, "pg-2", false},
		{"slightly ahead (clock skew)", k8s.Handoff{Target: "pg-1", Start: t0.Add(2 * time.Second), OK: true, Present: true}, "pg-2", true},
		{"far future (not an agent's)", k8s.Handoff{Target: "pg-1", Start: t0.Add(time.Hour), OK: true, Present: true}, "pg-2", false},
		{"unparseable", k8s.Handoff{Present: true}, "pg-2", false},
	} {
		a.handoff = c.h
		if got := a.handoffExcludes(c.node); got != c.want {
			t.Errorf("%s: excludes(%s) = %v, want %v", c.name, c.node, got, c.want)
		}
	}
}

// Phase 1: the first Switchover tick stamps the marker and keeps serving -- no demote, no
// release, the request still standing.
func TestSwitchoverStampsBeforeSteppingDown(t *testing.T) {
	a, pm, d, cs := handoffTestAgent(t, "pg-0", map[string]string{k8s.SwitchoverTargetAnnotation: "pg-1"})
	a.handoffNow = func() time.Time { return t0 }
	obs := reconcile.Observation{Local: reconcile.LocalState{HasData: true, Running: true}, SwitchoverTarget: "pg-1"}
	if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.Switchover, Target: "pg-1"}, obs); err != nil {
		t.Fatal(err)
	}
	if pm.stopped || d.released {
		t.Fatal("phase 1 must not step down")
	}
	v, ok := markerAnn(t, cs, k8s.SwitchoverHandoffAnnotation)
	if h := k8s.ParseHandoff(v); !ok || !h.OK || h.Target != "pg-1" || !h.Start.Equal(t0) {
		t.Fatalf("handoff annotation = %q", v)
	}
	if _, ok := markerAnn(t, cs, k8s.SwitchoverTargetAnnotation); !ok {
		t.Fatal("the request must stand until the step-down")
	}
}

// While settling, nothing happens; once settled, the request is cleared and the primary
// steps down, leaving the stamp for the target's window.
func TestSwitchoverStepsDownOnceSettled(t *testing.T) {
	a, pm, d, cs := handoffTestAgent(t, "pg-0", map[string]string{
		k8s.SwitchoverTargetAnnotation:  "pg-1",
		k8s.SwitchoverHandoffAnnotation: k8s.FormatHandoff("pg-1", t0),
	})
	a.handoff = k8s.Handoff{Target: "pg-1", Start: t0, OK: true, Present: true}
	obs := reconcile.Observation{Local: reconcile.LocalState{HasData: true, Running: true}, SwitchoverTarget: "pg-1"}
	dec := reconcile.Decision{Action: reconcile.Switchover, Target: "pg-1"}

	a.handoffNow = func() time.Time { return t0.Add(10 * time.Second) }
	if err := a.act(context.Background(), dec, obs); err != nil {
		t.Fatal(err)
	}
	if pm.stopped || d.released {
		t.Fatal("must not step down while the other members may still contend")
	}
	if v, _ := markerAnn(t, cs, k8s.SwitchoverHandoffAnnotation); v != k8s.FormatHandoff("pg-1", t0) {
		t.Fatalf("a settling stamp must not be rewritten (that would restart the settle): %q", v)
	}

	a.handoffNow = func() time.Time { return t0.Add(16 * time.Second) }
	if err := a.act(context.Background(), dec, obs); err != nil {
		t.Fatal(err)
	}
	if !pm.stopped || !d.released {
		t.Fatal("a settled handoff must demote and release")
	}
	if _, ok := markerAnn(t, cs, k8s.SwitchoverTargetAnnotation); ok {
		t.Fatal("the request must be cleared at the step-down (one-shot)")
	}
	if _, ok := markerAnn(t, cs, k8s.SwitchoverHandoffAnnotation); !ok {
		t.Fatal("the stamp must outlive the step-down: it is what parks the other members")
	}
}

// A stamp past the step-down slot (the target fell behind for a while) or for another target
// is restamped, not acted on.
func TestSwitchoverRestampsAStaleOrForeignStamp(t *testing.T) {
	for _, h := range []k8s.Handoff{
		{Target: "pg-1", Start: t0.Add(-40 * time.Second), OK: true, Present: true},
		{Target: "pg-2", Start: t0.Add(-16 * time.Second), OK: true, Present: true},
	} {
		a, pm, d, cs := handoffTestAgent(t, "pg-0", map[string]string{k8s.SwitchoverTargetAnnotation: "pg-1"})
		a.handoff = h
		a.handoffNow = func() time.Time { return t0 }
		obs := reconcile.Observation{Local: reconcile.LocalState{HasData: true, Running: true}, SwitchoverTarget: "pg-1"}
		if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.Switchover, Target: "pg-1"}, obs); err != nil {
			t.Fatal(err)
		}
		if pm.stopped || d.released {
			t.Fatalf("stamp %+v: must restamp, not step down", h)
		}
		if v, _ := markerAnn(t, cs, k8s.SwitchoverHandoffAnnotation); v != k8s.FormatHandoff("pg-1", t0) {
			t.Fatalf("stamp %+v: annotation = %q", h, v)
		}
	}
}

// The serving primary retires a stamp whose handoff is over, and keeps one it is still
// working on.
func TestRetireHandoff(t *testing.T) {
	for _, c := range []struct {
		name    string
		self    string
		target  string // the standing request
		h       k8s.Handoff
		removed bool
	}{
		{"the target, promoted", "pg-1", "", k8s.Handoff{Target: "pg-1", Start: t0, OK: true, Present: true}, true},
		{"a request cancelled mid-settle", "pg-0", "", k8s.Handoff{Target: "pg-1", Start: t0, OK: true, Present: true}, true},
		{"a request replaced", "pg-0", "pg-2", k8s.Handoff{Target: "pg-1", Start: t0, OK: true, Present: true}, true},
		{"unparseable", "pg-0", "", k8s.Handoff{Present: true}, true},
		{"still in progress", "pg-0", "pg-1", k8s.Handoff{Target: "pg-1", Start: t0, OK: true, Present: true}, false},
	} {
		a, _, _, cs := handoffTestAgent(t, c.self, map[string]string{k8s.SwitchoverHandoffAnnotation: "x"})
		a.handoff = c.h
		a.retireHandoff(context.Background(), reconcile.Observation{SwitchoverTarget: c.target})
		if _, ok := markerAnn(t, cs, k8s.SwitchoverHandoffAnnotation); ok == c.removed {
			t.Errorf("%s: annotation present=%v, want removed=%v", c.name, ok, c.removed)
		}
	}
}

// A member booting mid-handoff parks before its election starts, unless it is the target.
func TestInitialCandidacyParksForAHandoff(t *testing.T) {
	for _, c := range []struct {
		self     string
		wantPark bool
	}{{"pg-2", true}, {"pg-1", false}} {
		f := &fakeDCS{}
		cs := k8sfake.NewSimpleClientset(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-primary", Namespace: "ns", Annotations: map[string]string{
				k8s.SwitchoverHandoffAnnotation: k8s.FormatHandoff("pg-1", t0),
			}},
			Data: map[string]string{"primary": "pg-0", "timeline": "1"},
		})
		a := &agent{
			cfg: &config.Config{PodName: c.self, MarkerName: "pg-primary", ReconcileInterval: 5 * time.Second,
				LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second},
			dcs:        f,
			kube:       k8s.NewWithClient(cs, "ns"),
			handoffNow: func() time.Time { return t0.Add(5 * time.Second) },
		}
		a.initialCandidacy(context.Background())
		if parked := f.candidacySet && !f.candidate; parked != c.wantPark {
			t.Errorf("%s: parked=%v, want %v", c.self, parked, c.wantPark)
		}
	}
}

// #389 review: a non-target that was already contending when the stamp landed can still win
// the Lease the primary frees. The promote re-reads the marker and hands the Lease on instead.
func TestPromoteHandoffGuard(t *testing.T) {
	stamp := map[string]string{k8s.SwitchoverHandoffAnnotation: k8s.FormatHandoff("pg-1", t0.Add(-16*time.Second))}
	t.Run("a non-target releases instead of promoting, despite a stale cached view", func(t *testing.T) {
		a, _, d, _ := handoffTestAgent(t, "pg-2", stamp)
		a.handoffNow = func() time.Time { return t0 }
		a.markerReadThisTick = true
		if a.promoteHandoffAllows(context.Background()) {
			t.Fatal("pg-2 must not promote past a fresh handoff to pg-1")
		}
		if !d.released || !d.candidacySet || d.candidate {
			t.Fatalf("released=%v candidacySet=%v candidate=%v, want a parked release", d.released, d.candidacySet, d.candidate)
		}
		if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.Promote}, reconcile.Observation{}); err != nil {
			t.Fatalf("act: %v", err)
		}
		if a.servingRW.Load() {
			t.Fatal("the promote must not arm the read-write latch on a non-target")
		}
	})
	t.Run("the target promotes", func(t *testing.T) {
		a, _, d, _ := handoffTestAgent(t, "pg-1", stamp)
		a.handoffNow = func() time.Time { return t0 }
		a.markerReadThisTick = true
		if !a.promoteHandoffAllows(context.Background()) || d.released {
			t.Fatal("the named target must promote")
		}
	})
	t.Run("an expired stamp blocks nobody", func(t *testing.T) {
		a, _, _, _ := handoffTestAgent(t, "pg-2", stamp)
		a.handoffNow = func() time.Time { return t0.Add(time.Minute) }
		a.markerReadThisTick = true
		if !a.promoteHandoffAllows(context.Background()) {
			t.Fatal("an expired handoff must not block a failover")
		}
	})
	failReads := func(cs *k8sfake.Clientset) {
		cs.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
	}
	t.Run("a failed re-read behind a reused marker holds off", func(t *testing.T) {
		a, _, d, cs := handoffTestAgent(t, "pg-2", nil)
		a.handoffNow = func() time.Time { return t0 }
		failReads(cs)
		a.markerReadThisTick = false
		if a.promoteHandoffAllows(context.Background()) || d.released {
			t.Fatal("with no fresh marker this tick the promote waits, keeping the Lease")
		}
	})
	t.Run("a failed re-read after a fresh observe trusts this tick's copy", func(t *testing.T) {
		a, _, _, cs := handoffTestAgent(t, "pg-2", nil)
		a.handoffNow = func() time.Time { return t0 }
		failReads(cs)
		a.markerReadThisTick = true
		if !a.promoteHandoffAllows(context.Background()) {
			t.Fatal("no stamp in this tick's fresh read: promote")
		}
		a.handoff = k8s.ParseHandoff(stamp[k8s.SwitchoverHandoffAnnotation])
		if a.promoteHandoffAllows(context.Background()) {
			t.Fatal("a fresh stamp in this tick's read still blocks a non-target")
		}
	})
}

// #389 review: a lease lost during the handoff re-read must stop the promote before the
// read-write latch is armed.
func TestActPromoteRechecksTheLeaseAfterTheHandoffRead(t *testing.T) {
	a, _, d, _ := handoffTestAgent(t, "pg-1", nil)
	d.leaderSeq = []bool{true, false}
	a.markerReadThisTick = true
	if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.Promote}, reconcile.Observation{}); err != nil {
		t.Fatalf("act: %v", err)
	}
	if a.servingRW.Load() {
		t.Fatal("a node that lost the lease during the handoff re-read must not arm the read-write latch")
	}
}
