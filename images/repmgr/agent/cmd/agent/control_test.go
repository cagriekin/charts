package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/cagriekin/pg-ha-agent/internal/config"
	"github.com/cagriekin/pg-ha-agent/internal/control"
	"github.com/cagriekin/pg-ha-agent/internal/k8s"
	"github.com/cagriekin/pg-ha-agent/internal/observe"
	"github.com/cagriekin/pg-ha-agent/internal/pg"
	"github.com/cagriekin/pg-ha-agent/internal/process"
	"github.com/cagriekin/pg-ha-agent/internal/reconcile"
)

// FORCE bypasses pgBackRest's postmaster.pid interlock -- the last guard against
// restoring over a live volume. The API must not be able to set it (nor clear a
// values-set one): the only inputs it may change are the PITR target fields.
func TestRestoreEnvOverridesNeverTouchForce(t *testing.T) {
	b := backupAPI{a: &agent{cfg: &config.Config{}}}
	ov := b.restoreEnvOverrides(control.RestoreRequest{
		TargetType: "time", Target: "2026-08-01 09:55:00+00", BackupSet: "20260801-090002F",
		Force: true, // the API's own confirmation flag, NOT pgbackrest's --force
	})
	if _, ok := ov["FORCE"]; ok {
		t.Fatalf("FORCE must never be overridden by the API: %v", ov)
	}
	// Exactly the target fields, nothing else.
	want := map[string]string{
		"TARGET_TYPE": "time",
		"TARGET":      "2026-08-01 09:55:00+00",
		"BACKUP_SET":  "20260801-090002F",
	}
	if len(ov) != len(want) {
		t.Fatalf("overrides = %v, want only %v", ov, want)
	}
	for k, v := range want {
		if ov[k] != v {
			t.Errorf("override %s = %q, want %q", k, ov[k], v)
		}
	}
}

// Verbose per-file logging is confined to API-triggered restores, and only when the
// operator opted into reading them.
func TestRestoreEnvOverridesRaiseLogLevelOnlyWhenOptedIn(t *testing.T) {
	off := backupAPI{a: &agent{cfg: &config.Config{}}}
	if _, ok := off.restoreEnvOverrides(control.RestoreRequest{})["PGBACKREST_LOG_LEVEL_CONSOLE"]; ok {
		t.Error("log level must not be raised without ControlRestoreReadPodLogs")
	}
	on := backupAPI{a: &agent{cfg: &config.Config{ControlRestoreReadPodLogs: true}}}
	if got := on.restoreEnvOverrides(control.RestoreRequest{})["PGBACKREST_LOG_LEVEL_CONSOLE"]; got != "detail" {
		t.Errorf("log level = %q, want detail (pgBackRest logs its per-file percentages at detail)", got)
	}
}

func TestRestorePhase(t *testing.T) {
	cases := []struct {
		name string
		job  k8s.JobView
		pod  k8s.PodView
		want string
	}{
		{"no job", k8s.JobView{}, k8s.PodView{}, "none"},
		{"succeeded", k8s.JobView{Present: true, Succeeded: 1}, k8s.PodView{}, "succeeded"},
		{"failed", k8s.JobView{Present: true, Failed: 1}, k8s.PodView{}, "failed"},
		// A retrying Job (backoffLimit > 0) has a failed attempt AND an active one; it is
		// still running, not failed.
		{"failed but retrying", k8s.JobView{Present: true, Failed: 1, Active: 1}, k8s.PodView{Present: true, ContainerStarted: true}, "running"},
		{"running", k8s.JobView{Present: true, Active: 1}, k8s.PodView{Present: true, ContainerStarted: true}, "running"},
		// The state a restore sits in until the StatefulSet is scaled down: the pod
		// cannot attach the data volume, so no container has started.
		{"pending on volume", k8s.JobView{Present: true, Active: 1}, k8s.PodView{Present: true, Phase: "Pending"}, "pending"},
		{"no pod yet", k8s.JobView{Present: true}, k8s.PodView{}, "pending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := restorePhase(tc.job, tc.pod); got != tc.want {
				t.Errorf("restorePhase = %q, want %q", got, tc.want)
			}
		})
	}
}

// A stopped node must report "unknown" rather than a role guessed from its on-disk
// state -- that ambiguity is exactly what the reconcile loop exists to resolve, and
// publishing a guess would make the API contradict the loop.
func TestLocalRole(t *testing.T) {
	cases := []struct {
		name string
		ls   reconcile.LocalState
		want string
	}{
		{"stopped", reconcile.LocalState{HasData: true}, "unknown"},
		{"stopped with primary-state data", reconcile.LocalState{HasData: true, InRecovery: false}, "unknown"},
		{"running standby", reconcile.LocalState{Running: true, InRecovery: true}, "standby"},
		{"running primary", reconcile.LocalState{Running: true}, "primary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := localRole(tc.ls); got != tc.want {
				t.Errorf("localRole = %q, want %q", got, tc.want)
			}
		})
	}
}

// An unreadable LSN must render as empty, never as "0/0" -- which would read as a
// real position at the start of the WAL.
func TestLSNString(t *testing.T) {
	if got := lsnString(pg.LSN{Hi: 0x16, Lo: 0xB374D848}, true); got != "16/B374D848" {
		t.Errorf("lsnString = %q", got)
	}
	if got := lsnString(pg.LSN{}, false); got != "" {
		t.Errorf("an unknown LSN must be empty, got %q", got)
	}
}

func TestIntentBudgetScalesWithReconcileInterval(t *testing.T) {
	if got := intentBudget(5 * time.Second); got != 30*time.Second {
		t.Errorf("default interval: budget = %s, want the 30s floor", got)
	}
	// A cloud preset widens the reconcile interval; the budget must widen with it or
	// every restart would report a spurious timeout.
	if got := intentBudget(15 * time.Second); got != 60*time.Second {
		t.Errorf("wide interval: budget = %s, want 60s", got)
	}
}

// --- intents ---

func newIntentAgent(t *testing.T, pm *fakePostmaster) *agent {
	t.Helper()
	return &agent{
		cfg:     &config.Config{PGDATA: t.TempDir(), PodName: "pg-0"},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		sup:     process.NewSupervisor(pm),
		metr:    observe.New(),
		intents: make(chan intentRequest, 1),
	}
}

// Restart must stop AND start: relying on the reconcile loop to bring Postgres back
// would leave it down under maintenance mode, where the loop is a no-op.
func TestRunIntentRestartStopsAndStarts(t *testing.T) {
	pm := &fakePostmaster{running: true}
	a := newIntentAgent(t, pm)
	if err := a.runIntent(context.Background(), control.IntentRestart); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if !pm.stopped || !pm.started {
		t.Errorf("stopped=%v started=%v, want both", pm.stopped, pm.started)
	}
	// A clean (Fast) stop, not the crash-stop used on the fence path.
	if pm.stopMode != process.Fast {
		t.Errorf("stopMode = %v, want Fast for an operator-requested restart", pm.stopMode)
	}
}

// The restore precondition: stop and stay stopped.
func TestRunIntentStopLeavesPostgresDown(t *testing.T) {
	pm := &fakePostmaster{running: true}
	a := newIntentAgent(t, pm)
	if err := a.runIntent(context.Background(), control.IntentStop); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !pm.stopped {
		t.Error("postgres should be stopped")
	}
	if pm.started {
		t.Error("stop must NOT start it again: the caller is about to restore over this data directory")
	}
}

func TestRunIntentReload(t *testing.T) {
	pm := &fakePostmaster{running: true}
	a := newIntentAgent(t, pm)
	if err := a.runIntent(context.Background(), control.IntentReload); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !pm.reloaded {
		t.Error("reload should signal the postmaster")
	}
	if pm.stopped || pm.started {
		t.Error("a reload must not restart anything")
	}
}

func TestRunIntentUnknownKind(t *testing.T) {
	a := newIntentAgent(t, &fakePostmaster{})
	if err := a.runIntent(context.Background(), control.IntentKind(99)); err == nil {
		t.Error("an unknown intent must be an error, not a silent no-op")
	}
}

// Submit must not block forever when the reconcile loop never gets to the intent --
// the caller's deadline is what turns that into a 504.
func TestSubmitRespectsContextWhenLoopIsBusy(t *testing.T) {
	a := newIntentAgent(t, &fakePostmaster{})
	// Fill the single slot so the enqueue itself blocks.
	a.intents <- intentRequest{kind: control.IntentReload, done: make(chan error, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := (nodeAPI{a: a}).Submit(ctx, control.IntentReload)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

// The round trip a real request makes: enqueue, loop runs it, result comes back.
func TestSubmitReturnsTheLoopsResult(t *testing.T) {
	pm := &fakePostmaster{running: true}
	a := newIntentAgent(t, pm)
	done := make(chan struct{})
	go func() { // stands in for the run loop's select
		defer close(done)
		req := <-a.intents
		req.done <- a.runIntent(context.Background(), req.kind)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := (nodeAPI{a: a}).Submit(ctx, control.IntentRestart); err != nil {
		t.Fatalf("submit: %v", err)
	}
	<-done
	if !pm.stopped || !pm.started {
		t.Error("the intent should have reached the postmaster")
	}
}

// --- snapshot ---

func TestSnapshotBeforeFirstTickReportsIdentityOnly(t *testing.T) {
	a := &agent{cfg: &config.Config{PodName: "pg-0", PGMajor: "18"}}
	snap := (nodeAPI{a: a}).Snapshot()
	if snap.Node != "pg-0" || snap.PGMajor != "18" {
		t.Errorf("identity should be available immediately: %+v", snap)
	}
	if !snap.ObservedAt.IsZero() || snap.HoldsLease || len(snap.Peers) != 0 {
		t.Errorf("no state should be invented before the first tick: %+v", snap)
	}
}

func TestPublishSnapshotMapsObservationAndDecision(t *testing.T) {
	a := &agent{
		cfg:    &config.Config{PodName: "pg-0", PGMajor: "18", PGDATA: t.TempDir()},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		prober: pg.NewProber(),
	}
	obs := reconcile.Observation{
		HoldLease:        true,
		LeaderIdentity:   "pg-0",
		Paused:           true,
		SwitchoverTarget: "pg-1",
		Local: reconcile.LocalState{
			HasData: true, Running: true, Timeline: 7, TimelineOK: true,
			LSN: pg.LSN{Hi: 0x16, Lo: 0xB3000000}, LSNOK: true,
		},
		Peers: []reconcile.PeerState{{
			Name: "pg-1", Reachable: false, Gossip: true, Role: pg.RoleStandby,
			Timeline: 7, TimelineOK: true,
		}},
		Marker: reconcile.MarkerState{Present: true, Primary: "pg-0"},
	}
	dec := reconcile.Decision{Action: reconcile.StayPrimary, Reason: "holds lease"}
	a.publishSnapshot(context.Background(), obs, dec)

	snap := (nodeAPI{a: a}).Snapshot()
	if snap.Local.Role != "primary" || snap.Local.LSN != "16/B3000000" {
		t.Errorf("local: %+v", snap.Local)
	}
	if !snap.Local.Self {
		t.Error("the local member must be flagged as self")
	}
	if !snap.Paused || snap.SwitchoverTarget != "pg-1" || !snap.MarkerPresent {
		t.Errorf("cluster intents: %+v", snap)
	}
	if len(snap.Peers) != 1 || !snap.Peers[0].Gossip || snap.Peers[0].Reachable {
		t.Errorf("peers: %+v", snap.Peers)
	}
	// A gossip-only peer has no LSN of its own recorded here; what matters is that it
	// is marked as gossip so the API never presents it as a live probe.
	if snap.Decision.Action != "StayPrimary" || snap.Decision.Reason != "holds lease" {
		t.Errorf("decision: %+v", snap.Decision)
	}
	if snap.ObservedAt.IsZero() {
		t.Error("ObservedAt must be stamped so a client can judge the age")
	}
}

// Replay progress costs a probe, so it is read only while recovering -- the one time
// it means anything.
func TestPublishSnapshotSkipsReplayProbeWhenNotRecovering(t *testing.T) {
	ex := &countingExec{}
	a := &agent{
		cfg:    &config.Config{PodName: "pg-0", PGDATA: t.TempDir()},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		prober: &pg.Prober{Exec: ex},
	}
	a.publishSnapshot(context.Background(), reconcile.Observation{
		Local: reconcile.LocalState{Running: true, InRecovery: false},
	}, reconcile.Decision{Action: reconcile.StayPrimary})
	if ex.calls != 0 {
		t.Errorf("no probe should run for a non-recovering node, got %d", ex.calls)
	}
	if snap := (nodeAPI{a: a}).Snapshot(); snap.Recovery.InRecovery {
		t.Errorf("recovery: %+v", snap.Recovery)
	}
}

// countingExec counts every command run, so a test can assert that NO probe happened.
type countingExec struct{ calls int }

func (c *countingExec) Run(context.Context, []string, string, ...string) (string, error) {
	c.calls++
	return "", nil
}
