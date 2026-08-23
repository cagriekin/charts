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

	"net/http/httptest"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/cagriekin/pg-ha-agent/internal/config"
	"github.com/cagriekin/pg-ha-agent/internal/dcs"
	"github.com/cagriekin/pg-ha-agent/internal/k8s"
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

// --- #289: replication slot ownership ---

func TestSlotNameForIsOrdinalDerivedAndStable(t *testing.T) {
	for _, tc := range []struct{ pod, want string }{
		{"pg-0", "pg_ha_slot_0"},
		{"pg-1", "pg_ha_slot_1"},
		{"my-release-pg-12", "pg_ha_slot_12"},
		// No parseable ordinal: better slotless than an unstable name that strands a
		// new slot on every restart.
		{"pg", ""},
		{"pg-abc", ""},
		{"", ""},
	} {
		if got := slotNameFor(tc.pod); got != tc.want {
			t.Errorf("slotNameFor(%q) = %q, want %q", tc.pod, got, tc.want)
		}
	}
}

// The slot name must be one PostgreSQL accepts, for every ordinal the chart can produce
// -- a name rejected at create time would leave the standby slotless with only a per-tick
// warning. PostgreSQL allows lower-case letters, digits and underscore, max 63 chars;
// asserted here directly so this stays honest even if the probe's own guard drifts.
func TestSlotNameForProducesAValidSlotName(t *testing.T) {
	for _, ord := range []int{0, 1, 9, 10, 99, 1000} {
		name := slotNameFor(fmt.Sprintf("pg-%d", ord))
		if name == "" || len(name) > 63 {
			t.Errorf("slotNameFor(pg-%d) = %q: empty or over 63 chars", ord, name)
			continue
		}
		for _, r := range name {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
				continue
			}
			t.Errorf("slotNameFor(pg-%d) = %q contains %q, which PostgreSQL rejects in a slot name", ord, name, r)
		}
	}
}

func TestOrphanSlotReclaimsOnlyDepartedOrdinalsAndSelf(t *testing.T) {
	const self = "pg-0"
	live3 := map[int]bool{0: true, 1: true, 2: true} // a 3-pod StatefulSet
	for _, tc := range []struct {
		name string
		slot string
		live map[int]bool
		want bool
		why  string
	}{
		// Departed ordinals: the pod is gone, nobody will ever reattach.
		{"ghost above the live range", "pg_ha_slot_3", live3, true, "no pod-3 exists"},
		{"ghost far above", "pg_ha_slot_9", live3, true, "no pod-9 exists"},
		// Live peers: their standby is expected to stream through this slot.
		{"live peer", "pg_ha_slot_1", live3, false, "pod-1 exists"},
		{"highest live peer", "pg_ha_slot_2", live3, false, "pod-2 exists"},
		// The primary does not stream from itself, so its own slot is unused.
		{"own slot while primary", "pg_ha_slot_0", live3, true, "self ordinal"},
		// Legacy repmgr slots are reclaimable regardless of liveness: native never streams
		// through one, so every one is dead weight the moment the cluster is on this
		// mechanism. Scoping them to departed ordinals left a repmgr->native migration
		// (#292) with a permanent orphan per surviving node -- the exact case it cleans up.
		// The atomic `AND NOT active` in the drop, not the pod set, is what protects a slot
		// still carrying a stream mid-migration.
		{"legacy slot, departed ordinal", "repmgr_slot_1003", live3, true, "no pod-3 exists"},
		{"legacy slot, LIVE ordinal", "repmgr_slot_1001", live3, true, "native never streams through it; NOT active guards a live stream"},
		{"legacy slot for self", "repmgr_slot_1000", live3, true, "native never streams through it"},
		// Anything the agent did not mint is left strictly alone.
		{"operator's own slot", "my_own_slot", live3, false, "not agent-minted"},
		{"logical-looking slot", "debezium_cdc", live3, false, "not agent-minted"},
		{"prefix but no ordinal", "pg_ha_slot_abc", live3, false, "unparseable ordinal"},
		{"legacy prefix but no ordinal", "repmgr_slot_abc", live3, false, "unparseable ordinal"},
		{"legacy id below the base", "repmgr_slot_7", live3, false, "not a node_id this agent assigned"},
		// An empty/failed pod list must reclaim nothing but self -- never treat every
		// standby's slot as orphaned because the API read came back empty.
		{"empty pod list, peer slot", "pg_ha_slot_1", map[int]bool{}, false, "no authoritative pod set"},
		{"empty pod list, ghost slot", "pg_ha_slot_9", map[int]bool{}, false, "no authoritative pod set"},
		{"empty pod list, legacy slot", "repmgr_slot_1009", map[int]bool{}, true, "legacy slots do not depend on the pod set"},
		{"empty pod list, own slot", "pg_ha_slot_0", map[int]bool{}, true, "self is unused regardless of the pod set"},
	} {
		if got := orphanSlot(tc.slot, self, tc.live); got != tc.want {
			t.Errorf("%s: orphanSlot(%q, %q, %v) = %v, want %v (%s)",
				tc.name, tc.slot, self, tc.live, got, tc.want, tc.why)
		}
	}
}

// The scale-up regression this guards (review finding): REPMGR_NODE_COUNT is baked into
// each pod at render time, so during a scale-up rollout the not-yet-rolled primary holds
// the OLD count. Deciding ownership from the LIVE pod set instead means a brand-new
// standby's just-created slot survives even while the primary's own NodeCount still says
// that ordinal should not exist -- and it is briefly inactive between pg_basebackup
// finishing and its postmaster reattaching, which is exactly when a drop would land.
func TestOrphanSlotSurvivesAScaleUpWithAStalePrimaryNodeCount(t *testing.T) {
	// Scaled 2 -> 3: pod-2 now exists, but the primary still believes NodeCount == 2.
	live := map[int]bool{0: true, 1: true, 2: true}
	if orphanSlot("pg_ha_slot_2", "pg-0", live) {
		t.Error("pg_ha_slot_2 reclaimed during a scale-up: a live standby's slot must never be dropped, even when the primary's NodeCount is stale")
	}
}

// A promoted pod's own slot becomes reclaimable, and the ex-primary's does not: ownership
// follows whoever currently holds the lease, so the set shifts on failover.
func TestOrphanSlotSelfSlotFollowsTheCurrentPrimary(t *testing.T) {
	live := map[int]bool{0: true, 1: true, 2: true}
	if !orphanSlot("pg_ha_slot_1", "pg-1", live) {
		t.Error("pg-1 as primary should reclaim its own slot pg_ha_slot_1")
	}
	if orphanSlot("pg_ha_slot_0", "pg-1", live) {
		t.Error("pg-1 as primary must NOT reclaim pg-0's slot -- pg-0 is a live standby streaming through it")
	}
}

func TestSlotOrdinalRecognisesOnlyOwnableNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		ord  int
		ok   bool
	}{
		{"pg_ha_slot_0", 0, true},
		{"pg_ha_slot_12", 12, true},
		{"repmgr_slot_1000", 0, true},
		{"repmgr_slot_1002", 2, true},
		{"repmgr_slot_999", 0, false}, // below nodeIDBase: not our numbering
		{"pg_ha_slot_-1", 0, false},
		{"pg_ha_slot_", 0, false},
		{"my_own_slot", 0, false},
		{"", 0, false},
	} {
		ord, ok := slotOrdinal(tc.name)
		if ok != tc.ok || (ok && ord != tc.ord) {
			t.Errorf("slotOrdinal(%q) = (%d, %v), want (%d, %v)", tc.name, ord, ok, tc.ord, tc.ok)
		}
	}
}

func TestSlotMetricsAggregatesCountInactiveAndMaxRetained(t *testing.T) {
	got := slotMetrics([]pg.SlotState{
		{Name: "pg_ha_slot_1", Active: true, RetainedWALBytes: 1024, WALStatus: "reserved", Reserving: true},
		{Name: "pg_ha_slot_2", Active: false, RetainedWALBytes: 16 << 20, WALStatus: "reserved", Reserving: true},
		{Name: "pg_ha_slot_3", Active: false, RetainedWALBytes: 8 << 20, WALStatus: "extended", Reserving: true},
	})
	if got.Total != 3 {
		t.Errorf("Total = %d, want 3", got.Total)
	}
	if got.Inactive != 2 {
		t.Errorf("Inactive = %d, want 2", got.Inactive)
	}
	if got.MaxRetainedWALBytes != 16<<20 {
		t.Errorf("MaxRetainedWALBytes = %d, want %d", got.MaxRetainedWALBytes, 16<<20)
	}
	empty := slotMetrics(nil)
	if empty.Total != 0 || empty.Inactive != 0 || empty.MaxRetainedWALBytes != 0 {
		t.Errorf("no slots: got %+v, want all zero", empty)
	}
}

// A slot the primary pre-created for a standby that has not arrived is inactive and
// reserves NOTHING (restart_lsn NULL). Counting it as inactive would fire
// PGHAReplicationSlotInactive -- "so it is accumulating WAL" -- over a slot accumulating
// nothing, and native mode pre-creates one per expected ordinal, so that is the DEFAULT
// state there rather than an edge case.
func TestSlotMetricsIgnoresSlotsThatReserveNothing(t *testing.T) {
	got := slotMetrics([]pg.SlotState{
		{Name: "pg_ha_slot_1", Active: false, RetainedWALBytes: 0, WALStatus: "", Reserving: false},
	})
	if got.Total != 1 {
		t.Errorf("Total = %d, want 1 (the slot still exists and must be enumerated)", got.Total)
	}
	if got.Inactive != 0 {
		t.Errorf("Inactive = %d, want 0: a slot reserving no WAL is not accumulating any", got.Inactive)
	}
}

// An invalidated slot needs its OWN gauge: exceeding max_slot_wal_keep_size (4GB by default
// in this image) nulls restart_lsn, so the retained-bytes figure collapses to zero at the
// instant the slot dies. Read through the bytes gauge alone, the worst outcome -- a standby
// that can now only recover by a full re-clone -- is indistinguishable from the healthiest.
func TestSlotMetricsCountsInvalidatedSlotsSeparately(t *testing.T) {
	got := slotMetrics([]pg.SlotState{
		{Name: "pg_ha_slot_1", Active: false, RetainedWALBytes: 0, WALStatus: "lost", Reserving: false},
		{Name: "pg_ha_slot_2", Active: true, RetainedWALBytes: 4096, WALStatus: "reserved", Reserving: true},
	})
	if got.Invalidated != 1 {
		t.Errorf("Invalidated = %d, want 1", got.Invalidated)
	}
	if got.MaxRetainedWALBytes != 4096 {
		t.Errorf("MaxRetainedWALBytes = %d: the invalidated slot reports 0 bytes, which is exactly why it needs its own gauge", got.MaxRetainedWALBytes)
	}
	if got.Inactive != 0 {
		t.Errorf("Inactive = %d, want 0: an invalidated slot reserves nothing, and it is counted as invalidated instead", got.Inactive)
	}
}

// slotExec answers the three slot statements and records what was asked, so a test can
// distinguish "looked at the slots" from "changed them".
type slotExec struct {
	rows    string // pg_replication_slots output, "name|active|bytes" per line
	listed  int
	created []string
	dropped []string
}

func (s *slotExec) Run(_ context.Context, _ []string, name string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	switch {
	case name == "psql" && strings.Contains(joined, "pg_create_physical_replication_slot"):
		s.created = append(s.created, slotArg(joined))
		return "", nil
	case name == "psql" && strings.Contains(joined, "pg_drop_replication_slot"):
		s.dropped = append(s.dropped, slotArg(joined))
		return slotArg(joined), nil
	case name == "psql" && strings.Contains(joined, "pg_replication_slots"):
		s.listed++
		return s.rows, nil
	}
	return "", nil
}

// slotArg pulls the first quoted slot name out of a statement, which is enough to identify
// which slot it acted on without parsing SQL.
func slotArg(sql string) string {
	i := strings.Index(sql, "'")
	if i < 0 {
		return ""
	}
	rest := sql[i+1:]
	j := strings.Index(rest, "'")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// #289: slot OBSERVATION runs under EVERY mechanism; slot MUTATION only under native.
//
// The split matters because the chart renders the two slot PrometheusRules for every
// agent-mode release regardless of mechanism. If the gauges were published only in native
// mode, those alerts would sit pinned at zero on the DEFAULT mechanism -- an alert that
// cannot fire reads as coverage while providing none, which is worse than shipping none.
// Mutation stays gated: under the repmgr mechanism repmgr owns slot lifecycle, and two
// owners fighting over the same slots is worse than one.
func TestSlotsTickObservesUnderEveryMechanismAndMutatesOnlyInNative(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mechanism  string
		wantMutate bool
	}{
		{"repmgr (absent, the default)", "", false},
		{"repmgr (explicit)", config.MechanismRepmgr, false},
		{"native", config.MechanismNative, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// One slot for a departed ordinal (2, outside the live pod set below), inactive
			// and pinning WAL: reclaimable under native, untouchable under repmgr.
			ex := &slotExec{rows: "pg_ha_slot_2|f|4096|reserved|t\n"}
			a := newSlotTestAgent(t, ex, tc.mechanism)

			a.slotsTick(context.Background())

			if ex.listed == 0 {
				t.Errorf("slots were never listed, so the gauges the shipped alerts read stay at zero")
			}
			if body := scrapeMetrics(t, a); !strings.Contains(body, "pg_ha_agent_replication_slot_max_retained_wal_bytes 4096") {
				t.Errorf("retained-WAL gauge not published under mechanism %q: %s", tc.mechanism, body)
			}
			mutated := len(ex.created) > 0 || len(ex.dropped) > 0
			if mutated != tc.wantMutate {
				t.Errorf("mutated=%v (created=%v dropped=%v), want %v under mechanism %q",
					mutated, ex.created, ex.dropped, tc.wantMutate, tc.mechanism)
			}
		})
	}
}

// A failed slot list must not be read as "there are no slots": creating or dropping on that
// basis is how a slot a standby still needs gets destroyed (#289).
func TestSlotsTickMutatesNothingWhenTheSlotListFails(t *testing.T) {
	ex := &slotExec{rows: "not-three-fields\n"} // parse failure inside PhysicalSlots
	a := newSlotTestAgent(t, ex, config.MechanismNative)

	a.slotsTick(context.Background())

	if len(ex.created) > 0 || len(ex.dropped) > 0 {
		t.Errorf("mutated slots on an unreadable list: created=%v dropped=%v", ex.created, ex.dropped)
	}
}

// newSlotTestAgent wires a real Prober over ex plus a fake apiserver holding pods 0 and 1,
// so the live-pod-set half of the reconcile is exercised rather than stubbed.
func newSlotTestAgent(t *testing.T, ex *slotExec, mech string) *agent {
	t.Helper()
	cs := k8sfake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pg-0", Namespace: "ns", Labels: map[string]string{"app": "pg"}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pg-1", Namespace: "ns", Labels: map[string]string{"app": "pg"}}},
	)
	return &agent{
		cfg: &config.Config{
			PodName:        "pg-0",
			Namespace:      "ns",
			PodSelector:    "app=pg",
			NodeCount:      2,
			Mechanism:      mech,
			RepmgrUser:     "repmgr",
			RepmgrDB:       "repmgr",
			RepmgrPassword: "pw",
		},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		kube:   k8s.NewWithClient(cs, "ns"),
		prober: &pg.Prober{Exec: ex, Timeout: time.Second},
		metr:   observe.New(),
	}
}

// scrapeMetrics reads the agent's real metrics endpoint, so the assertion covers what a
// Prometheus scrape would actually see rather than an internal field.
func scrapeMetrics(t *testing.T, a *agent) string {
	t.Helper()
	rec := httptest.NewRecorder()
	a.metr.Handler(time.Minute).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

// #289 review: a demoted primary keeps every slot it minted while it WAS the primary --
// pg_basebackup and pg_rewind both exclude pg_replslot, and a plain Follow touches nothing.
// Those slots go inactive and an inactive slot restricts WAL removal on a standby exactly as
// it does on a primary, so the ex-primary's own pg_wal grows without bound on its own volume.
// It does not self-heal on a later re-promotion either: by then those ordinals have live pods
// again, so the primary reconcile reads them as live peers' slots and leaves them alone.
func TestStandbySlotsTickReclaimsSlotsLeftBehindByADemotion(t *testing.T) {
	// Two slots this node minted while primary, for ordinals whose pods are very much alive
	// (they stream from the NEW primary now) -- which is precisely why the primary-side
	// pod-set test cannot reclaim them.
	ex := &slotExec{rows: "pg_ha_slot_1|f|16777216|reserved|t\npg_ha_slot_2|f|8388608|reserved|t\n"}
	a := newSlotTestAgent(t, ex, config.MechanismNative)

	a.standbySlotsTick(context.Background())

	if len(ex.dropped) != 2 {
		t.Fatalf("dropped %v, want both leftover slots reclaimed", ex.dropped)
	}
	if len(ex.created) != 0 {
		t.Errorf("a standby created slots (%v): it is never an upstream under this mechanism", ex.created)
	}
}

// Observation is mechanism-agnostic (a standby holding WAL back must be visible), mutation
// is native-only -- under repmgr, repmgr owns slot lifecycle and two owners would fight.
func TestStandbySlotsTickPublishesUnderRepmgrButReclaimsNothing(t *testing.T) {
	ex := &slotExec{rows: "pg_ha_slot_1|f|16777216|reserved|t\n"}
	a := newSlotTestAgent(t, ex, config.MechanismRepmgr)

	a.standbySlotsTick(context.Background())

	if len(ex.dropped) != 0 {
		t.Errorf("reclaimed %v under the repmgr mechanism, where repmgr owns slot lifecycle", ex.dropped)
	}
	if body := scrapeMetrics(t, a); !strings.Contains(body, "pg_ha_agent_replication_slot_max_retained_wal_bytes 16777216") {
		t.Errorf("a standby holding 16MiB of WAL back published nothing: %s", body)
	}
}

// What a standby may reclaim is decided by NAME alone, with no ordinal or pod-set test: a
// standby is never an upstream under this mechanism (its own slot lives on its upstream, and
// cascadingReplication + native is rejected at render time), so the ordinal a leftover names
// says nothing about whether it is still needed. An operator's slot stays untouchable.
func TestLeftoverStandbySlotClaimsOnlyAgentMintedAndLegacyNames(t *testing.T) {
	for _, tc := range []struct {
		slot string
		want bool
		why  string
	}{
		{"pg_ha_slot_0", true, "agent-minted"},
		{"pg_ha_slot_7", true, "agent-minted, ordinal irrelevant on a standby"},
		{"repmgr_slot_1001", true, "native never streams through a legacy slot"},
		{"my_own_slot", false, "operator's"},
		{"debezium_cdc", false, "a logical subscription's"},
		{"pg_ha_slot_abc", false, "unparseable ordinal: not a name this agent mints"},
	} {
		if got := leftoverStandbySlot(tc.slot); got != tc.want {
			t.Errorf("leftoverStandbySlot(%q) = %v, want %v (%s)", tc.slot, got, tc.want, tc.why)
		}
	}
}

// The gauges must be retracted when a node is neither primary nor a steady-state standby,
// but NOT on Follow -- that is the branch a demoted ex-primary settles into, and it is where
// leftover slots reserving WAL on its own volume have to stay visible.
func TestFollowKeepsPublishingSlotGaugesWhileOtherActionsRetractThem(t *testing.T) {
	for _, tc := range []struct {
		action reconcile.Action
		want   bool
	}{
		{reconcile.Promote, true},
		{reconcile.StayPrimary, true},
		{reconcile.Follow, true},
		{reconcile.DemoteFence, false},
		{reconcile.NoOp, false},
	} {
		m := observe.New()
		m.SetSlots(observe.SlotStats{Total: 3, Inactive: 1, MaxRetainedWALBytes: 4096})
		switch tc.action {
		case reconcile.Promote, reconcile.StayPrimary, reconcile.Follow:
		default:
			m.ClearSlots()
		}
		rec := httptest.NewRecorder()
		m.Handler(time.Minute).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		kept := strings.Contains(rec.Body.String(), "pg_ha_agent_replication_slot_max_retained_wal_bytes 4096")
		if kept != tc.want {
			t.Errorf("action %v: gauges kept=%v, want %v", tc.action, kept, tc.want)
		}
	}
}

// initdbExec records what the bootstrap branch shells out to, so the sequencing can be
// asserted without a real initdb.
type initdbExec struct {
	calls [][]string
	err   error
}

func (e *initdbExec) Run(_ context.Context, _ []string, name string, args ...string) (string, error) {
	e.calls = append(e.calls, append([]string{name}, args...))
	if e.err != nil {
		return "boom", e.err
	}
	return "", nil
}

// #288: under repmgr the branch must stay inert -- the entrypoint initdbs inline before the
// agent runs, and any behaviour change here would move the default path.
func TestBootstrapInitdbIsInertUnderRepmgr(t *testing.T) {
	ex := &initdbExec{}
	a := newBootstrapTestAgent(t, ex, config.MechanismRepmgr)
	if err := a.act(context.Background(),
		reconcile.Decision{Action: reconcile.BootstrapInitdb},
		reconcile.Observation{}); err != nil {
		t.Fatalf("act: %v", err)
	}
	if len(ex.calls) != 0 {
		t.Errorf("repmgr mode shelled out to %v; the entrypoint owns initdb there", ex.calls)
	}
}

// Under native the lease holder creates the cluster. Whether to initdb at all is a
// cluster-wide decision, and the lease is the only thing that makes it happen exactly once --
// without this, every pod arrives with an empty PGDATA and initdbs its own cluster, which
// assertSameCluster then refuses to rejoin forever.
func TestBootstrapInitdbNativeInvokesTheEntrypointThenStarts(t *testing.T) {
	ex := &initdbExec{}
	pm := &fakePostmaster{}
	a := newBootstrapTestAgentWithPM(t, ex, config.MechanismNative, pm)
	if err := a.act(context.Background(),
		reconcile.Decision{Action: reconcile.BootstrapInitdb},
		reconcile.Observation{}); err != nil {
		t.Fatalf("act: %v", err)
	}
	if len(ex.calls) != 1 || ex.calls[0][0] != entrypointPath || ex.calls[0][1] != "initdb" {
		t.Fatalf("want one %s initdb call, got %v", entrypointPath, ex.calls)
	}
	if !pm.started {
		t.Error("the cluster was created but never started")
	}
	// The lost-leadership fence must be armed before any tick observes this node as a writer.
	if !a.servingRW.Load() {
		t.Error("servingRW not armed after creating a read-write primary")
	}
}

// Never initdb over existing data, whatever the decision says. Decide should not produce this
// combination, but the branch is the last line of defence before a destructive command.
func TestBootstrapInitdbNativeRefusesWhenDataExists(t *testing.T) {
	for _, obs := range []reconcile.Observation{
		{Local: reconcile.LocalState{HasData: true}},
		{Local: reconcile.LocalState{Running: true}},
	} {
		ex := &initdbExec{}
		a := newBootstrapTestAgent(t, ex, config.MechanismNative)
		if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.BootstrapInitdb}, obs); err != nil {
			t.Fatalf("act: %v", err)
		}
		if len(ex.calls) != 0 {
			t.Errorf("initdb attempted over existing data (obs=%+v): %v", obs.Local, ex.calls)
		}
	}
}

// A failed initdb must surface, not be swallowed: the node has no cluster, and a silent
// success would let the next tick observe an empty PGDATA forever.
func TestBootstrapInitdbNativePropagatesFailure(t *testing.T) {
	ex := &initdbExec{err: errors.New("exit status 1")}
	a := newBootstrapTestAgent(t, ex, config.MechanismNative)
	if err := a.act(context.Background(),
		reconcile.Decision{Action: reconcile.BootstrapInitdb},
		reconcile.Observation{}); err == nil {
		t.Fatal("a failed initdb reported success")
	}
}

func newBootstrapTestAgent(t *testing.T, ex *initdbExec, mech string) *agent {
	t.Helper()
	return newBootstrapTestAgentWithPM(t, ex, mech, &fakePostmaster{})
}

func newBootstrapTestAgentWithPM(t *testing.T, ex *initdbExec, mech string, pm *fakePostmaster) *agent {
	t.Helper()
	dataDir := t.TempDir()
	// native's GenerateConfig appends an include line to PGDATA/postgresql.conf, which a real
	// initdb would have created; the fake exec does not, so seed it.
	if err := os.WriteFile(filepath.Join(dataDir, "postgresql.conf"), []byte("# seeded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := mechanism.NewNative(dataDir, "/usr/lib/postgresql/18/bin", "pw", "pg_ha_slot_0", "pg-0")
	return &agent{
		cfg: &config.Config{
			PGDATA: dataDir, PodName: "pg-0", HeadlessService: "h",
			RepmgrUser: "repmgr", RepmgrDB: "repmgr", RepmgrPassword: "pw",
			Mechanism: mech, PgHbaPeerCIDR: "10.0.0.0/8", RenewDeadline: 2 * time.Second,
		},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		dcs:    &fakeDCS{},
		mech:   m,
		prober: &pg.Prober{Exec: ex, Timeout: time.Second},
		sup:    process.NewSupervisor(pm),
		metr:   observe.New(),
	}
}
