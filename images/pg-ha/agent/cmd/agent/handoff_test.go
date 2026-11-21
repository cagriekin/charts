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

// Chart-default timing: settle 15s, step-down slot 20s, window 60s.
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
	a.prober = execProber(&slotExec{})
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
	if a.handoffSettle() != 15*time.Second || a.handoffStepDownSlot() != 20*time.Second || a.handoffWindow() != 60*time.Second {
		t.Fatalf("settle=%v slot=%v window=%v, want 15s/20s/60s", a.handoffSettle(), a.handoffStepDownSlot(), a.handoffWindow())
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
		{"expired", k8s.Handoff{Target: "pg-1", Start: t0.Add(-60 * time.Second), OK: true, Present: true}, "pg-2", false},
		{"slightly ahead (clock skew)", k8s.Handoff{Target: "pg-1", Start: t0.Add(2 * time.Second), OK: true, Present: true}, "pg-2", true},
		{"ahead by more than the skew allowance", k8s.Handoff{Target: "pg-1", Start: t0.Add(10 * time.Second), OK: true, Present: true}, "pg-2", false},
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

	// The settling tick runs the primary's upkeep; its routing write fails on this fake
	// cluster (no Services), which is beside the point here.
	a.handoffNow = func() time.Time { return t0.Add(10 * time.Second) }
	_ = a.act(context.Background(), dec, obs)
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

// A stamp that aged past the step-down slot between observe() and act() is not restamped on
// the lag slack Decide applied to it: the next tick re-runs the strict catch-up check.
func TestSwitchoverDoesNotRestampOnTheStampedSlack(t *testing.T) {
	stale := k8s.Handoff{Target: "pg-1", Start: t0.Add(-40 * time.Second), OK: true, Present: true}
	a, pm, d, cs := handoffTestAgent(t, "pg-0", map[string]string{
		k8s.SwitchoverTargetAnnotation:  "pg-1",
		k8s.SwitchoverHandoffAnnotation: k8s.FormatHandoff(stale.Target, stale.Start),
	})
	a.handoff = stale
	a.handoffNow = func() time.Time { return t0 }
	obs := reconcile.Observation{Local: reconcile.LocalState{HasData: true, Running: true}, SwitchoverTarget: "pg-1", SwitchoverStamped: true}
	_ = a.act(context.Background(), reconcile.Decision{Action: reconcile.Switchover, Target: "pg-1"}, obs)
	if pm.stopped || d.released {
		t.Fatal("must not step down on a stamp past its slot")
	}
	if v, _ := markerAnn(t, cs, k8s.SwitchoverHandoffAnnotation); v != k8s.FormatHandoff(stale.Target, stale.Start) {
		t.Fatalf("restamped on the slack: %q", v)
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

// #389 review: a tick that reused lastMarker re-reads it before promoting. A fresh stamp
// naming another pod hands the lease on; a failed re-read holds off only when the last good
// read showed a switchover in progress, so an apiserver outage never stalls a failover.
func TestPromoteHandoffGuard(t *testing.T) {
	stamp := map[string]string{k8s.SwitchoverHandoffAnnotation: k8s.FormatHandoff("pg-1", t0.Add(-16*time.Second))}
	t.Run("a non-target behind a reused marker releases instead of promoting", func(t *testing.T) {
		a, _, d, _ := handoffTestAgent(t, "pg-2", stamp)
		a.handoffNow = func() time.Time { return t0 }
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
		if !a.promoteHandoffAllows(context.Background()) || d.released {
			t.Fatal("the named target must promote")
		}
	})
	t.Run("an expired stamp blocks nobody", func(t *testing.T) {
		a, _, _, _ := handoffTestAgent(t, "pg-2", stamp)
		a.handoffNow = func() time.Time { return t0.Add(time.Minute) }
		if !a.promoteHandoffAllows(context.Background()) {
			t.Fatal("an expired handoff must not block a failover")
		}
	})
	t.Run("a tick that read the marker itself is not re-read", func(t *testing.T) {
		a, _, _, cs := handoffTestAgent(t, "pg-2", stamp)
		a.handoffNow = func() time.Time { return t0 }
		a.markerReadThisTick = true
		cs.ClearActions()
		if !a.promoteHandoffAllows(context.Background()) || len(cs.Actions()) != 0 {
			t.Fatalf("Decide already saw this tick's marker; want no re-read, got %d calls", len(cs.Actions()))
		}
	})
	failReads := func(cs *k8sfake.Clientset) {
		cs.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
	}
	t.Run("a failed re-read with no switchover on record promotes", func(t *testing.T) {
		a, _, d, cs := handoffTestAgent(t, "pg-2", nil)
		a.handoffNow = func() time.Time { return t0 }
		a.lastMarker, a.lastMarkerOK = k8s.Marker{Present: true, Primary: "pg-0"}, true
		failReads(cs)
		if !a.promoteHandoffAllows(context.Background()) || d.released {
			t.Fatal("an apiserver outage must not stall an ordinary failover")
		}
	})
	t.Run("a failed re-read with a switchover on record holds off", func(t *testing.T) {
		a, _, d, cs := handoffTestAgent(t, "pg-2", nil)
		a.handoffNow = func() time.Time { return t0 }
		a.lastMarker, a.lastMarkerOK = k8s.Marker{Present: true, Primary: "pg-0", SwitchoverTarget: "pg-1"}, true
		failReads(cs)
		if a.promoteHandoffAllows(context.Background()) || d.released {
			t.Fatal("with a switchover on record the promote waits a tick, keeping the lease")
		}
	})
	t.Run("a failed re-read does not hold off the target the request names", func(t *testing.T) {
		a, _, d, cs := handoffTestAgent(t, "pg-1", nil)
		a.handoffNow = func() time.Time { return t0 }
		a.lastMarker, a.lastMarkerOK = k8s.Marker{Present: true, Primary: "pg-0", SwitchoverTarget: "pg-1"}, true
		failReads(cs)
		if !a.promoteHandoffAllows(context.Background()) || d.released {
			t.Fatal("the named target must promote: the request on record is the one it is completing")
		}
	})
	t.Run("a failed re-read holds off for a fresh stamp but not an expired one", func(t *testing.T) {
		h := k8s.ParseHandoff(stamp[k8s.SwitchoverHandoffAnnotation])
		for _, tc := range []struct {
			now  time.Time
			want bool
		}{{t0, false}, {t0.Add(time.Minute), true}} {
			a, _, _, cs := handoffTestAgent(t, "pg-2", nil)
			a.handoffNow = func() time.Time { return tc.now }
			a.lastMarker, a.lastMarkerOK = k8s.Marker{Present: true, Primary: "pg-0", Handoff: h}, true
			a.handoff = h
			failReads(cs)
			if got := a.promoteHandoffAllows(context.Background()); got != tc.want {
				t.Fatalf("now=%v: promote allowed=%v, want %v", tc.now, got, tc.want)
			}
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

// #389 review: the settle counts from when this node's stamp write returned, so a slow write
// does not shorten it; the step-down slot's end stays on the stamp's own time.
func TestHandoffSettleCountsFromTheWrite(t *testing.T) {
	a, _, _, _ := handoffTestAgent(t, "pg-0", nil)
	h := k8s.Handoff{Target: "pg-1", Start: t0, OK: true, Present: true}
	a.handoff, a.handoffWrote, a.handoffWroteAt = h, h, t0.Add(4*time.Second)
	for _, c := range []struct {
		at   time.Duration
		want handoffStep
	}{
		{16 * time.Second, handoffSettling},
		{19 * time.Second, handoffStepDown},
		{35 * time.Second, handoffStepDown},
		{36 * time.Second, handoffStamp},
	} {
		a.handoffNow = func() time.Time { return t0.Add(c.at) }
		if got := a.handoffPhase("pg-1"); got != c.want {
			t.Errorf("at +%v: phase %v, want %v", c.at, got, c.want)
		}
	}
	a.handoffWrote = k8s.Handoff{}
	a.handoffNow = func() time.Time { return t0.Add(16 * time.Second) }
	if got := a.handoffPhase("pg-1"); got != handoffStepDown {
		t.Errorf("a stamp this process did not write settles from its own time: phase %v", got)
	}
}

// #389 review: at any sustained tick spacing the agent survives (up to the /healthz
// staleness bound of three intervals) and any stamp write within its budget, some call lands
// in the step-down interval before the slot ends -- the switchover never restamps forever.
func TestHandoffStepDownReachableAtAnyTickSpacing(t *testing.T) {
	a, _, _, _ := handoffTestAgent(t, "pg-0", nil)
	h := k8s.Handoff{Target: "pg-1", Start: t0, OK: true, Present: true}
	for w := time.Duration(0); w <= a.fenceBudget(); w += 250 * time.Millisecond {
		for s := max(w, 250*time.Millisecond); s <= 3*a.cfg.ReconcileInterval; s += 250 * time.Millisecond {
			a.handoff, a.handoffWrote, a.handoffWroteAt = h, h, t0.Add(w)
			reached := false
			for at := s; !reached; at += s {
				a.handoffNow = func() time.Time { return t0.Add(at) }
				switch a.handoffPhase("pg-1") {
				case handoffStepDown:
					reached = true
				case handoffStamp:
					t.Fatalf("write %v, spacing %v: call at +%v restamps without a step-down", w, s, at)
				}
			}
		}
	}
}

// #389 review: stamping records the write, so the same tick's upkeep and the next tick see it.
func TestSwitchoverStampRecordsTheWrite(t *testing.T) {
	a, _, _, _ := handoffTestAgent(t, "pg-0", map[string]string{k8s.SwitchoverTargetAnnotation: "pg-1"})
	a.handoffNow = func() time.Time { return t0 }
	obs := reconcile.Observation{Local: reconcile.LocalState{HasData: true, Running: true}, SwitchoverTarget: "pg-1"}
	if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.Switchover, Target: "pg-1"}, obs); err != nil {
		t.Fatal(err)
	}
	if a.handoff.Target != "pg-1" || !a.handoff.Start.Equal(t0) || a.handoffWrote != a.handoff || !a.handoffWroteAt.Equal(t0) {
		t.Fatalf("handoff=%+v wrote=%+v at %v", a.handoff, a.handoffWrote, a.handoffWroteAt)
	}
}

// #389 review: the serving primary keeps contending through its own handoff; once demoted it
// parks like every other non-target.
func TestServingPrimaryContendsThroughItsHandoff(t *testing.T) {
	a, _, _, _ := handoffTestAgent(t, "pg-0", nil)
	a.handoffNow = func() time.Time { return t0 }
	a.handoff = k8s.Handoff{Target: "pg-1", Start: t0.Add(-5 * time.Second), OK: true, Present: true}
	o := reconcile.Observation{LocalNode: "pg-0"}
	a.servingRW.Store(true)
	if !a.contends(o) {
		t.Fatal("the serving primary must keep contending while its handoff settles")
	}
	a.servingRW.Store(false)
	if a.contends(o) {
		t.Fatal("a demoted primary parks for the rest of the handoff")
	}
}
