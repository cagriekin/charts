package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
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

func TestLogStartupConfigExcludesSecrets(t *testing.T) {
	const (
		repmgrSecret   = "repmgr-password-sentinel"
		postgresSecret = "postgres-password-sentinel"
		etcdCert       = "etcd-cert-sentinel"
		etcdKey        = "etcd-key-sentinel"
		etcdCA         = "etcd-ca-sentinel"
	)
	var out bytes.Buffer
	cfg := &config.Config{
		PodName:            "pg-0",
		Namespace:          "db",
		LeaseName:          "pg-leader",
		DCSBackend:         "kubernetes",
		ReconcileInterval:  5 * time.Second,
		LeaseDuration:      15 * time.Second,
		RenewDeadline:      10 * time.Second,
		RetryPeriod:        2 * time.Second,
		NodeCount:          3,
		HeadlessService:    "pg-headless",
		MasterService:      "pg",
		MarkerName:         "pg-primary",
		CascadeReplication: true,
		PGMajor:            "17",
		RepmgrPassword:     repmgrSecret,
		PostgresPassword:   postgresSecret,
		EtcdCertFile:       etcdCert,
		EtcdKeyFile:        etcdKey,
		EtcdCAFile:         etcdCA,
	}

	logStartupConfig(slog.New(slog.NewTextHandler(&out, nil)), cfg)
	logLine := out.String()

	for _, want := range []string{
		"starting pg-ha-agent",
		"podName=pg-0",
		"namespace=db",
		"leaseName=pg-leader",
		"dcsBackend=kubernetes",
		"reconcileInterval=5s",
		"leaseDuration=15s",
		"renewDeadline=10s",
		"retryPeriod=2s",
		"nodeCount=3",
		"headlessService=pg-headless",
		"masterService=pg",
		"markerName=pg-primary",
		"cascadeReplication=true",
		"pgMajor=17",
	} {
		if !strings.Contains(logLine, want) {
			t.Errorf("startup log missing %q: %s", want, logLine)
		}
	}
	for _, forbidden := range []string{
		repmgrSecret, postgresSecret, etcdCert, etcdKey, etcdCA,
		"RepmgrPassword", "PostgresPassword",
		"EtcdCertFile", "EtcdKeyFile", "EtcdCAFile",
	} {
		if strings.Contains(logLine, forbidden) {
			t.Errorf("startup log leaked sensitive config %q: %s", forbidden, logLine)
		}
	}
}

// --- fakes for the act() path ---

type fakePostmaster struct {
	started  bool
	stopped  bool
	stopMode process.StopMode
	reloaded bool
	running  bool
	// stopErr, when set, is what Stop returns -- context.DeadlineExceeded stands in for
	// the real postmaster's deadline-forced SIGKILL. deadOnStop clears running the way a
	// killed-and-reaped child does; without it the child is still alive after the failure.
	stopErr            error
	deadOnStop         bool
	stopCtxHadDeadline bool
}

func (f *fakePostmaster) Start(context.Context) error { f.started = true; return nil }
func (f *fakePostmaster) Stop(ctx context.Context, m process.StopMode) error {
	f.stopped, f.stopMode = true, m
	_, f.stopCtxHadDeadline = ctx.Deadline()
	if f.stopErr != nil {
		if f.deadOnStop {
			f.running = false
		}
		return f.stopErr
	}
	f.running = false
	return nil
}
func (f *fakePostmaster) Reload(context.Context) error { f.reloaded = true; return nil }
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
	walRcv       string // pg_stat_wal_receiver "sender_host|status" output (psql)
	nodes        string // SELECT node_id FROM repmgr.nodes output (psql)
	regErr       error  // error returned for `repmgr standby register` (nil = success)
	follows      int    // number of `repmgr standby follow` calls
	unregistered []int  // node_ids passed to `repmgr standby unregister`
	followOut    string // combined output for `repmgr standby follow`; non-empty = it fails
	rejoins      int    // number of `repmgr node rejoin` calls (the #297 re-clone escalation)
	nodesQueries int    // number of `SELECT ... FROM repmgr.nodes` calls (psql)
}

func (s *scriptedExec) Run(_ context.Context, _ []string, name string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	switch {
	case name == "psql" && strings.Contains(joined, "repmgr.nodes"):
		s.nodesQueries++
		return s.nodes, nil
	case name == "psql" && strings.Contains(joined, "pg_stat_wal_receiver"):
		return s.walRcv, nil
	case name == "repmgr" && strings.Contains(joined, "standby register"):
		if s.regErr != nil {
			return "register failed", s.regErr
		}
		return "ok", nil
	case name == "repmgr" && strings.Contains(joined, "standby follow"):
		s.follows++
		if s.followOut != "" {
			return s.followOut, errors.New("exit status 1")
		}
		return "ok", nil
	case name == "repmgr" && strings.Contains(joined, "node rejoin"):
		s.rejoins++
		return "ok", nil
	case name == "repmgr" && strings.Contains(joined, "standby unregister"):
		for _, a := range args {
			if strings.HasPrefix(a, "--node-id=") {
				if n, err := strconv.Atoi(strings.TrimPrefix(a, "--node-id=")); err == nil {
					s.unregistered = append(s.unregistered, n)
				}
			}
		}
		return "ok", nil
	}
	return "ok", nil
}

// newFollowTestAgent wires a real Repmgr + Prober backed by one scriptedExec so the
// Follow act() path (register -> streaming probe -> follow) is exercised end to end.
func newFollowTestAgent(t *testing.T, ex *scriptedExec) *agent {
	t.Helper()
	return newFollowTestAgentWithPM(t, ex, &fakePostmaster{})
}

// newFollowTestAgentWithPM is newFollowTestAgent with an injectable postmaster, so a test can
// make Demote fail and exercise an EARLY return from rejoinOnto.
func newFollowTestAgentWithPM(t *testing.T, ex *scriptedExec, pm *fakePostmaster) *agent {
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
		sup:    process.NewSupervisor(pm),
		metr:   observe.New(),
	}
}

// #182: a standby already streaming from the lease holder must NOT re-run repmgr
// standby follow (which errors "slot already active" and, unlatched, repeats every
// tick). The act path skips the command and latches followUpstream.
// #287: the factory must select the mechanism from config, and an absent value must stay
// repmgr so an existing release and an older env-less image are unaffected. Asserted on the
// concrete type because picking the wrong mechanics is invisible until a promote or a clone
// actually runs -- by which point it has already touched a data directory. An unrecognised
// value must fail loudly, not silently fall back to repmgr (fail-fast, matching the rest of
// this codebase's required-config posture) -- config.Load already rejects it at boot, so
// this is a defense against the enum and this factory drifting apart in the future.
func TestNewMechanismSelectsFromConfig(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		name    string
		set     string
		want    string
		wantErr bool
	}{
		{"absent -> repmgr (unchanged default)", "", "*mechanism.Repmgr", false},
		{"explicit repmgr", config.MechanismRepmgr, "*mechanism.Repmgr", false},
		{"native", config.MechanismNative, "*mechanism.Native", false},
		{"unrecognised value fails loudly", "patroni", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{PGDATA: t.TempDir(), Mechanism: tc.set}
			m, err := newMechanism(cfg, "/etc/repmgr/repmgr.conf", "/usr/lib/postgresql/18/bin", log)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("mechanism %q: expected an error, got %T", tc.set, m)
				}
				return
			}
			if err != nil {
				t.Fatalf("mechanism %q: unexpected error: %v", tc.set, err)
			}
			if got := fmt.Sprintf("%T", m); got != tc.want {
				t.Errorf("mechanism %q selected %s, want %s", tc.set, got, tc.want)
			}
		})
	}
}

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

// #287: the native mechanism only writes files (managed conf + standby.signal) and relies
// on the caller to apply them -- unlike repmgr standby follow, which applies the repoint
// itself. act() must reload the supervised postmaster after every successful Follow, not
// just for one mechanism, or native mode's Follow is silently inert (the file changes but
// the running postmaster never reconnects to the new upstream). Asserted against the
// generic act() path (mechanism-agnostic) rather than duplicating a full Native-backed
// harness, since the fix lives in act(), not in either mechanism.
func TestActFollowReloadsPostmasterAfterSuccess(t *testing.T) {
	ex := &scriptedExec{walRcv: ""}
	pm := &fakePostmaster{}
	a := newFollowTestAgentWithPM(t, ex, pm)
	dec := reconcile.Decision{Action: reconcile.Follow, Target: "pg-0"}
	if err := a.act(context.Background(), dec, reconcile.Observation{}); err != nil {
		t.Fatalf("act: %v", err)
	}
	if !pm.reloaded {
		t.Fatal("act must reload the postmaster after a successful Follow, or a mechanism that only writes files (native) never applies the repoint")
	}
}

// #297: a standby absent from its OWN repmgr.nodes copy can never obtain the row, so
// following is permanently impossible -- it would sit Running-but-never-Ready, silently not
// replicating. Re-clone from the current primary, which replaces data and metadata together.
func TestActFollowReclonesWhenLocalRecordMissing(t *testing.T) {
	ex := &scriptedExec{walRcv: "", followOut: "ERROR: unable to retrieve record for local node 1002"}
	a := newFollowTestAgent(t, ex)
	a.followUpstream = "stale"
	dec := reconcile.Decision{Action: reconcile.Follow, Target: "pg-1"}
	if err := a.act(context.Background(), dec, reconcile.Observation{}); err != nil {
		t.Fatalf("act must recover by re-cloning, got %v", err)
	}
	if ex.rejoins != 1 {
		t.Fatalf("a missing local record must escalate to a rejoin/re-clone, got %d calls", ex.rejoins)
	}
	if a.followUpstream != "" {
		t.Fatalf("the follow latch must reset after a re-clone, got %q", a.followUpstream)
	}
}

// The follow latch is invalidated on ENTRY to rejoinOnto, so it is cleared even when the
// rejoin fails before reconfiguring anything (here: Demote fails). Clearing it late would
// leave a stale latch behind a failed rejoin. Nothing depends on that today -- the
// escalation is only reachable when the latch already differs from the target -- but the
// invariant "an attempt to rejoin invalidates the latch" is unconditional, and this pins it
// so a future edit that reads the latch earlier cannot silently rely on a stale value.
func TestRejoinOntoClearsFollowLatchEvenWhenItFailsEarly(t *testing.T) {
	ex := &scriptedExec{walRcv: "", followOut: "ERROR: unable to retrieve record for local node 1002"}
	pm := &fakePostmaster{stopErr: errors.New("stop failed")}
	a := newFollowTestAgentWithPM(t, ex, pm)
	a.followUpstream = "stale-upstream"
	dec := reconcile.Decision{Action: reconcile.Follow, Target: "pg-1"}
	if err := a.act(context.Background(), dec, reconcile.Observation{}); err == nil {
		t.Fatal("a rejoin whose demote fails must surface an error")
	}
	if ex.rejoins != 0 {
		t.Fatalf("rejoin must not have reconfigured anything after a failed demote, got %d", ex.rejoins)
	}
	if a.followUpstream != "" {
		t.Fatalf("the follow latch must be cleared on entry to a rejoin, got %q", a.followUpstream)
	}
}

// ...but a missing UPSTREAM record must NOT re-clone. That is the ordinary post-failover
// case (the target has not promoted yet) and waiting is correct; re-cloning there demotes a
// healthy standby and destroys its data directory -- the #286 regression this guards.
func TestActFollowDoesNotRecloneWhenUpstreamRecordMissing(t *testing.T) {
	ex := &scriptedExec{walRcv: "", followOut: "ERROR: unable to find record for intended upstream node 1002"}
	a := newFollowTestAgent(t, ex)
	dec := reconcile.Decision{Action: reconcile.Follow, Target: "pg-1"}
	if err := a.act(context.Background(), dec, reconcile.Observation{}); err == nil {
		t.Fatal("a missing upstream record must surface so the next tick retries")
	}
	if ex.rejoins != 0 {
		t.Fatalf("a missing upstream record must NOT re-clone the node, got %d rejoins", ex.rejoins)
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

// A freshly-cloned standby streams before it is registered in repmgr.nodes. If
// RegisterStandby fails, the probe-skip must NOT latch (that would strand the node
// without a record and break a later promote); it falls through to repmgr standby
// follow, which re-establishes the record or errors so the next tick retries.
func TestActFollowDoesNotSkipWhenRegisterFails(t *testing.T) {
	ex := &scriptedExec{walRcv: "pg-0.h|streaming", regErr: errors.New("primary unreachable")}
	a := newFollowTestAgent(t, ex)
	dec := reconcile.Decision{Action: reconcile.Follow, Target: "pg-0"}
	if err := a.act(context.Background(), dec, reconcile.Observation{}); err != nil {
		t.Fatalf("act: %v", err)
	}
	if ex.follows != 1 {
		t.Fatalf("a failed register must NOT skip follow (the probe bypasses repmgr's node-record check), got %d follow calls", ex.follows)
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

// ghostNodeIDs is the #139 scale-down discriminator: a repmgr.nodes row is a ghost
// iff its ordinal (node_id - nodeIDBase) is >= the live pod count (live ordinals are
// 0..nodeCount-1). It must be purely structural and must never flag a live node --
// including the safety case of a zero/negative nodeCount, which must be a no-op.
func TestGhostNodeIDs(t *testing.T) {
	eq := func(a, b []int) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	cases := []struct {
		name      string
		ids       []int
		nodeCount int
		want      []int
	}{
		{"two ghosts above the live range", []int{1000, 1001, 1002, 1003, 1004}, 3, []int{1003, 1004}},
		{"no ghosts when all in range", []int{1000, 1001, 1002}, 3, nil},
		{"single-node cluster, one ghost", []int{1000, 1001}, 1, []int{1001}},
		{"zero nodeCount is a no-op (never treat all as ghosts)", []int{1000, 1001}, 0, nil},
		{"negative nodeCount is a no-op", []int{1000, 1001}, -1, nil},
		{"ids below the base are never flagged", []int{999, 1000, 1001}, 1, []int{1001}},
		{"empty input", nil, 3, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ghostNodeIDs(c.ids, c.nodeCount); !eq(got, c.want) {
				t.Fatalf("ghostNodeIDs(%v, %d) = %v, want %v", c.ids, c.nodeCount, got, c.want)
			}
		})
	}
}

// cleanupGhostNodes wires the real Prober.StandbyNodeIDs + ghostNodeIDs +
// Repmgr.Unregister: the primary lists standby rows and unregisters only the ordinals
// above the live range (#139). End-to-end over the same scriptedExec the Follow tests use.
func TestCleanupGhostNodes(t *testing.T) {
	// 3 registered nodes but NodeCount=2 (a scale-down from 3 pods to 2): node 1002
	// (ordinal 2) is a ghost and must be unregistered; the live 1000/1001 must not.
	ex := &scriptedExec{nodes: "1000\n1001\n1002\n"}
	a := newFollowTestAgent(t, ex)
	a.cfg.NodeCount = 2
	a.cleanupGhostNodes(context.Background())
	if len(ex.unregistered) != 1 || ex.unregistered[0] != 1002 {
		t.Fatalf("expected only node 1002 unregistered, got %v", ex.unregistered)
	}
}

func TestCleanupGhostNodesNoGhosts(t *testing.T) {
	// Every registered node is within the live ordinal range: no unregister at all.
	ex := &scriptedExec{nodes: "1000\n1001\n"}
	a := newFollowTestAgent(t, ex)
	a.cfg.NodeCount = 2
	a.cleanupGhostNodes(context.Background())
	if len(ex.unregistered) != 0 {
		t.Fatalf("expected no unregister when there are no ghosts, got %v", ex.unregistered)
	}
}

// #287: native mode has no repmgr.nodes at all (mechanism.Native's Unregister is a
// no-op), so the query would error on every primary tick forever -- pure log noise with
// nothing to retry. cleanupGhostNodes must skip the query entirely rather than warn.
func TestCleanupGhostNodesSkippedUnderNativeMechanism(t *testing.T) {
	ex := &scriptedExec{nodes: "1000\n1001\n1002\n"}
	a := newFollowTestAgent(t, ex)
	a.cfg.NodeCount = 2
	a.cfg.Mechanism = config.MechanismNative
	a.cleanupGhostNodes(context.Background())
	if ex.nodesQueries != 0 {
		t.Fatalf("expected no repmgr.nodes query under native mechanism, got %d", ex.nodesQueries)
	}
	if len(ex.unregistered) != 0 {
		t.Fatalf("expected no unregister under native mechanism, got %v", ex.unregistered)
	}
}
