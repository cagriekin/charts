package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cagriekin/pg-ha-agent/internal/config"
	"github.com/cagriekin/pg-ha-agent/internal/control"
	"github.com/cagriekin/pg-ha-agent/internal/k8s"
	"github.com/cagriekin/pg-ha-agent/internal/observe"
	"github.com/cagriekin/pg-ha-agent/internal/pg"
	"github.com/cagriekin/pg-ha-agent/internal/process"
	"github.com/cagriekin/pg-ha-agent/internal/reconcile"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
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
		cfg: &config.Config{PGDATA: t.TempDir(), PodName: "pg-0", MarkerName: "pg-primary"},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// A non-leader DCS, because runIntent's destructive verbs re-check holdership under
		// opMu (#298 review) -- see TestRunIntentReinitializeRefusesOnceThisNodeIsPrimary.
		dcs: &fakeDCS{},
		// A real k8s client over a fake clientset: IntentStop now re-reads the pause marker
		// FRESH (#339), so it needs a working kube. An absent marker ConfigMap reads as
		// Present:false / Paused:false, which is the correct "not paused" for tests that
		// never create one.
		kube:    k8s.NewWithClient(k8sfake.NewSimpleClientset(), "ns"),
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
	if err := a.runIntent(context.Background(), intentRequest{kind: control.IntentRestart}); err != nil {
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

// #298 review: the pause is re-asserted at DEQUEUE time. The handler verified it before Submit,
// but the intent outlives the caller's deadline -- a restore accepted while act() sat inside a
// multi-hour clone, timed out at the client, and the operator resumed the cluster; hours later the
// dequeued stop took down the serving primary for a restore nobody was still running.
func TestRunIntentStopRefusesOnceTheClusterIsNoLongerPaused(t *testing.T) {
	pm := &fakePostmaster{running: true}
	a := newIntentAgent(t, pm)
	// No paused marker written: the fresh re-read (#339) sees an unpaused cluster.
	err := a.runIntent(context.Background(), intentRequest{kind: control.IntentStop})
	if err == nil || !strings.Contains(err.Error(), "no longer paused") {
		t.Fatalf("expected a refusal naming the lifted pause, got %v", err)
	}
	if pm.stopped {
		t.Error("the postmaster must not be stopped when the pause is gone")
	}
}

// The restore precondition: stop and stay stopped.
func TestRunIntentStopLeavesPostgresDown(t *testing.T) {
	pm := &fakePostmaster{running: true}
	a := newIntentAgent(t, pm)
	// The stop re-asserts the pause at dequeue with a FRESH read (#298 review, #339), so the
	// paused state must live in the marker, not just a.lastMarker.
	ctx := context.Background()
	if err := a.kube.WriteMarker(ctx, a.cfg.MarkerName, "pg-0", 1); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	if err := a.kube.SetPause(ctx, a.cfg.MarkerName, true, "dba"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := a.runIntent(ctx, intentRequest{kind: control.IntentStop}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !pm.stopped {
		t.Error("postgres should be stopped")
	}
	if pm.started {
		t.Error("stop must NOT start it again: the caller is about to restore over this data directory")
	}
}

// #339 regression: the pause re-assertion must read the marker FRESH, not the reconcile
// loop's cached a.lastMarker. The documented runbook pauses and IMMEDIATELY requests the
// restore, so with no reconcile tick in between a.lastMarker still reads unpaused -- and the
// stop was refused on a correctly-paused cluster, making POST /v1/restore intermittently 500.
func TestRunIntentStopReadsThePauseFreshNotTheLoopCache(t *testing.T) {
	pm := &fakePostmaster{running: true}
	a := newIntentAgent(t, pm)
	// The loop's cache is STALE: it last saw the cluster unpaused.
	a.lastMarkerOK, a.lastMarker.Paused = true, false
	// The operator has just paused it; the marker is fresh even though the loop has not ticked.
	ctx := context.Background()
	if err := a.kube.WriteMarker(ctx, a.cfg.MarkerName, "pg-0", 1); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	if err := a.kube.SetPause(ctx, a.cfg.MarkerName, true, "dba"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := a.runIntent(ctx, intentRequest{kind: control.IntentStop}); err != nil {
		t.Fatalf("stop must proceed on a freshly-paused cluster despite a stale loop cache: %v", err)
	}
	if !pm.stopped {
		t.Error("postgres should be stopped when the fresh marker reports paused")
	}
}

func TestRunIntentReload(t *testing.T) {
	pm := &fakePostmaster{running: true}
	a := newIntentAgent(t, pm)
	if err := a.runIntent(context.Background(), intentRequest{kind: control.IntentReload}); err != nil {
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
	if err := a.runIntent(context.Background(), intentRequest{kind: control.IntentKind(99)}); err == nil {
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
		req.done <- a.runIntent(context.Background(), req)
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

// Reinitialize stops PostgreSQL and discards the data directory -- and nothing else. The
// rebuild is the reconcile loop's ordinary "empty data, not the chosen primary -> clone
// from the lease holder" path, so there is no second clone implementation to diverge.
func TestRunIntentReinitializeStopsAndWipes(t *testing.T) {
	pm := &fakePostmaster{running: true}
	a := newIntentAgent(t, pm)
	// Make PGDATA look like a real, stopped data directory.
	if err := os.MkdirAll(filepath.Join(a.cfg.PGDATA, "base"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"PG_VERSION", "postgresql.conf"} {
		if err := os.WriteFile(filepath.Join(a.cfg.PGDATA, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if !process.HasData(a.cfg.PGDATA) {
		t.Fatal("fixture should look like a data directory")
	}
	if err := a.runIntent(context.Background(), intentRequest{kind: control.IntentReinitialize}); err != nil {
		t.Fatalf("reinitialize: %v", err)
	}
	if !pm.stopped {
		t.Error("postgres must be stopped before the data directory is discarded")
	}
	if pm.started {
		t.Error("it must NOT start postgres: there is no data to start, the loop re-clones")
	}
	if process.HasData(a.cfg.PGDATA) {
		t.Error("the data directory should be empty so the loop sees BootstrapClone")
	}
	// The directory itself must survive (it is a volume mount).
	if fi, err := os.Stat(a.cfg.PGDATA); err != nil || !fi.IsDir() {
		t.Errorf("PGDATA itself must remain: %v", err)
	}
}

// The restore record lives BESIDE PGDATA so it survives the Job that writes it -- which
// means the wipe cannot reach it. A rebuilt directory holds data cloned from the primary,
// so leaving the record would make GET /v1/status report a backup set as the provenance of
// data that never came from one.
func TestRunIntentReinitializeInvalidatesTheRestoreRecord(t *testing.T) {
	pm := &fakePostmaster{running: true}
	a := newIntentAgent(t, pm)
	if err := os.WriteFile(filepath.Join(a.cfg.PGDATA, "PG_VERSION"), []byte("18"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := a.cfg.RestoreStatusPath()
	if err := os.WriteFile(rec, []byte("exitCode=0\nbackupSet=20260801-120000F\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.runIntent(context.Background(), intentRequest{kind: control.IntentReinitialize}); err != nil {
		t.Fatalf("reinitialize: %v", err)
	}
	if _, err := os.Stat(rec); !os.IsNotExist(err) {
		t.Errorf("the restore record must be gone after a wipe, got err=%v", err)
	}
}

// Best effort: a missing record is the normal case (no restore ever ran here) and must not
// turn a completed rebuild into a failure.
func TestDropRestoreRecordToleratesAMissingFile(t *testing.T) {
	a := newIntentAgent(t, &fakePostmaster{})
	a.dropRestoreRecord("test") // must not panic or log an error path
	if _, err := os.Stat(a.cfg.RestoreStatusPath()); !os.IsNotExist(err) {
		t.Errorf("nothing should have been created: %v", err)
	}
}

// A postmaster that ignores SIGINT must not hold the reconcile loop (and the leadership
// fence behind the same mutex) for the life of the process: the request's deadline travels
// with the intent, so the stop escalates exactly as the fence path does.
func TestRunIntentStopIsBoundedByTheRequestDeadline(t *testing.T) {
	pm := &fakePostmaster{running: true, stopErr: context.DeadlineExceeded}
	a := newIntentAgent(t, pm)
	// The stop re-asserts the pause at dequeue with a FRESH read (#298 review, #339).
	ctx := context.Background()
	if err := a.kube.WriteMarker(ctx, a.cfg.MarkerName, "pg-0", 1); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	if err := a.kube.SetPause(ctx, a.cfg.MarkerName, true, "dba"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	req := intentRequest{kind: control.IntentStop, deadline: time.Now().Add(50 * time.Millisecond)}
	err := a.runIntent(context.Background(), req)
	if err == nil {
		t.Fatal("a stop that had to be forced must be reported as a failure: SIGKILL leaves postmaster.pid, which blocks the restore Job")
	}
	if !pm.stopCtxHadDeadline {
		t.Error("the stop must run under the request's deadline, not the loop's root context")
	}
}

// ... but a restart must still bring PostgreSQL back up after a forced stop, or the node
// stays down for exactly the wedge this escalated past.
func TestRunIntentRestartStartsAgainAfterAForcedStop(t *testing.T) {
	pm := &fakePostmaster{running: true, stopErr: context.DeadlineExceeded, deadOnStop: true}
	a := newIntentAgent(t, pm)
	req := intentRequest{kind: control.IntentRestart, deadline: time.Now().Add(50 * time.Millisecond)}
	if err := a.runIntent(context.Background(), req); err != nil {
		t.Fatalf("restart after a forced stop: %v", err)
	}
	if !pm.started {
		t.Error("postgres must be started again")
	}
}

// A stop that failed for any other reason, or that left the child alive, is still fatal:
// starting a second postmaster on the same data directory is not a recovery.
func TestRunIntentRestartDoesNotStartAfterAFailedStop(t *testing.T) {
	pm := &fakePostmaster{running: true, stopErr: errors.New("boom")}
	a := newIntentAgent(t, pm)
	if err := a.runIntent(context.Background(), intentRequest{kind: control.IntentRestart}); err == nil {
		t.Fatal("a failed stop must fail the restart")
	}
	if pm.started {
		t.Error("it must not start a postmaster on top of a child that would not stop")
	}
}

// If the wipe is refused (its own interlocks), the intent must fail rather than report
// success and leave the loop nothing to do.
func TestRunIntentReinitializeSurfacesWipeRefusal(t *testing.T) {
	pm := &fakePostmaster{running: true}
	a := newIntentAgent(t, pm)
	// PGDATA exists but is not an initialized data directory -> WipeDataDir refuses.
	err := a.runIntent(context.Background(), intentRequest{kind: control.IntentReinitialize})
	if err == nil {
		t.Fatal("a refused wipe must surface as an error")
	}
	if !strings.Contains(err.Error(), "PG_VERSION") {
		t.Errorf("the error should carry the reason: %v", err)
	}
}

// A request that omits the recovery point must INHERIT whatever values pinned, not blank
// it. Overriding unconditionally would restore the latest backup and replay all WAL over
// the live data directory instead of stopping at the reviewed point in time.
func TestRestoreEnvOverridesOnlyWhatTheRequestSpecified(t *testing.T) {
	b := backupAPI{a: &agent{cfg: &config.Config{}}}

	// Nothing specified: no target keys at all, so the rendered values stand.
	empty := b.restoreEnvOverrides(control.RestoreRequest{Force: true})
	for _, k := range []string{"TARGET_TYPE", "TARGET", "BACKUP_SET"} {
		if _, ok := empty[k]; ok {
			t.Errorf("%s must not be overridden when the request omits it: %v", k, empty)
		}
	}

	// A backup set alone must not blank the values-pinned PITR target.
	setOnly := b.restoreEnvOverrides(control.RestoreRequest{BackupSet: "20260801-090002F"})
	if setOnly["BACKUP_SET"] != "20260801-090002F" {
		t.Errorf("BACKUP_SET should be overridden: %v", setOnly)
	}
	if _, ok := setOnly["TARGET_TYPE"]; ok {
		t.Errorf("TARGET_TYPE must be left alone: %v", setOnly)
	}

	// A target overrides both halves together (they are validated to arrive together).
	tgt := b.restoreEnvOverrides(control.RestoreRequest{TargetType: "time", Target: "2026-08-01 09:55:00+00"})
	if tgt["TARGET_TYPE"] != "time" || tgt["TARGET"] != "2026-08-01 09:55:00+00" {
		t.Errorf("both target halves should be set: %v", tgt)
	}
	if _, ok := tgt["BACKUP_SET"]; ok {
		t.Errorf("BACKUP_SET must be left alone: %v", tgt)
	}
	// FORCE is still never touched.
	for _, m := range []map[string]string{empty, setOnly, tgt} {
		if _, ok := m["FORCE"]; ok {
			t.Errorf("FORCE must never be overridden: %v", m)
		}
	}
}

// --- the control API's cluster adapter, end to end against a fake apiserver ---

// clusterAPI is what /v1/pause, /v1/resume and /v1/switchover write through, and every
// one of those verbs is an operator action taken during an incident. The adapter is
// thin, which is exactly why a mis-wired field is invisible in review: it would compile,
// return no error, and quietly write the wrong marker key.
func TestClusterAPIRoundTripsThroughTheMarker(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()
	a := &agent{
		cfg:  &config.Config{MarkerName: "pg-primary", PodName: "pg-0"},
		kube: k8s.NewWithClient(cs, "ns"),
	}
	c := clusterAPI{a: a}
	ctx := context.Background()

	// Nothing written yet: absent is a legitimate state, not an error.
	m, err := c.Marker(ctx)
	if err != nil {
		t.Fatalf("an absent marker must not be an error: %v", err)
	}
	if m.Present {
		t.Error("no marker has been written yet")
	}

	// Pausing annotates the marker the cluster recorded when it first elected a
	// primary; there is nothing to annotate before that, which the adapter reports
	// rather than inventing.
	if err := c.SetPause(ctx, true, "dba"); err == nil {
		t.Error("pausing a cluster that has never recorded a primary must be refused, not silently no-op")
	}
	if err := a.kube.WriteMarker(ctx, "pg-primary", "pg-0", 1); err != nil {
		t.Fatal(err)
	}

	if err := c.SetPause(ctx, true, "dba"); err != nil {
		t.Fatal(err)
	}
	m, err = c.Marker(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Paused {
		t.Error("the pause did not reach the marker: automatic failover would still run")
	}
	// PausedBy is what the audit trail and the resume warning both read.
	if m.PausedBy != "dba" {
		t.Errorf("PausedBy = %q, want dba", m.PausedBy)
	}

	if err := c.SetSwitchoverTarget(ctx, "pg-1", "dba"); err != nil {
		t.Fatal(err)
	}
	if m, _ = c.Marker(ctx); m.SwitchoverTarget != "pg-1" {
		t.Errorf("SwitchoverTarget = %q, want pg-1", m.SwitchoverTarget)
	}

	if err := c.ClearSwitchoverTarget(ctx); err != nil {
		t.Fatal(err)
	}
	m, _ = c.Marker(ctx)
	if m.SwitchoverTarget != "" {
		t.Errorf("SwitchoverTarget = %q after the clear, want empty: the handoff would repeat every tick", m.SwitchoverTarget)
	}
	// Clearing the switchover must not clear the pause: they are independent operator
	// decisions on the same object, and a merge that replaced the map would drop one.
	if !m.Paused {
		t.Error("clearing the switchover target also cleared the pause")
	}

	if err := c.SetPause(ctx, false, "dba"); err != nil {
		t.Fatal(err)
	}
	if m, _ = c.Marker(ctx); m.Paused {
		t.Error("resume did not clear the pause")
	}
}

// An apiserver failure must SURFACE rather than read as "no marker": the handlers gate
// destructive verbs on Paused, and a swallowed error would present an unpaused cluster
// as safe to restore.
func TestClusterAPIMarkerReportsAReadFailure(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("injected apiserver failure")
	})
	c := clusterAPI{a: &agent{
		cfg:  &config.Config{MarkerName: "pg-primary"},
		kube: k8s.NewWithClient(cs, "ns"),
	}}
	if _, err := c.Marker(context.Background()); err == nil {
		t.Fatal("an unreadable marker must not be reported as an absent one")
	}
}

// HoldsLeaseNow reads the LIVE DCS rather than the snapshot, because the control
// listener accepts requests before the first tick has published anything -- and the
// restart interlock keys on it. A snapshot-based answer would be false during exactly
// the window an unforced restart of the serving primary must be refused.
func TestNodeAPIHoldsLeaseNowReadsTheLiveDCS(t *testing.T) {
	d := &fakeDCS{}
	a := &agent{cfg: &config.Config{PodName: "pg-0"}, dcs: d}
	n := nodeAPI{a: a}
	if n.HoldsLeaseNow() {
		t.Error("a node that holds nothing must not report the lease")
	}
	d.leader = true
	if !n.HoldsLeaseNow() {
		t.Error("HoldsLeaseNow did not follow the live DCS")
	}
	// And it is NOT reading the snapshot, which is still unpublished.
	if s := n.Snapshot(); s.HoldsLease {
		t.Error("the snapshot should still be empty; HoldsLeaseNow must not be sourced from it")
	}
}

// Before the first tick the snapshot reports IDENTITY ONLY -- every position field zero
// and ObservedAt zero -- rather than inventing state. The API surfaces that zero
// ObservedAt as the "has not completed its first reconcile tick" case, which several
// interlocks fail closed on.
func TestNodeAPISnapshotBeforeTheFirstTick(t *testing.T) {
	n := nodeAPI{a: &agent{cfg: &config.Config{PodName: "pg-0", PGMajor: "18"}}}
	s := n.Snapshot()
	if s.Node != "pg-0" || s.PGMajor != "18" {
		t.Errorf("identity missing from the pre-tick snapshot: %+v", s)
	}
	if !s.ObservedAt.IsZero() {
		t.Error("ObservedAt must stay zero until a tick publishes, or the interlocks read stale state as fresh")
	}
	if s.Local.Running || s.Local.Role != "" || s.LeaseHolder != "" {
		t.Errorf("the pre-tick snapshot invented state: %+v", s)
	}
}

// RestoreEnabled mirrors the chart's control.restore.enabled. It gates both the restore
// verb and the in-flight-restore interlock, and the interlock is skipped entirely when
// it is false -- because the `get jobs` grant that reads the Job is rendered only under
// the same value, so consulting it otherwise would fail closed on an RBAC error.
func TestBackupAPIRestoreEnabledFollowsTheRenderedValue(t *testing.T) {
	if (backupAPI{a: &agent{cfg: &config.Config{}}}).RestoreEnabled() {
		t.Error("restore must be off unless the chart turned it on")
	}
	if !(backupAPI{a: &agent{cfg: &config.Config{ControlRestoreEnabled: true}}}).RestoreEnabled() {
		t.Error("RestoreEnabled did not follow the config")
	}
}

// Reinitialize RE-CHECKS the replica-only gate inside the reconcile goroutine (#298 review).
// handleReinitialize's own checks all run on the HTTP goroutine before the intent is queued,
// and the loop may serve a tick first -- so a standby that wins the lease and is promoted in
// that window would otherwise have the cluster's new primary wiped out from under it.
func TestRunIntentReinitializeRefusesOnceThisNodeIsPrimary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*agent)
		want  string
	}{
		{"holds the lease", func(a *agent) { a.dcs = &fakeDCS{leader: true} }, "acquired the leader lease"},
		{"marked read-write", func(a *agent) { a.servingRW.Store(true) }, "marked read-write"},
		{"marker names this pod", func(a *agent) {
			a.lastMarker, a.lastMarkerOK = k8s.Marker{Present: true, Primary: "pg-0"}, true
		}, "primary marker now records"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pm := &fakePostmaster{running: true}
			a := newIntentAgent(t, pm)
			for _, f := range []string{"PG_VERSION", "postgresql.conf"} {
				if err := os.WriteFile(filepath.Join(a.cfg.PGDATA, f), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tc.setup(a)
			err := a.runIntent(context.Background(), intentRequest{kind: control.IntentReinitialize})
			if err == nil {
				t.Fatal("reinitialize must be refused once this node may be the primary")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			// And nothing may have been touched: the refusal precedes the stop and the wipe.
			if pm.stopped {
				t.Error("the postmaster must not be stopped by a refused reinitialize")
			}
			if !process.HasData(a.cfg.PGDATA) {
				t.Error("the data directory must survive a refused reinitialize")
			}
		})
	}
}

// The control-API restart asserts the on-disk role before starting (#298 review).
// sup.Start is a bare `postgres -D PGDATA`, so without this a fenced ex-primary -- stopped,
// primary-state data, no standby.signal, no lease -- came back READ-WRITE beside the real
// primary until the next tick's DemoteFence.
func TestRunIntentRestartPinsANonHolderReadOnly(t *testing.T) {
	pm := &fakePostmaster{running: true}
	a := newIntentAgent(t, pm)
	a.pgControlDataBin = "/nonexistent/pg_controldata" // the unreadable-controldata arm
	for _, f := range []string{"PG_VERSION", "postgresql.conf"} {
		if err := os.WriteFile(filepath.Join(a.cfg.PGDATA, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a.prober = pg.NewProber()
	if err := a.runIntent(context.Background(), intentRequest{kind: control.IntentRestart}); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if !pm.stopped || !pm.started {
		t.Fatalf("stopped=%v started=%v, want both", pm.stopped, pm.started)
	}
	if _, err := os.Stat(filepath.Join(a.cfg.PGDATA, "standby.signal")); err != nil {
		t.Errorf("standby.signal must exist before a non-holder is started: %v", err)
	}
}

// ...and it never REMOVES a signal, not even on the lease holder: that would be a new way to
// bring a node up read-write, bypassing the guards act() applies (the #125 highwater check).
//
// Named for what it asserts (#298 review): with the control file unreadable the holder is pinned
// READ-ONLY like anyone else, so the old name ("LeavesTheHoldersDirectoryAlone") described the
// readable-primary-state case this test does not exercise. Leaving it alone is what happens once
// pg_controldata answers, which is the switch case below the fail-safe arm.
func TestRunIntentRestartPinsTheHolderReadOnlyOnAnUnreadableControlFile(t *testing.T) {
	pm := &fakePostmaster{running: true}
	a := newIntentAgent(t, pm)
	a.dcs = &fakeDCS{leader: true}
	a.pgControlDataBin = "/nonexistent/pg_controldata"
	a.prober = pg.NewProber()
	for _, f := range []string{"PG_VERSION", "postgresql.conf"} {
		if err := os.WriteFile(filepath.Join(a.cfg.PGDATA, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.runIntent(context.Background(), intentRequest{kind: control.IntentRestart}); err != nil {
		t.Fatalf("restart: %v", err)
	}
	// The control file is unreadable here, which is the fail-safe arm: the holder still gets
	// standby.signal rather than a read-write start on a role nothing could confirm.
	if _, err := os.Stat(filepath.Join(a.cfg.PGDATA, "standby.signal")); err != nil {
		t.Errorf("an unreadable control file must still pin the node read-only: %v", err)
	}
}
