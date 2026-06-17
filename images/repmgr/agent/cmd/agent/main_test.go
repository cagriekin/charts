package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cagriekin/pg-ha-agent/internal/config"
	"github.com/cagriekin/pg-ha-agent/internal/dcs"
	"github.com/cagriekin/pg-ha-agent/internal/mechanism"
	"github.com/cagriekin/pg-ha-agent/internal/observe"
	"github.com/cagriekin/pg-ha-agent/internal/pg"
	"github.com/cagriekin/pg-ha-agent/internal/process"
	"github.com/cagriekin/pg-ha-agent/internal/reconcile"
)

// --- fakes for the act() path ---

type fakePostmaster struct {
	started  bool
	stopped  bool
	stopMode process.StopMode
	running  bool
}

func (f *fakePostmaster) Start(context.Context) error { f.started = true; return nil }
func (f *fakePostmaster) Stop(_ context.Context, m process.StopMode) error {
	f.stopped, f.stopMode = true, m
	return nil
}
func (f *fakePostmaster) Reload(context.Context) error { return nil }
func (f *fakePostmaster) Running() bool                { return f.running }

type fakeDCS struct{ released bool }

func (f *fakeDCS) Run(context.Context, string, dcs.Callbacks) {}
func (f *fakeDCS) IsLeader() bool                             { return false }
func (f *fakeDCS) Leader() string                             { return "" }
func (f *fakeDCS) Release()                                   { f.released = true }

// scriptedExec backs BOTH mechanism.Runner (repmgr CLI) and pg.Exec (psql) -- the
// signatures are identical -- so one fake drives the whole Follow act() path. It
// counts repmgr standby follow invocations and stubs the pg_stat_wal_receiver probe.
type scriptedExec struct {
	walRcv  string // pg_stat_wal_receiver "sender_host|status" output (psql)
	follows int    // number of `repmgr standby follow` calls
}

func (s *scriptedExec) Run(_ context.Context, _ []string, name string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	switch {
	case name == "psql" && strings.Contains(joined, "pg_stat_wal_receiver"):
		return s.walRcv, nil
	case name == "repmgr" && strings.Contains(joined, "standby follow"):
		s.follows++
		return "ok", nil
	}
	return "ok", nil
}

// newFollowTestAgent wires a real Repmgr + Prober backed by one scriptedExec so the
// Follow act() path (register -> streaming probe -> follow) is exercised end to end.
func newFollowTestAgent(t *testing.T, ex *scriptedExec) *agent {
	t.Helper()
	m := mechanism.NewRepmgr("/etc/repmgr/repmgr.conf", t.TempDir(), "pw")
	m.Runner = ex
	return &agent{
		cfg: &config.Config{
			PGDATA:          t.TempDir(),
			HeadlessService: "h",
			RepmgrUser:      "repmgr",
			RepmgrDB:        "repmgr",
			RepmgrPassword:  "pw",
			RenewDeadline:   2 * time.Second,
		},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		dcs:    &fakeDCS{},
		mech:   m,
		prober: &pg.Prober{Exec: ex, Timeout: time.Second},
		sup:    process.NewSupervisor(&fakePostmaster{}),
		metr:   observe.New(),
	}
}

// #182: a standby already streaming from the lease holder must NOT re-run repmgr
// standby follow (which errors "slot already active" and, unlatched, repeats every
// tick). The act path skips the command and latches followUpstream.
func TestActFollowSkipsWhenAlreadyStreaming(t *testing.T) {
	ex := &scriptedExec{walRcv: "pg-0.h|streaming"}
	a := newFollowTestAgent(t, ex)
	dec := reconcile.Decision{Action: reconcile.Follow, Target: "pg-0"}
	if err := a.act(context.Background(), dec, reconcile.Observation{}); err != nil {
		t.Fatalf("act: %v", err)
	}
	if ex.follows != 0 {
		t.Fatalf("repmgr standby follow must be skipped when already streaming, got %d calls", ex.follows)
	}
	if a.followUpstream != "pg-0" {
		t.Fatalf("followUpstream must latch to skip future ticks, got %q", a.followUpstream)
	}
}

// A standby not yet streaming (or being repointed to a new upstream) must run
// repmgr standby follow, then latch.
func TestActFollowRunsWhenNotStreaming(t *testing.T) {
	ex := &scriptedExec{walRcv: ""} // no walreceiver row
	a := newFollowTestAgent(t, ex)
	dec := reconcile.Decision{Action: reconcile.Follow, Target: "pg-0"}
	if err := a.act(context.Background(), dec, reconcile.Observation{}); err != nil {
		t.Fatalf("act: %v", err)
	}
	if ex.follows != 1 {
		t.Fatalf("repmgr standby follow must run when not streaming, got %d calls", ex.follows)
	}
	if a.followUpstream != "pg-0" {
		t.Fatal("followUpstream must latch after a successful follow")
	}
}

// Streaming from a DIFFERENT host than the target (a stale upstream after a leader
// change) must NOT be mistaken for already-following: the agent repoints via follow.
func TestActFollowRepointsWhenStreamingFromWrongUpstream(t *testing.T) {
	ex := &scriptedExec{walRcv: "pg-9.h|streaming"} // streaming from the old leader
	a := newFollowTestAgent(t, ex)
	dec := reconcile.Decision{Action: reconcile.Follow, Target: "pg-0"}
	if err := a.act(context.Background(), dec, reconcile.Observation{}); err != nil {
		t.Fatalf("act: %v", err)
	}
	if ex.follows != 1 {
		t.Fatalf("a standby streaming from the wrong upstream must be repointed via follow, got %d calls", ex.follows)
	}
}

// Once latched, a steady-state Follow tick is a pure no-op: no probe, no follow.
func TestActFollowShortCircuitsWhenLatched(t *testing.T) {
	ex := &scriptedExec{walRcv: "pg-0.h|streaming"}
	a := newFollowTestAgent(t, ex)
	a.followUpstream = "pg-0"
	dec := reconcile.Decision{Action: reconcile.Follow, Target: "pg-0"}
	if err := a.act(context.Background(), dec, reconcile.Observation{}); err != nil {
		t.Fatalf("act: %v", err)
	}
	if ex.follows != 0 {
		t.Fatalf("a latched standby must not re-run follow, got %d calls", ex.follows)
	}
}

// A non-Follow action resets the latch so the next Follow re-registers + repoints.
func TestActResetsFollowLatchOnNonFollow(t *testing.T) {
	a := newTestAgent(t, &fakePostmaster{}, &fakeDCS{})
	a.followUpstream = "pg-0"
	obs := reconcile.Observation{Local: reconcile.LocalState{HasData: true, Running: true, InRecovery: true}}
	if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.ReleaseLease}, obs); err != nil {
		t.Fatalf("act: %v", err)
	}
	if a.followUpstream != "" {
		t.Fatalf("followUpstream must reset on a non-Follow action, got %q", a.followUpstream)
	}
}

func newTestAgent(t *testing.T, pm *fakePostmaster, d *fakeDCS) *agent {
	t.Helper()
	return &agent{
		cfg:  &config.Config{PGDATA: t.TempDir(), RenewDeadline: 2 * time.Second},
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		dcs:  d,
		sup:  process.NewSupervisor(pm),
		metr: observe.New(),
	}
}

// StartLocal resuming on-disk primary-state data must arm servingRW synchronously
// so a lease loss during the resume window still fences (mirrors the Promote path).
func TestActStartLocalArmsServingRWForPrimaryState(t *testing.T) {
	pm := &fakePostmaster{}
	a := newTestAgent(t, pm, &fakeDCS{})
	obs := reconcile.Observation{Local: reconcile.LocalState{HasData: true, Running: false, InRecovery: false}}
	if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.StartLocal}, obs); err != nil {
		t.Fatalf("act: %v", err)
	}
	if !pm.started {
		t.Fatal("postmaster must be started")
	}
	if !a.servingRW.Load() {
		t.Fatal("servingRW must be armed synchronously when resuming primary-state data read-write")
	}
}

// A standby-state StartLocal (read-only) must NOT arm servingRW (it is not a writer).
func TestActStartLocalDoesNotArmServingRWForStandbyState(t *testing.T) {
	pm := &fakePostmaster{}
	a := newTestAgent(t, pm, &fakeDCS{})
	obs := reconcile.Observation{Local: reconcile.LocalState{HasData: true, Running: false, InRecovery: true}}
	if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.StartLocal}, obs); err != nil {
		t.Fatalf("act: %v", err)
	}
	if !pm.started {
		t.Fatal("postmaster must be started")
	}
	if a.servingRW.Load() {
		t.Fatal("a read-only standby must not arm servingRW")
	}
}

// Self-health failover (a wedged/frozen primary-state node) must force-stop the
// supervised postmaster before releasing the lease, so a peer cannot promote into a
// second writer if the frozen primary later unfreezes.
func TestActReleaseLeaseForceStopsWedgedPrimary(t *testing.T) {
	pm := &fakePostmaster{}
	d := &fakeDCS{}
	a := newTestAgent(t, pm, d)
	obs := reconcile.Observation{LocalStuck: true, Local: reconcile.LocalState{HasData: true, Running: false, InRecovery: false}}
	if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.ReleaseLease}, obs); err != nil {
		t.Fatalf("act: %v", err)
	}
	if !d.released {
		t.Fatal("lease must be released")
	}
	if !pm.stopped || pm.stopMode != process.Immediate {
		t.Fatalf("a wedged primary must be force-stopped (Immediate) before handing leadership away; stopped=%v mode=%v", pm.stopped, pm.stopMode)
	}
}

// fenceBudget = LeaseDuration - RenewDeadline, floored at RetryPeriod. It bounds
// the apiserver writes on a read-write tick so a slow write cannot starve the
// OnLost fence past the soft-fence window.
func TestFenceBudget(t *testing.T) {
	cases := []struct {
		lease, renew, retry, want time.Duration
	}{
		{15 * time.Second, 10 * time.Second, 2 * time.Second, 5 * time.Second}, // normal margin
		{30 * time.Second, 20 * time.Second, 4 * time.Second, 10 * time.Second},
		{12 * time.Second, 10 * time.Second, 3 * time.Second, 3 * time.Second}, // margin < retry -> floored
	}
	for _, c := range cases {
		a := &agent{cfg: &config.Config{LeaseDuration: c.lease, RenewDeadline: c.renew, RetryPeriod: c.retry}}
		if got := a.fenceBudget(); got != c.want {
			t.Errorf("fenceBudget(L=%s R=%s r=%s) = %s, want %s", c.lease, c.renew, c.retry, got, c.want)
		}
	}
}

// Invariant 9: a peer is a valid replication source only if its system_identifier
// matches the local cluster's.
func TestSameClusterCheck(t *testing.T) {
	if err := sameClusterCheck("pg-1", 7395000000000000001, 7395000000000000001); err != nil {
		t.Errorf("matching system_identifier must be accepted, got %v", err)
	}
	if err := sameClusterCheck("pg-1", 7395000000000000001, 9999999999999999999); err == nil {
		t.Error("a different system_identifier must be refused (invariant 9)")
	}
}

// Releasing the lease as a read-only standby must NOT churn its postmaster.
func TestActReleaseLeaseLeavesStandbyRunning(t *testing.T) {
	pm := &fakePostmaster{}
	d := &fakeDCS{}
	a := newTestAgent(t, pm, d)
	obs := reconcile.Observation{Local: reconcile.LocalState{HasData: true, Running: true, InRecovery: true}}
	if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.ReleaseLease}, obs); err != nil {
		t.Fatalf("act: %v", err)
	}
	if !d.released {
		t.Fatal("lease must be released")
	}
	if pm.stopped {
		t.Fatal("a read-only standby must not be stopped on ReleaseLease")
	}
}

func TestDesiredRoleLabels(t *testing.T) {
	peers := []reconcile.PeerState{
		{Name: "pg-1", Reachable: true, Role: pg.RoleStandby},  // -> standby (joins read-only Service)
		{Name: "pg-2", Reachable: true, Role: pg.RolePrimary},  // a second primary -> orphan (out of reads)
		{Name: "pg-3", Reachable: false, Role: pg.RoleUnknown}, // unreachable -> omitted (label untouched)
	}
	got := desiredRoleLabels("pg-0", peers)

	want := map[string]string{"pg-0": "primary", "pg-1": "standby", "pg-2": "orphan"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("label[%q] = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["pg-3"]; ok {
		t.Error("unreachable peer must be omitted so its label is left untouched (#140)")
	}
}

func TestSelfHealthTracker(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	h := &selfHealthTracker{grace: 15 * time.Second}

	// A slow startup (should serve, never been running) must NOT arm the timer,
	// even well past the grace -- otherwise crash-recovery WAL replay looks "stuck".
	if h.stuck(true, false, t0) {
		t.Fatal("startup (never running) must not be stuck")
	}
	if h.stuck(true, false, t0.Add(60*time.Second)) {
		t.Fatal("never-running data must never trip self-health")
	}

	// Once it comes up healthy the tracker is primed.
	if h.stuck(true, true, t0.Add(61*time.Second)) {
		t.Fatal("a running primary is not stuck")
	}

	// It then goes unreachable (frozen): not stuck until the grace elapses, then stuck.
	base := t0.Add(70 * time.Second)
	if h.stuck(true, false, base) {
		t.Fatal("just-unreachable primary should not be stuck before the grace")
	}
	if h.stuck(true, false, base.Add(14*time.Second)) {
		t.Fatal("within grace must not be stuck")
	}
	if !h.stuck(true, false, base.Add(15*time.Second)) {
		t.Fatal("past the grace the wedged primary must be stuck")
	}

	// Recovery (running again) clears the timer; a later blip re-arms from scratch.
	if h.stuck(true, true, base.Add(20*time.Second)) {
		t.Fatal("recovered primary is not stuck")
	}
	if h.stuck(true, false, base.Add(21*time.Second)) {
		t.Fatal("a fresh unreachable period must re-arm, not carry the old timer")
	}

	// Losing the holder role (or becoming a standby) resets everything.
	if h.stuck(false, false, base.Add(100*time.Second)) {
		t.Fatal("a non-serving node is never stuck")
	}

	// Maintenance pause: the caller passes shouldServe=false while paused, so a
	// primary intentionally stopped during the window does NOT arm self-health, and
	// on resume a still-stopped node is treated as a startup -- it must not fire an
	// immediate failover (the pause-contract fix).
	hp := &selfHealthTracker{grace: 15 * time.Second}
	if hp.stuck(true, true, t0) {
		t.Fatal("running primary not stuck (primes the tracker)")
	}
	if hp.stuck(false, false, t0.Add(60*time.Second)) {
		t.Fatal("paused (shouldServe=false) must not be stuck even past the grace")
	}
	if hp.stuck(true, false, t0.Add(61*time.Second)) {
		t.Fatal("on resume a still-stopped node is a startup, not stuck")
	}
	if hp.stuck(true, false, t0.Add(120*time.Second)) {
		t.Fatal("a slow post-resume startup must not trip self-health")
	}
}

func TestShouldAdvanceMarker(t *testing.T) {
	tl := func(n uint32) pg.Timeline { return pg.Timeline(n) }
	cases := []struct {
		name string
		tl   pg.Timeline
		tlOK bool
		m    reconcile.MarkerState
		want bool
	}{
		{"unreadable local timeline never advances", tl(5), false, reconcile.MarkerState{}, false},
		{"no marker -> establish it", tl(5), true, reconcile.MarkerState{}, true},
		{"above highwater -> advance", tl(6), true, reconcile.MarkerState{Present: true, Timeline: tl(5)}, true},
		{"equal highwater -> no write", tl(5), true, reconcile.MarkerState{Present: true, Timeline: tl(5)}, false},
		{"below highwater -> never lower", tl(4), true, reconcile.MarkerState{Present: true, Timeline: tl(5)}, false},
		{"malformed marker -> re-establish", tl(5), true, reconcile.MarkerState{Present: true, Malformed: true, Timeline: tl(9)}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldAdvanceMarker(c.tl, c.tlOK, c.m); got != c.want {
				t.Errorf("shouldAdvanceMarker = %v, want %v", got, c.want)
			}
		})
	}
}

func TestNodeIDAndBaseName(t *testing.T) {
	if got := baseName("my-pg-0"); got != "my-pg" {
		t.Errorf("baseName = %q, want my-pg", got)
	}
	if got := nodeID("my-pg-3"); got != 1003 {
		t.Errorf("nodeID = %d, want 1003", got)
	}
}
