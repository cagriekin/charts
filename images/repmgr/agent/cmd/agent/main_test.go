package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"net/http/httptest"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/cagriekin/pg-ha-agent/internal/config"
	"github.com/cagriekin/pg-ha-agent/internal/dcs"
	"github.com/cagriekin/pg-ha-agent/internal/k8s"
	"github.com/cagriekin/pg-ha-agent/internal/mechanism"
	"github.com/cagriekin/pg-ha-agent/internal/observe"
	"github.com/cagriekin/pg-ha-agent/internal/pg"
	"github.com/cagriekin/pg-ha-agent/internal/pgbackrest"
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
	// #317: the apiserver route is read from the environment, not from cfg, so pin it
	// or this asserts whatever the developer's shell happens to have set.
	t.Setenv("KUBECONFIG", "")
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
		// #317: the one field that says which apiserver address this agent is using.
		// A denied-egress hang and a misrouted kubeconfig log the same dial timeout,
		// so losing this field costs the only way to tell them apart.
		"apiserver=in-cluster",
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

// The kubeconfig route must be distinguishable in the boot log from the in-cluster
// one, and must name the file -- "some kubeconfig" is not an answer during an
// incident on a cluster that mounts more than one (#317).
func TestLogStartupConfigNamesTheKubeconfigRoute(t *testing.T) {
	t.Setenv("KUBECONFIG", "/etc/apiserver-proxy/kubeconfig")
	var out bytes.Buffer
	logStartupConfig(slog.New(slog.NewTextHandler(&out, nil)), &config.Config{PodName: "pg-0", Namespace: "db"})
	if want := "apiserver=\"kubeconfig /etc/apiserver-proxy/kubeconfig\""; !strings.Contains(out.String(), want) {
		t.Errorf("startup log missing %s: %s", want, out.String())
	}
}

// --- fakes for the act() path ---

type fakePostmaster struct {
	started   bool
	stopped   bool
	stopMode  process.StopMode
	reloaded  bool
	reloadErr error
	running   bool
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

// reloadErr, when set, is what Reload returns -- used to test the #308 recovery path
// where a reload failure must not permanently strand a patched-but-not-reloaded
// primary_conninfo behind the "already streaming" shortcut.
func (f *fakePostmaster) Reload(context.Context) error {
	f.reloaded = true
	return f.reloadErr
}
func (f *fakePostmaster) Running() bool { return f.running }

type fakeDCS struct {
	released bool
	// leader lets a test hold the lease. Default false, matching every pre-existing use.
	leader bool
}

func (f *fakeDCS) Run(context.Context, string, dcs.Callbacks) {}
func (f *fakeDCS) IsLeader() bool                             { return f.leader }
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
	slots        string // pg_replication_slots rows (psql): "name|active|retainedBytes|wal_status|reserving" per line (#308 + #289)
	slotSyncSQL  []string
	nodesQueries int // number of `SELECT ... FROM repmgr.nodes` calls (psql)
}

func (s *scriptedExec) Run(_ context.Context, _ []string, name string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	switch {
	case name == "psql" && strings.Contains(joined, "pg_replication_slots"):
		return s.slots, nil
	case name == "psql" && (strings.Contains(joined, "ALTER SYSTEM") || strings.Contains(joined, "pg_reload_conf")):
		s.slotSyncSQL = append(s.slotSyncSQL, joined)
		return "", nil
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

// #308: repmgr's own `standby follow` writes primary_conninfo without dbname (PG17+'s
// sync_replication_slots worker needs it, physical replication does not). act() must
// patch it in and reload so the change takes effect on this already-running standby.
func TestActFollowPatchesPrimaryConninfoDBNameAndReloads(t *testing.T) {
	ex := &scriptedExec{walRcv: ""} // no walreceiver row -- Follow runs
	pm := &fakePostmaster{}
	a := newFollowTestAgentWithPM(t, ex, pm)
	confPath := filepath.Join(a.cfg.PGDATA, "postgresql.auto.conf")
	if err := os.WriteFile(confPath, []byte(
		`primary_conninfo = 'host=''pg-0.h'' port=5432 user=repmgr application_name=''pg-1'' connect_timeout=10'`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dec := reconcile.Decision{Action: reconcile.Follow, Target: "pg-0"}
	if err := a.act(context.Background(), dec, reconcile.Observation{}); err != nil {
		t.Fatalf("act: %v", err)
	}
	b, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "dbname=") {
		t.Errorf("primary_conninfo was not patched with dbname:\n%s", b)
	}
	if !pm.reloaded {
		t.Error("act must reload the postmaster after patching primary_conninfo")
	}
}

// The dbname patch must also run on the "already streaming" shortcut path (no real
// `repmgr standby follow` call), not only after a real Follow -- a repmgrd->agent
// migration or a post-failover rejoin can leave a standby already streaming with a
// primary_conninfo repmgr itself never patched.
func TestActFollowPatchesPrimaryConninfoDBNameOnAlreadyStreamingShortcut(t *testing.T) {
	ex := &scriptedExec{walRcv: "pg-0.h|streaming"} // already streaming -> shortcut, no real Follow
	pm := &fakePostmaster{}
	a := newFollowTestAgentWithPM(t, ex, pm)
	confPath := filepath.Join(a.cfg.PGDATA, "postgresql.auto.conf")
	if err := os.WriteFile(confPath, []byte(
		`primary_conninfo = 'host=''pg-0.h'' port=5432 user=repmgr application_name=''pg-1'' connect_timeout=10'`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dec := reconcile.Decision{Action: reconcile.Follow, Target: "pg-0"}
	if err := a.act(context.Background(), dec, reconcile.Observation{}); err != nil {
		t.Fatalf("act: %v", err)
	}
	if ex.follows != 0 {
		t.Fatalf("repmgr standby follow must still be skipped when already streaming, got %d calls", ex.follows)
	}
	b, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "dbname=") {
		t.Errorf("primary_conninfo was not patched with dbname on the shortcut path:\n%s", b)
	}
	if !pm.reloaded {
		t.Error("act must reload the postmaster after patching primary_conninfo, even on the shortcut path")
	}
}

// A reload failure must not permanently strand a patched-but-not-reloaded
// primary_conninfo: act() returns an error (so followUpstream never latches), but the
// FILE is already patched -- the next tick must retry the reload via the
// already-streaming shortcut rather than silently giving up once streaming looks fine.
func TestActFollowRetriesReloadAfterPriorFailure(t *testing.T) {
	ex := &scriptedExec{walRcv: ""} // not yet streaming -> first tick runs a real Follow
	pm := &fakePostmaster{reloadErr: errors.New("reload failed")}
	a := newFollowTestAgentWithPM(t, ex, pm)
	confPath := filepath.Join(a.cfg.PGDATA, "postgresql.auto.conf")
	if err := os.WriteFile(confPath, []byte(
		`primary_conninfo = 'host=''pg-0.h'' port=5432 user=repmgr application_name=''pg-1'' connect_timeout=10'`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dec := reconcile.Decision{Action: reconcile.Follow, Target: "pg-0"}
	if err := a.act(context.Background(), dec, reconcile.Observation{}); err == nil {
		t.Fatal("expected the reload failure to surface as an error")
	}
	if a.followUpstream == "pg-0" {
		t.Fatal("followUpstream must not latch when the reload failed")
	}
	b, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "dbname=") {
		t.Fatalf("the file should already be patched from the failed attempt:\n%s", b)
	}

	// Second tick: the standby is now streaming (the underlying replication connection
	// never depended on dbname), so this would take the already-streaming shortcut. The
	// reload must still be retried -- fix the postmaster and confirm it recovers.
	ex.walRcv = "pg-0.h|streaming"
	followsBefore := ex.follows
	pm.reloadErr = nil
	pm.reloaded = false
	if err := a.act(context.Background(), dec, reconcile.Observation{}); err != nil {
		t.Fatalf("act: %v", err)
	}
	if ex.follows != followsBefore {
		t.Fatalf("the retry must go through the shortcut, not another real follow, got %d calls (was %d)", ex.follows, followsBefore)
	}
	if !pm.reloaded {
		t.Error("the second tick must retry the reload rather than silently latching")
	}
	if a.followUpstream != "pg-0" {
		t.Error("followUpstream must latch once the retried reload succeeds")
	}
}

// A standby that has no postgresql.auto.conf primary_conninfo line to patch (or no file
// at all) must not fail the Follow action -- physical replication itself does not depend
// on this fix, so a missing/malformed file is logged, not fatal.
func TestActFollowToleratesMissingPrimaryConninfoFile(t *testing.T) {
	ex := &scriptedExec{walRcv: ""}
	a := newFollowTestAgent(t, ex)
	dec := reconcile.Decision{Action: reconcile.Follow, Target: "pg-0"}
	if err := a.act(context.Background(), dec, reconcile.Observation{}); err != nil {
		t.Fatalf("act must not fail when postgresql.auto.conf is absent: %v", err)
	}
	if a.followUpstream != "pg-0" {
		t.Fatal("followUpstream must still latch")
	}
}

// #308: a fresh clone's primary_conninfo (written by repmgr's own `standby clone`) must
// be patched with dbname BEFORE Start, so the fresh boot picks it up with no extra
// reload -- unlike Follow, which patches an already-running standby.
func TestActBootstrapClonePatchesPrimaryConninfoBeforeStart(t *testing.T) {
	ex := &scriptedExec{}
	pm := &fakePostmaster{}
	a := newFollowTestAgentWithPM(t, ex, pm)
	confPath := filepath.Join(a.cfg.PGDATA, "postgresql.auto.conf")
	if err := os.WriteFile(confPath, []byte(
		`primary_conninfo = 'host=''pg-0.h'' port=5432 user=repmgr application_name=''pg-1'' connect_timeout=10'`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dec := reconcile.Decision{Action: reconcile.BootstrapClone, Target: "pg-0"}
	if err := a.act(context.Background(), dec, reconcile.Observation{}); err != nil {
		t.Fatalf("act: %v", err)
	}
	b, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "dbname=") {
		t.Errorf("primary_conninfo was not patched with dbname before Start:\n%s", b)
	}
	if !pm.started {
		t.Error("act must still start the postmaster after a successful clone")
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

// #308: no-op entirely (byte-identical to today) unless cfg.SyncReplicationSlots is set,
// even with active standby slots to reconcile.
func TestAssertSyncStandbySlotsNoOpWhenDisabled(t *testing.T) {
	ex := &scriptedExec{slots: "repmgr_slot_1001|t|0|reserved|t\n"}
	a := newFollowTestAgent(t, ex)
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), true)
	if len(ex.slotSyncSQL) != 0 {
		t.Fatalf("expected no SQL when disabled, got %v", ex.slotSyncSQL)
	}
}

// The desired set is registered, non-ghost standby node IDs intersected with slots
// that actually exist -- not pg_replication_slots.active (see the function's doc
// comment for why: that flapped on any blip). nodes and slots both need setting for a
// standby to be reconciled in.
func TestAssertSyncStandbySlotsReconciles(t *testing.T) {
	ex := &scriptedExec{nodes: "1001\n1002\n", slots: "repmgr_slot_1001|t|0|reserved|t\nrepmgr_slot_1002|t|0|reserved|t\n"}
	a := newFollowTestAgent(t, ex)
	a.cfg.SyncReplicationSlots = true
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), true)
	if len(ex.slotSyncSQL) != 2 {
		t.Fatalf("expected ALTER SYSTEM + reload (2 calls), got %v", ex.slotSyncSQL)
	}
	if !strings.Contains(ex.slotSyncSQL[0], "repmgr_slot_1001,repmgr_slot_1002") {
		t.Errorf("first call = %q, want the joined slot list", ex.slotSyncSQL[0])
	}
	if want := "repmgr_slot_1001,repmgr_slot_1002"; a.lastSyncStandbySlots == nil || *a.lastSyncStandbySlots != want {
		t.Errorf("lastSyncStandbySlots = %v, want %q", a.lastSyncStandbySlots, want)
	}
}

// repmgr.nodes has no ORDER BY, so StandbyNodeIDs' row order is not guaranteed stable
// between calls even when the standby SET is unchanged. desired doubles as the cache
// key (a bare string compare), so an unstable order would make the primary re-run ALTER
// SYSTEM + reload every tick even in the steady state -- churn indistinguishable from a
// real topology change. Two ticks with the same IDs in a DIFFERENT order must produce
// byte-identical output and no second SQL call.
func TestAssertSyncStandbySlotsOrderIndependent(t *testing.T) {
	ex := &scriptedExec{nodes: "1002\n1001\n", slots: "repmgr_slot_1001|t|0|reserved|t\nrepmgr_slot_1002|t|0|reserved|t\n"}
	a := newFollowTestAgent(t, ex)
	a.cfg.SyncReplicationSlots = true
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), true)
	if want := "repmgr_slot_1001,repmgr_slot_1002"; a.lastSyncStandbySlots == nil || *a.lastSyncStandbySlots != want {
		t.Fatalf("lastSyncStandbySlots = %v, want %q (sorted regardless of repmgr.nodes row order)", a.lastSyncStandbySlots, want)
	}
	if len(ex.slotSyncSQL) != 2 {
		t.Fatalf("first call: expected 2 SQL statements, got %v", ex.slotSyncSQL)
	}
	// Same standbys, rows returned in the OTHER order -- must be treated as unchanged.
	ex.nodes = "1001\n1002\n"
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), true)
	if len(ex.slotSyncSQL) != 2 {
		t.Fatalf("second call with the same standbys in a different row order must not issue more SQL, got %v", ex.slotSyncSQL)
	}
}

// A registered standby whose walsender briefly drops (a restart, a rolling upgrade, a
// network blip) must NOT be dropped from synchronized_standby_slots -- only genuine
// ghosts (see the next test) and standbys with no physical slot at all are excluded.
// PhysicalSlots (unlike the earlier active-filtered version) does not care whether the
// slot is currently attached, only that it exists.
func TestAssertSyncStandbySlotsSurvivesABlip(t *testing.T) {
	ex := &scriptedExec{nodes: "1001\n", slots: "repmgr_slot_1001|t|0|reserved|t\n"}
	a := newFollowTestAgent(t, ex)
	a.cfg.SyncReplicationSlots = true
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), true)
	if want := "repmgr_slot_1001"; a.lastSyncStandbySlots == nil || *a.lastSyncStandbySlots != want {
		t.Errorf("lastSyncStandbySlots = %v, want %q", a.lastSyncStandbySlots, want)
	}
}

// A ghost node (scaled down, per cleanupGhostNodes' own ordinal-vs-NodeCount
// discriminator) must not appear in synchronized_standby_slots even though its slot
// object may still exist momentarily.
func TestAssertSyncStandbySlotsExcludesGhostNodes(t *testing.T) {
	ex := &scriptedExec{nodes: "1001\n1002\n", slots: "repmgr_slot_1001|t|0|reserved|t\nrepmgr_slot_1002|t|0|reserved|t\n"}
	a := newFollowTestAgent(t, ex)
	a.cfg.SyncReplicationSlots = true
	a.cfg.NodeCount = 2 // live ordinals 0-1 -> node_id 1002 (ordinal 2) is a ghost
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), true)
	if want := "repmgr_slot_1001"; a.lastSyncStandbySlots == nil || *a.lastSyncStandbySlots != want {
		t.Errorf("lastSyncStandbySlots = %v, want %q (1002 excluded as a ghost)", a.lastSyncStandbySlots, want)
	}
}

// A standby registered a moment before its physical slot is created must not have a
// nonexistent slot named in synchronized_standby_slots -- that is the exact "blocks all
// logical decoding" failure this feature exists to prevent.
func TestAssertSyncStandbySlotsExcludesUnslottedStandby(t *testing.T) {
	ex := &scriptedExec{nodes: "1001\n1002\n", slots: "repmgr_slot_1001|t|0|reserved|t\n"} // 1002 has no slot yet
	a := newFollowTestAgent(t, ex)
	a.cfg.SyncReplicationSlots = true
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), true)
	if want := "repmgr_slot_1001"; a.lastSyncStandbySlots == nil || *a.lastSyncStandbySlots != want {
		t.Errorf("lastSyncStandbySlots = %v, want %q (1002 excluded, no slot yet)", a.lastSyncStandbySlots, want)
	}
}

// A freshly-promoted primary with no active standby slots yet must still issue the
// ALTER SYSTEM on its first reconcile (desired=="" is a real value, not a skip) --
// otherwise a synchronized_standby_slots value inherited from a prior primary term (or
// from being cloned from one) survives uncorrected, potentially naming a slot that no
// longer exists and permanently blocking logical decoding.
func TestAssertSyncStandbySlotsFirstReconcileIsUnconditionalEvenWhenEmpty(t *testing.T) {
	ex := &scriptedExec{}
	a := newFollowTestAgent(t, ex)
	a.cfg.SyncReplicationSlots = true
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), true)
	if len(ex.slotSyncSQL) != 2 {
		t.Fatalf("expected an unconditional first ALTER SYSTEM + reload even for an empty desired set, got %v", ex.slotSyncSQL)
	}
	if a.lastSyncStandbySlots == nil || *a.lastSyncStandbySlots != "" {
		t.Errorf("lastSyncStandbySlots = %v, want a non-nil pointer to \"\"", a.lastSyncStandbySlots)
	}
}

// act() must clear the cache whenever this node stops serving as primary, so the next
// primary term (this node re-promoted, or another node) starts with an unconditional
// first reconcile rather than a stale cache.
func TestActClearsLastSyncStandbySlotsWhenNotPrimary(t *testing.T) {
	ex := &scriptedExec{nodes: "1001\n", slots: "repmgr_slot_1001|t|0|reserved|t\n"}
	a := newFollowTestAgent(t, ex)
	a.cfg.SyncReplicationSlots = true
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), true)
	if a.lastSyncStandbySlots == nil {
		t.Fatal("expected a cached value after the first reconcile")
	}
	if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.Follow}, reconcile.Observation{}); err != nil {
		t.Fatalf("act: %v", err)
	}
	if a.lastSyncStandbySlots != nil {
		t.Errorf("lastSyncStandbySlots = %v, want nil after a non-primary action", a.lastSyncStandbySlots)
	}
}

// A steady-state tick with an unchanged standby set must skip the ALTER SYSTEM +
// reload entirely, not repeat it every 5s.
func TestAssertSyncStandbySlotsSkipsWhenUnchanged(t *testing.T) {
	ex := &scriptedExec{nodes: "1001\n", slots: "repmgr_slot_1001|t|0|reserved|t\n"}
	a := newFollowTestAgent(t, ex)
	a.cfg.SyncReplicationSlots = true
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), true)
	if len(ex.slotSyncSQL) != 2 {
		t.Fatalf("first call: expected 2 SQL statements, got %v", ex.slotSyncSQL)
	}
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), true)
	if len(ex.slotSyncSQL) != 2 {
		t.Fatalf("second call with an unchanged slot set must not issue more SQL, got %v", ex.slotSyncSQL)
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
	// Answer the post-initdb timeline read (#288 review, round 3): the marker write now POLLS
	// for the postmaster to accept SQL, because sup.Start is fire-and-forget. A fake that never
	// answers would make every one of these tests sit out the whole budget.
	if name == "psql" && strings.Contains(strings.Join(args, " "), "pg_walfile_name") {
		return "00000001|0/3000000", nil
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
	if len(ex.calls) == 0 || ex.calls[0][0] != entrypointPath || ex.calls[0][1] != "initdb" {
		t.Fatalf("want %s initdb as the FIRST call, got %v", entrypointPath, ex.calls)
	}
	if !pm.started {
		t.Error("the cluster was created but never started")
	}
	// #288 review (second pass): the highwater marker must be written HERE, not deferred to the
	// next StayPrimary tick. If the agent dies in that window the lease expires, a peer sees
	// empty PGDATA with no marker and no reachable primary, and takes BootstrapInitdb -- a
	// SECOND cluster with its own system_identifier, after which assertSameCluster refuses every
	// rejoin on both pods and only a PVC delete recovers.
	sawMarkerRead := false
	for _, c := range ex.calls {
		for _, arg := range c {
			if strings.Contains(arg, "pg_walfile_name") {
				sawMarkerRead = true
			}
		}
	}
	if !sawMarkerRead {
		t.Errorf("expected the timeline read that feeds the highwater marker write, got %v", ex.calls)
	}
	// And the write itself must land (#288 review, round 3). Asserting only the READ passed even
	// when the read failed and advanceMarker never ran -- which is exactly what happened while
	// this path made a single psql attempt against a postmaster that had just been exec'd.
	if mk, err := a.kube.ReadMarker(context.Background(), a.cfg.MarkerName); err != nil || !mk.Present {
		t.Errorf("the highwater marker was not written before returning (err=%v marker=%+v)", err, mk)
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
			Namespace: "ns", MarkerName: "pg-primary",
		},
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		dcs:  &fakeDCS{leader: true},
		mech: m,
		// A real k8s client over a fake apiserver, so the highwater marker write this path makes
		// is observable rather than swallowed by a nil kube.
		kube:   k8s.NewWithClient(k8sfake.NewSimpleClientset(), "ns"),
		prober: &pg.Prober{Exec: ex, Timeout: time.Second},
		sup:    process.NewSupervisor(pm),
		metr:   observe.New(),
		// Tests must not sit out the production budget when a fake never starts a postmaster.
		initdbMarkerWaitOverride: 200 * time.Millisecond,
	}
}

// #288: no repmgr.nodes query may run under the native mechanism. A native cluster has no
// repmgr extension at all now, so the read could only ever fail -- it was warning on every
// promote-candidate tick about a permanent condition. The #297 gate it fed is repmgr-specific
// (it guards against promoting a node no survivor can `repmgr standby follow`, which is a
// node_id-resolution constraint); native follows by conninfo, so nothing replaces it.
func TestObserveSkipsTheRegistryReadUnderNative(t *testing.T) {
	for _, tc := range []struct {
		mech        string
		wantQueries int
	}{
		{config.MechanismNative, 0},
		{config.MechanismRepmgr, 1},
	} {
		ex := &scriptedExec{nodes: "1000\n1001\n", walRcv: "pg-0.h|streaming"}
		a := newFollowTestAgent(t, ex)
		a.cfg.Mechanism = tc.mech
		a.cfg.PodName = "pg-1"
		// The one tick-state where the gate can fire: holder, running, in recovery.
		o := reconcile.Observation{HoldLease: true}
		o.Local = reconcile.LocalState{Running: true, InRecovery: true, HasData: true}
		a.readRegistryForGate(context.Background(), &o)
		if ex.nodesQueries != tc.wantQueries {
			t.Errorf("mechanism %q: %d repmgr.nodes queries, want %d", tc.mech, ex.nodesQueries, tc.wantQueries)
		}
		if tc.mech == config.MechanismNative && o.RegistryRead {
			t.Errorf("native mode set RegistryRead, which would arm the #297 gate")
		}
	}
}

// #288: identity resolution. application_name first (native writes the pod name there, repmgr
// writes node_name, same string); the ordinal-named slot as fallback for any standby cloned
// before #288, which still dials with libpq's default. Both shapes were verified streaming to
// one real PostgreSQL 18 primary at once.
func TestResolveReplicaPodUsesAppNameThenSlot(t *testing.T) {
	a := &agent{base: "pg"}
	for _, tc := range []struct {
		row  pg.ReplicaRow
		want string
		why  string
	}{
		{pg.ReplicaRow{AppName: "pg-1", SlotName: "pg_ha_slot_1"}, "pg-1", "application_name wins"},
		{pg.ReplicaRow{AppName: "walreceiver", SlotName: "pg_ha_slot_2"}, "pg-2", "libpq default falls back to the slot"},
		{pg.ReplicaRow{AppName: "", SlotName: "pg_ha_slot_3"}, "pg-3", "empty app name falls back to the slot"},
		{pg.ReplicaRow{AppName: "walreceiver", SlotName: "repmgr_slot_1004"}, "pg-4", "a legacy repmgr slot still resolves (mid-migration)"},
		{pg.ReplicaRow{AppName: "walreceiver", SlotName: ""}, "", "slotless and unnamed: unidentifiable"},
		{pg.ReplicaRow{AppName: "walreceiver", SlotName: "someone_elses_slot"}, "", "not a slot this agent mints"},
	} {
		if got := a.resolveReplicaPod(tc.row); got != tc.want {
			t.Errorf("%s: resolveReplicaPod(%+v) = %q, want %q", tc.why, tc.row, got, tc.want)
		}
	}
}

// The gauges must count what the primary can actually see, and must report separately when a
// streaming replica cannot be identified at all -- otherwise streaming-vs-expected alone would
// read as healthy while the topology view is incomplete.
func TestTopologyTickPublishesGaugesUnderBothMechanisms(t *testing.T) {
	// Two streaming rows (the catchup one is not counted), one of them unidentifiable.
	const rows = "pg-1|pg_ha_slot_1|streaming\nwalreceiver||streaming\npg-9|pg_ha_slot_9|catchup\n"

	// Native publishes the full picture: the expected/gap half needs the live pod set, and
	// reconcileSlots is already making that apiserver LIST on this path.
	ex := &slotExec{rows: rows}
	a := newSlotTestAgent(t, ex, config.MechanismNative)
	a.base = "pg"
	a.topologyTick(context.Background())
	body := scrapeMetrics(t, a)
	for _, want := range []string{
		"pg_ha_agent_replicas_streaming 2",
		"pg_ha_agent_replicas_unidentified 1",
		// The fake apiserver holds pods 0 and 1; self is pg-0, so one peer is expected.
		"pg_ha_agent_replicas_expected 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("native: missing %q in:\n%s", want, body)
		}
	}

	// Under repmgr only what the primary can see on its own is published. Charging every
	// existing install a SECOND uncached pod LIST per tick for an observational gauge is not a
	// trade worth making (#288 review) -- reconcileSlots returns before its own livePodOrdinals
	// on that path, so the LIST would be new cost.
	ex = &slotExec{rows: rows}
	a = newSlotTestAgent(t, ex, config.MechanismRepmgr)
	a.base = "pg"
	a.topologyTick(context.Background())
	body = scrapeMetrics(t, a)
	if !strings.Contains(body, "pg_ha_agent_replicas_streaming 2") {
		t.Errorf("repmgr: the streaming count must still be published:\n%s", body)
	}
	if !strings.Contains(body, "pg_ha_agent_replicas_expected 0") {
		t.Errorf("repmgr: expected must stay 0 (no extra pod LIST on the default path):\n%s", body)
	}
}

// A catchup replica exists but cannot serve or be promoted safely, so it must not be counted
// as streaming -- the distinction is the whole reason ReplicaRow carries State.
func TestTopologyTickIgnoresCatchupReplicas(t *testing.T) {
	ex := &slotExec{rows: "pg-1|pg_ha_slot_1|catchup\n"}
	a := newSlotTestAgent(t, ex, config.MechanismNative)
	a.base = "pg"
	a.topologyTick(context.Background())
	if body := scrapeMetrics(t, a); !strings.Contains(body, "pg_ha_agent_replicas_streaming 0") {
		t.Errorf("a catchup replica counted as streaming:\n%s", body)
	}
}

// The missing-peer warning is logged once per CHANGE, not once per tick: a rolling restart
// legitimately parks a peer off-stream, and a 5s warning loop would bury everything else.
func TestTopologyTickLogsTheGapOnlyOnChange(t *testing.T) {
	var out bytes.Buffer
	ex := &slotExec{rows: ""} // nothing streaming; the fake apiserver has a live peer pg-1
	a := newSlotTestAgent(t, ex, config.MechanismNative)
	a.base = "pg"
	a.log = slog.New(slog.NewTextHandler(&out, nil))
	a.topologyTick(context.Background())
	a.topologyTick(context.Background())
	if n := strings.Count(out.String(), "live peers are not streaming"); n != 1 {
		t.Errorf("gap warning logged %d times across two unchanged ticks, want 1:\n%s", n, out.String())
	}
	// And it must report recovery, so a resolved gap does not look like a silent stall.
	ex.rows = "pg-1|pg_ha_slot_1|streaming\n"
	a.topologyTick(context.Background())
	if !strings.Contains(out.String(), "replication topology complete") {
		t.Errorf("recovery from a topology gap was not logged:\n%s", out.String())
	}
}

// #288 audit: no native code path may carry a repmgr node_id. The offset itself cannot be
// deleted (slotOrdinal's legacy branch needs it to reclaim repmgr_slot_<node_id> orphans during
// a repmgr->native migration), so the tightening is to stop propagating ids instead.
func TestNativePathsCarryNoRepmgrNodeID(t *testing.T) {
	a := &agent{cfg: &config.Config{PodName: "pg-3", Mechanism: config.MechanismNative}}
	if got := a.repmgrNodeID(); got != 0 {
		t.Errorf("native self node_id = %d, want 0", got)
	}
	if got := a.repmgrPeerNodeID("pg-5"); got != 0 {
		t.Errorf("native peer node_id = %d, want 0", got)
	}
	a.cfg.Mechanism = config.MechanismRepmgr
	if got := a.repmgrNodeID(); got != 1003 {
		t.Errorf("repmgr self node_id = %d, want 1003", got)
	}
	if got := a.repmgrPeerNodeID("pg-5"); got != 1005 {
		t.Errorf("repmgr peer node_id = %d, want 1005", got)
	}
	// The offset must still be reversible for legacy slot reclaim -- #294 must not delete it.
	if ord, ok := slotOrdinal("repmgr_slot_1002"); !ok || ord != 2 {
		t.Errorf("legacy slot reclaim broken: slotOrdinal(repmgr_slot_1002) = (%d,%v)", ord, ok)
	}
}

// #288 review: the initdb exec is deliberately not fence-bounded, so the lease can flip during
// it -- and OnLost blocks on opMu until act() returns. Starting anyway would give the cluster
// two nodes that each initdb'd their own data with different system_identifiers, which
// assertSameCluster then refuses to rejoin. Exactly what this branch exists to prevent.
func TestBootstrapInitdbNativeDoesNotStartAfterLosingTheLease(t *testing.T) {
	ex := &initdbExec{}
	pm := &fakePostmaster{}
	a := newBootstrapTestAgentWithPM(t, ex, config.MechanismNative, pm)
	a.dcs = &fakeDCS{leader: false} // the lease flipped while initdb ran
	if err := a.act(context.Background(),
		reconcile.Decision{Action: reconcile.BootstrapInitdb},
		reconcile.Observation{}); err != nil {
		t.Fatalf("act: %v", err)
	}
	if len(ex.calls) != 1 {
		t.Fatalf("want the initdb attempt recorded, got %v", ex.calls)
	}
	if pm.started {
		t.Error("started read-write after losing the lease: a second cluster")
	}
	if a.servingRW.Load() {
		t.Error("armed servingRW after losing the lease")
	}
}

// Refusing to start is necessary but not sufficient: the data directory is initialized and no
// highwater marker exists yet, so this pod would sit HasData=true forever -- never eligible for
// BootstrapClone again, and rejected by assertSameCluster on every rejoin because the new holder
// created its own cluster with a different system_identifier. Only a PVC delete would recover
// it. The directory has never served a client, so it must be discarded (#288 review).
func TestBootstrapInitdbNativeDiscardsTheDataDirWhenTheLeaseIsLost(t *testing.T) {
	ex := &initdbExec{}
	pm := &fakePostmaster{}
	a := newBootstrapTestAgentWithPM(t, ex, config.MechanismNative, pm)
	a.dcs = &fakeDCS{leader: false}
	// Stand in for what a real initdb leaves behind; WipeDataDir keys on PG_VERSION.
	if err := os.WriteFile(filepath.Join(a.cfg.PGDATA, "PG_VERSION"), []byte("18\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.act(context.Background(),
		reconcile.Decision{Action: reconcile.BootstrapInitdb},
		reconcile.Observation{}); err != nil {
		t.Fatalf("act: %v", err)
	}
	if process.HasData(a.cfg.PGDATA) {
		t.Error("the orphaned cluster was left on disk: this pod can now never clone or rejoin")
	}
	if pm.started {
		t.Error("started read-write after losing the lease")
	}
}

// #288 review: a clone in flight opens its own replication connection (pg_basebackup -X
// stream). Counting it inflated the replica count, and taking its application_name at face
// value hid the pod the slot would have identified.
func TestTopologyIgnoresTheBaseBackupConnection(t *testing.T) {
	ex := &slotExec{rows: "pg_basebackup|pg_ha_slot_1|streaming\n"}
	a := newSlotTestAgent(t, ex, config.MechanismNative)
	a.base = "pg"
	a.topologyTick(context.Background())
	body := scrapeMetrics(t, a)
	if !strings.Contains(body, "pg_ha_agent_replicas_streaming 0") {
		t.Errorf("a pg_basebackup connection counted as a streaming replica:\n%s", body)
	}
	if !strings.Contains(body, "pg_ha_agent_replicas_unidentified 0") {
		t.Errorf("a skipped clone connection was also counted as unidentified:\n%s", body)
	}
	// And an application_name that is not a pod of this StatefulSet must fall through to the
	// slot rather than being trusted.
	if got := a.resolveReplicaPod(pg.ReplicaRow{AppName: "pg_basebackup", SlotName: "pg_ha_slot_1"}); got != "pg-1" {
		t.Errorf("resolveReplicaPod trusted a non-pod application_name: got %q, want pg-1", got)
	}
	if got := a.resolveReplicaPod(pg.ReplicaRow{AppName: "some-other-sts-3", SlotName: ""}); got != "" {
		t.Errorf("resolveReplicaPod accepted a pod name from another StatefulSet: %q", got)
	}
}

// #288 review: .pgpass must be attempted BEFORE the managed config. It writes to the postgres
// user's home, so it is the one boot step that can succeed on a fresh native install where
// PGDATA does not exist yet -- and with the old ordering native's GenerateConfig failed first
// and returned, so the credential was never written. A standby then cloned fine and sat
// Running-but-NotReady forever, its walreceiver failing `fe_sendauth: no password supplied`.
//
// Asserted through the ERROR IDENTITY: with an absent PGDATA both steps fail in this
// environment, so whichever one boot reports is the one it ran first.
func TestBootAttemptsPgpassBeforeConfigGeneration(t *testing.T) {
	ex := &scriptedExec{}
	a := newFollowTestAgent(t, ex)
	a.cfg.Mechanism = config.MechanismNative
	a.cfg.PGDATA = filepath.Join(t.TempDir(), "absent")
	a.mech = mechanism.NewNative(a.cfg.PGDATA, "/usr/lib/postgresql/18/bin", "pw", "pg_ha_slot_0", "pg-0")
	err := a.boot(context.Background())
	if err == nil {
		t.Skip("this environment can write the fixed .pgpass path; the ordering assertion needs it to fail")
	}
	if !strings.Contains(err.Error(), "pgpass") {
		t.Errorf("boot reported %q first; .pgpass must be attempted before the managed config, which cannot be written on an absent PGDATA", err)
	}
}

// And a GenerateConfig failure on an absent PGDATA must not abort boot: the clone and initdb
// paths both regenerate the fragment once the directory exists.
func TestBootToleratesConfigGenerationFailureOnAnAbsentDataDir(t *testing.T) {
	a := newFollowTestAgent(t, &scriptedExec{})
	a.cfg.Mechanism = config.MechanismNative
	a.cfg.PGDATA = filepath.Join(t.TempDir(), "absent")
	m := mechanism.NewNative(a.cfg.PGDATA, "/usr/lib/postgresql/18/bin", "pw", "pg_ha_slot_0", "pg-0")
	// GenerateConfig on an absent dir fails; assert the tolerance directly rather than through
	// boot(), whose fixed .pgpass path is not writable in a sandbox.
	if err := m.GenerateConfig(context.Background(), mechanism.NodeIdentity{DataDir: a.cfg.PGDATA}, mechanism.ConfigOpts{}); err == nil {
		t.Skip("GenerateConfig unexpectedly succeeded on an absent data directory")
	}
	if process.HasData(a.cfg.PGDATA) {
		t.Fatal("test setup: the data directory must be absent")
	}
}

// #288 review: Follow keeps the SLOT gauges (standbySlotsTick re-publishes them from a
// standby's own point of view) but must RETRACT the topology ones -- topologyTick reads the
// PRIMARY's connection list and has no standby equivalent, so a demoted primary would keep
// exporting its last view for the rest of its process lifetime. That is the max()-across-the-
// release latching ClearTopology exists to prevent.
func TestFollowRetractsTopologyButKeepsSlotGauges(t *testing.T) {
	m := observe.New()
	m.SetTopology(observe.TopologyStats{Streaming: 2, Expected: 2})
	m.SetSlots(observe.SlotStats{Total: 3, MaxRetainedWALBytes: 4096})
	// Mirror act()'s retract switch for the Follow case.
	m.ClearTopology()
	rec := httptest.NewRecorder()
	m.Handler(time.Minute).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "pg_ha_agent_replicas_expected 0") {
		t.Errorf("topology gauges survived a Follow, so a demoted primary latches them:\n%s", body)
	}
	if !strings.Contains(body, "pg_ha_agent_replication_slot_max_retained_wal_bytes 4096") {
		t.Errorf("slot gauges were retracted on Follow; a standby pinning WAL must stay visible:\n%s", body)
	}
}

// #288 review: an interrupted base backup leaves PG_VERSION behind, so HasData is true and
// Decide never returns BootstrapClone again -- the pod tries to rejoin a torn directory every
// tick forever, recoverable only by deleting the PVC. Under repmgr this could not happen:
// init-repmgr.sh wiped PGDATA before each clone attempt. Moving the clone into the agent means
// the agent owns that discard.
func TestDiscardTornCloneRearmsBootstrapClone(t *testing.T) {
	root := t.TempDir()
	pgdata := filepath.Join(root, "pgdata")
	if err := os.MkdirAll(pgdata, 0o700); err != nil {
		t.Fatal(err)
	}
	// What an interrupted pg_basebackup leaves: PG_VERSION written, nothing else usable.
	if err := os.WriteFile(filepath.Join(pgdata, "PG_VERSION"), []byte("18\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &agent{
		cfg: &config.Config{PGDATA: pgdata, PodName: "pg-1"},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	// The marker must live OUTSIDE PGDATA: pg_basebackup requires an empty target, so anything
	// inside would be destroyed by the operation it tracks.
	if dir := filepath.Dir(a.cloneMarkerPath()); dir == pgdata {
		t.Fatalf("the clone marker is inside PGDATA (%s); the clone would destroy it", dir)
	}
	a.beginClone()
	if process.HasData(pgdata) != true {
		t.Fatal("test setup: PG_VERSION should make HasData true")
	}
	a.discardTornClone(context.Background())
	if process.HasData(pgdata) {
		t.Error("the torn directory survived, so BootstrapClone stays disarmed and the pod is stuck")
	}
	if _, err := os.Stat(a.cloneMarkerPath()); !os.IsNotExist(err) {
		t.Error("the clone marker survived the discard")
	}
}

// A completed clone must leave no marker, or the next boot would discard a healthy standby.
func TestEndCloneClearsTheMarker(t *testing.T) {
	root := t.TempDir()
	pgdata := filepath.Join(root, "pgdata")
	if err := os.MkdirAll(pgdata, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pgdata, "PG_VERSION"), []byte("18\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &agent{
		cfg: &config.Config{PGDATA: pgdata, PodName: "pg-1"},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	a.beginClone()
	a.endClone()
	a.discardTornClone(context.Background())
	if !process.HasData(pgdata) {
		t.Error("a successfully cloned data directory was discarded on the next boot")
	}
}

// #288 review: WipeDataDir removes PGDATA's ENTRIES and leaves the directory, so after a
// torn-clone discard the path exists and is empty -- and native's writeManagedConf then succeeds,
// leaving one file behind. pg_basebackup takes no flag permitting a populated target and refuses
// with `directory "..." exists but is not empty` (verified against PostgreSQL 18), so the standby
// would be wedged for good: the exact failure the clone marker exists to prevent, reached through
// its own recovery path. boot() must write NOTHING into an empty data directory.
func TestBootWritesNothingIntoAnEmptyDataDir(t *testing.T) {
	a := newFollowTestAgent(t, &scriptedExec{})
	a.cfg.Mechanism = config.MechanismNative
	pgdata := filepath.Join(t.TempDir(), "pgdata")
	if err := os.MkdirAll(pgdata, 0o700); err != nil { // exists and empty: post-wipe shape
		t.Fatal(err)
	}
	a.cfg.PGDATA = pgdata
	a.mech = mechanism.NewNative(pgdata, "/usr/lib/postgresql/18/bin", "pw", "pg_ha_slot_1", "pg-1")
	_ = a.boot(context.Background()) // .pgpass may fail in a sandbox; the assertion is below
	entries, err := os.ReadDir(pgdata)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("boot left %v in an empty data directory; pg_basebackup will refuse the target forever", names)
	}
}

// #288 review: bootstrap_initdb runs initdb BEFORE creating the repmgr role and database, so an
// exec failure after that point leaves PGDATA initialized with no role -- and bootstrap_initdb
// no-ops on a populated directory forever, so the node comes up as a primary the agent can never
// authenticate against and no standby can clone from. That path needs the same cleanup as the
// ones after it.
func TestBootstrapInitdbNativeDiscardsAPartialDataDirOnExecFailure(t *testing.T) {
	ex := &initdbExec{err: errors.New("exit status 1")}
	a := newBootstrapTestAgentWithPM(t, ex, config.MechanismNative, &fakePostmaster{})
	// What a failed bootstrap_initdb leaves: initdb ran, the roles never got created.
	if err := os.WriteFile(filepath.Join(a.cfg.PGDATA, "PG_VERSION"), []byte("18\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.act(context.Background(),
		reconcile.Decision{Action: reconcile.BootstrapInitdb},
		reconcile.Observation{}); err == nil {
		t.Fatal("a failed initdb reported success")
	}
	if process.HasData(a.cfg.PGDATA) {
		t.Error("the partial cluster was left behind: bootstrap_initdb will no-op on it forever")
	}
}

// #288 review: the stall signal needs hysteresis. A walreceiver reconnect, or an upstream that is
// briefly down, looks identical to a permanent divergence for a tick or two -- and escalating on
// that would re-clone a healthy standby.
func TestObserveStandbyStallRequiresPersistence(t *testing.T) {
	ex := &scriptedExec{walRcv: ""} // running standby, no walreceiver row
	a := newFollowTestAgent(t, ex)
	// A SETTLED standby: the repoint has completed at least once. Without the latch the
	// escalation must not fire at all -- see the sibling test below.
	a.followUpstream = "pg-0"
	base := reconcile.Observation{Local: reconcile.LocalState{Running: true, InRecovery: true, HasData: true}}

	for i := 1; i < standbyStallTicks; i++ {
		o := base
		a.observeStandbyStall(context.Background(), &o)
		if o.StandbyStalled {
			t.Fatalf("stalled after only %d tick(s); want at least %d", i, standbyStallTicks)
		}
	}
	o := base
	a.observeStandbyStall(context.Background(), &o)
	if !o.StandbyStalled {
		t.Errorf("not stalled after %d receiver-less ticks", standbyStallTicks)
	}

	// A single streaming tick must reset it: the standby recovered on its own.
	ex.walRcv = "pg-0.h|streaming"
	o = base
	a.observeStandbyStall(context.Background(), &o)
	if o.StandbyStalled || a.standbyNoReceiverTicks != 0 {
		t.Errorf("streaming did not clear the stall latch (stalled=%v ticks=%d)", o.StandbyStalled, a.standbyNoReceiverTicks)
	}
}

// Ceasing to be a running standby (promoted, stopped, demoted) must reset the latch, or a
// later spell as a standby would inherit a stale count and escalate early.
func TestObserveStandbyStallResetsWhenNotARunningStandby(t *testing.T) {
	ex := &scriptedExec{walRcv: ""}
	a := newFollowTestAgent(t, ex)
	for i := 0; i < standbyStallTicks; i++ {
		o := reconcile.Observation{Local: reconcile.LocalState{Running: true, InRecovery: true, HasData: true}}
		a.observeStandbyStall(context.Background(), &o)
	}
	// Now a primary.
	o := reconcile.Observation{Local: reconcile.LocalState{Running: true, InRecovery: false, HasData: true}}
	a.observeStandbyStall(context.Background(), &o)
	if a.standbyNoReceiverTicks != 0 || o.StandbyStalled {
		t.Errorf("latch survived the role change (ticks=%d stalled=%v)", a.standbyNoReceiverTicks, o.StandbyStalled)
	}
}

// An unreadable probe is not evidence of a stall: hold the counter rather than advancing it, so a
// psql blip cannot march a healthy standby toward a re-clone.
func TestObserveStandbyStallHoldsOnAProbeError(t *testing.T) {
	a := newFollowTestAgent(t, &scriptedExec{walRcv: ""})
	for i := 0; i < 3; i++ {
		o := reconcile.Observation{Local: reconcile.LocalState{Running: true, InRecovery: true, HasData: true}}
		a.observeStandbyStall(context.Background(), &o)
	}
	before := a.standbyNoReceiverTicks
	// Swap in a prober whose psql fails.
	a.prober = &pg.Prober{Exec: &failingExec{}, Timeout: time.Second}
	o := reconcile.Observation{Local: reconcile.LocalState{Running: true, InRecovery: true, HasData: true}}
	a.observeStandbyStall(context.Background(), &o)
	if a.standbyNoReceiverTicks != before {
		t.Errorf("a probe error advanced the stall counter: %d -> %d", before, a.standbyNoReceiverTicks)
	}
}

type failingExec struct{}

func (failingExec) Run(context.Context, []string, string, ...string) (string, error) {
	return "", errors.New("exit status 2")
}

// #288 review (second pass): following a peer must NOT expire the restore claim.
//
// The first attempt dropped it here, which is a race: under native, Follow only writes
// primary_conninfo + standby.signal and reloads -- no rewind -- so a DIVERGED standby reaches it
// too. A PITR lands on pgbackrest.restore.podOrdinal, which need not be the lease holder; that
// pod comes up read-write, takes DemoteFence then Follow, and would drop its claim ~10s after
// boot, racing the holder's tick which needs that record to decide to release the lease to it.
// Expiry belongs on ADOPTION (promote + marker advance) instead.
func TestLatchFollowPreservesTheRestoreClaim(t *testing.T) {
	dir := t.TempDir()
	pgdata := filepath.Join(dir, "pgdata")
	if err := os.MkdirAll(pgdata, 0o700); err != nil {
		t.Fatal(err)
	}
	a := &agent{
		cfg: &config.Config{PGDATA: pgdata},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	rec := a.cfg.RestoreStatusPath()
	if err := os.WriteFile(rec, []byte("exitCode=0\nrestoredAt=2026-08-24T12:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.latchFollow("pg-1")
	if a.followUpstream != "pg-1" {
		t.Errorf("follow latch not set: %q", a.followUpstream)
	}
	if _, err := os.Stat(rec); err != nil {
		t.Errorf("the restore claim must SURVIVE a follow, or the holder's release-to-restored-node tick races it: %v", err)
	}
}

// dropRestoreRecord is for volumes whose CONTENTS stopped matching the record -- a clone, a
// rewind, a wipe. Adoption no longer comes through here (it stamps adoptedAt instead, see
// TestAdoptRestoreKeepsTheRecordAndExpiresTheClaim): expiring the claim by unlinking the file
// destroyed the provenance GET /v1/status reports, permanently, on the restore that succeeded.
func TestDropRestoreRecordRemovesTheClaim(t *testing.T) {
	dir := t.TempDir()
	pgdata := filepath.Join(dir, "pgdata")
	if err := os.MkdirAll(pgdata, 0o700); err != nil {
		t.Fatal(err)
	}
	a := &agent{
		cfg: &config.Config{PGDATA: pgdata},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	rec := a.cfg.RestoreStatusPath()
	if err := os.WriteFile(rec, []byte("exitCode=0\nrestoredAt=2026-08-24T12:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.dropRestoreRecord("adopted")
	if _, err := os.Stat(rec); !os.IsNotExist(err) {
		t.Errorf("the claim must be gone after adoption (stat err=%v)", err)
	}
}

// And it must be silent/harmless on the overwhelmingly common case: an ordinary standby that was
// never restored has no record to drop.
func TestLatchFollowWithoutARecordIsHarmless(t *testing.T) {
	dir := t.TempDir()
	pgdata := filepath.Join(dir, "pgdata")
	if err := os.MkdirAll(pgdata, 0o700); err != nil {
		t.Fatal(err)
	}
	a := &agent{
		cfg: &config.Config{PGDATA: pgdata},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	a.latchFollow("pg-1")
	if a.followUpstream != "pg-1" {
		t.Errorf("follow latch not set: %q", a.followUpstream)
	}
}

// #288 review (second pass): a standby that has NOT settled on an upstream must never escalate.
// A freshly repointed standby is indistinguishable from a diverged one -- Follow writes
// primary_conninfo and reloads, and the walreceiver can legitimately take tens of seconds to
// attach (the new primary may not have created the slot yet, sender slots can be exhausted, DNS
// may be settling). Escalating there stops, rewinds and possibly RE-CLONES a healthy standby.
func TestObserveStandbyStallDoesNotFireBeforeTheFollowLatchIsSet(t *testing.T) {
	ex := &scriptedExec{walRcv: ""} // running standby, no walreceiver row
	a := newFollowTestAgent(t, ex)
	a.followUpstream = "" // not settled on any upstream yet
	base := reconcile.Observation{Local: reconcile.LocalState{Running: true, InRecovery: true, HasData: true}}

	for i := 0; i < standbyStallTicks*3; i++ {
		o := base
		a.observeStandbyStall(context.Background(), &o)
		if o.StandbyStalled {
			t.Fatalf("escalated on tick %d with no follow latch: a repointing standby would be rewound", i+1)
		}
	}
}

// stallExec answers the two queries observeStandbyStall makes: the walreceiver lookup
// (StreamingUpstream) and the position read (StandbyReceiveLSN). recvLSN is what the position
// query returns, so a test can hold it still or advance it between ticks.
type stallExec struct {
	streaming bool
	recvLSN   string
}

func (s *stallExec) Run(_ context.Context, _ []string, name string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	switch {
	case name == "psql" && strings.Contains(joined, "pg_stat_wal_receiver"):
		if s.streaming {
			return "up-0|streaming", nil
		}
		return "", nil // no walreceiver row at all -- archive recovery, or genuinely stalled
	case name == "psql" && strings.Contains(joined, "pg_last_wal_receive_lsn"):
		return s.recvLSN, nil
	}
	return "", nil
}

func newStallTestAgent(t *testing.T, ex *stallExec) *agent {
	t.Helper()
	a := newSlotTestAgent(t, nil, config.MechanismNative)
	a.prober = &pg.Prober{Exec: ex, Timeout: time.Second}
	a.followUpstream = "pg-1" // pointed at an upstream, the precondition for a stall
	return a
}

// A standby replaying from restore_command (pgbackrest archive-get) has NO pg_stat_wal_receiver
// row for as long as it works through archived WAL. Counting those ticks as a stall made Decide
// escalate to RejoinForward -- stopping, rewinding or re-cloning a node that was catching up
// correctly. Forward progress in the replay position is what tells the two apart (#288 review).
func TestObserveStandbyStallIgnoresArchiveCatchUp(t *testing.T) {
	ex := &stallExec{streaming: false, recvLSN: "0/3000000"}
	a := newStallTestAgent(t, ex)
	standby := reconcile.LocalState{Running: true, InRecovery: true}

	for i := 0; i < standbyStallTicks*2; i++ {
		o := reconcile.Observation{Local: standby}
		a.observeStandbyStall(context.Background(), &o)
		if o.StandbyStalled {
			t.Fatalf("tick %d: a standby making replay progress must never be reported stalled", i)
		}
		// Each tick replays a little more WAL, exactly as archive recovery does.
		ex.recvLSN = fmt.Sprintf("0/%d000000", 4+i)
	}
}

// The flip side: no walreceiver AND no forward progress is a genuinely wedged standby, which
// must still earn StandbyStalled after stallTicks so a diverged node can be rejoined.
func TestObserveStandbyStallStillFiresWhenNothingMoves(t *testing.T) {
	ex := &stallExec{streaming: false, recvLSN: "0/3000000"}
	a := newStallTestAgent(t, ex)
	standby := reconcile.LocalState{Running: true, InRecovery: true}

	var last reconcile.Observation
	for i := 0; i < standbyStallTicks+1; i++ {
		last = reconcile.Observation{Local: standby}
		a.observeStandbyStall(context.Background(), &last)
	}
	if !last.StandbyStalled {
		t.Fatalf("a standby with no walreceiver and no replay progress must be reported stalled after %d ticks", standbyStallTicks)
	}
}

// An observed streaming upstream clears both the counter and the progress watermark, so a later
// stall starts from scratch rather than comparing against a stale position.
func TestObserveStandbyStallResetsOnStreaming(t *testing.T) {
	ex := &stallExec{streaming: false, recvLSN: "0/3000000"}
	a := newStallTestAgent(t, ex)
	standby := reconcile.LocalState{Running: true, InRecovery: true}
	for i := 0; i < standbyStallTicks+1; i++ {
		o := reconcile.Observation{Local: standby}
		a.observeStandbyStall(context.Background(), &o)
	}
	ex.streaming = true
	o := reconcile.Observation{Local: standby}
	a.observeStandbyStall(context.Background(), &o)
	if o.StandbyStalled || a.standbyNoReceiverTicks != 0 || a.standbyLastProgressLSN != (pg.LSN{}) {
		t.Fatalf("streaming must clear the stall state, got stalled=%v ticks=%d lsn=%v",
			o.StandbyStalled, a.standbyNoReceiverTicks, a.standbyLastProgressLSN)
	}
}

// noopExec answers every command with success and no output: enough for discardFreshDataDir's
// best-effort `pg_ctl stop` on a directory whose postmaster is long gone.
type noopExec struct{}

func (noopExec) Run(_ context.Context, _ []string, _ string, _ ...string) (string, error) {
	return "", nil
}

func newInitdbMarkerAgent(t *testing.T) *agent {
	t.Helper()
	root := t.TempDir()
	pgdata := filepath.Join(root, "pgdata")
	if err := os.MkdirAll(pgdata, 0o700); err != nil {
		t.Fatal(err)
	}
	// What a bootstrap that got as far as initdb leaves behind.
	if err := os.WriteFile(filepath.Join(pgdata, "PG_VERSION"), []byte("18\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &agent{
		cfg:    &config.Config{PGDATA: pgdata, PodName: "pg-0"},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		prober: &pg.Prober{Exec: noopExec{}, Timeout: time.Second},
	}
}

// #288 review: bootstrap_initdb starts a postmaster halfway through, which satisfies the chart's
// startupProbe and arms the liveness probe while act() still holds opMu and is not beating
// /healthz -- so the kubelet can SIGKILL the container mid-bootstrap. No error is returned for
// discardFreshDataDir to act on, and the result is unrecoverable by hand: PGDATA is initialized
// but carries no repmgr role or database, bootstrap_initdb no-ops on it forever, and the pod
// comes up as a primary the agent cannot authenticate against. Only a next-boot check recovers.
func TestDiscardTornInitdbWipesAnUnfinishedBootstrap(t *testing.T) {
	a := newInitdbMarkerAgent(t)
	if dir := filepath.Dir(a.initdbMarkerPath()); dir == a.cfg.PGDATA {
		t.Fatalf("the initdb marker is inside PGDATA (%s); initdb requires an empty target", dir)
	}
	a.beginInitdb()
	a.discardTornInitdb(context.Background())
	if process.HasData(a.cfg.PGDATA) {
		t.Error("a half-bootstrapped data directory survived, so the pod stays wedged")
	}
	if _, err := os.Stat(a.initdbMarkerPath()); !os.IsNotExist(err) {
		t.Error("the initdb marker survived the discard")
	}
}

// The marker alone must NEVER justify wiping: endInitdb only warns when its remove fails, so a
// stale marker is reachable over a bootstrap that DID finish -- and by the next boot that
// directory can be a serving primary. bootstrap_initdb's completion sentinel is the evidence.
func TestDiscardTornInitdbKeepsACompletedBootstrap(t *testing.T) {
	a := newInitdbMarkerAgent(t)
	a.beginInitdb()
	if err := os.WriteFile(a.bootstrapCompletePath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a.discardTornInitdb(context.Background())
	if !process.HasData(a.cfg.PGDATA) {
		t.Error("a completed bootstrap was discarded; that is data loss on a cluster that may already serve")
	}
	if _, err := os.Stat(a.initdbMarkerPath()); !os.IsNotExist(err) {
		t.Error("the stale marker survived, so a later boot would reconsider a healthy directory")
	}
}

// No marker means no bootstrap was ever in flight here -- e.g. a standby that cloned. Nothing
// may be touched, sentinel or not (a clone from a pre-sentinel cluster carries none).
func TestDiscardTornInitdbLeavesUnmarkedDirectoriesAlone(t *testing.T) {
	a := newInitdbMarkerAgent(t)
	a.discardTornInitdb(context.Background())
	if !process.HasData(a.cfg.PGDATA) {
		t.Error("a data directory with no in-progress marker was discarded")
	}
}

// A successful bootstrap must leave no marker behind, or the next boot inspects a healthy
// cluster for tornness.
func TestEndInitdbClearsTheMarker(t *testing.T) {
	a := newInitdbMarkerAgent(t)
	a.beginInitdb()
	a.endInitdb()
	if _, err := os.Stat(a.initdbMarkerPath()); !os.IsNotExist(err) {
		t.Fatal("endInitdb left the marker in place")
	}
	a.discardTornInitdb(context.Background())
	if !process.HasData(a.cfg.PGDATA) {
		t.Error("a bootstrapped data directory was discarded on the next boot")
	}
}

// newRestoreClaimAgent wires an agent whose pgbackrest client reads/writes a real record file.
func newRestoreClaimAgent(t *testing.T, body string) *agent {
	t.Helper()
	dir := t.TempDir()
	pgdata := filepath.Join(dir, "pgdata")
	if err := os.MkdirAll(pgdata, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{PGDATA: pgdata}
	if body != "" {
		if err := os.WriteFile(cfg.RestoreStatusPath(), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &agent{
		cfg:  cfg,
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		pgbr: pgbackrest.Client{StatusPath: cfg.RestoreStatusPath()},
	}
}

// #288 review, round 4: the documented restore procedure is scale to 0, restore into the target
// ordinal's PVC, scale up -- and pgbackrest runs --target-action=promote, so that pod comes back
// holding primary-state data and takes StartLocal, then StayPrimary. It never passes through
// Promote, so a stamp only on the Promote branch left the claim in force FOREVER on the most
// common restore path -- and a permanent claim vetoes every peer in moreAdvancedPeer, so this
// node would later promote (or be handed the lease) over a peer holding more WAL.
func TestAdoptRestoreKeepsTheRecordAndExpiresTheClaim(t *testing.T) {
	a := newRestoreClaimAgent(t, "exitCode=0\nrestoredAt=2026-08-24T12:00:00Z\nbackupSet=20260824-110000F\n")
	if got := a.localRestoredAt(); got != "2026-08-24T12:00:00Z" {
		t.Fatalf("test setup: the claim should stand before adoption, got %q", got)
	}
	a.adoptRestoreIfServing(reconcile.Observation{Local: reconcile.LocalState{RestoredAt: a.localRestoredAt()}})

	if got := a.localRestoredAt(); got != "" {
		t.Errorf("the election claim survived adoption (%q); a permanent claim outranks peers with more WAL", got)
	}
	rec, err := a.pgbr.LastRestore()
	if err != nil {
		t.Fatalf("LastRestore: %v", err)
	}
	if !rec.Present || rec.BackupSet != "20260824-110000F" || rec.AdoptedAt == "" {
		t.Errorf("provenance must survive adoption, stamped: %+v", rec)
	}
}

// No claim, no write: an ordinary primary tick on a never-restored volume must not touch the
// record path at all (this runs on every StayPrimary tick).
func TestAdoptRestoreIsANoopWithoutAClaim(t *testing.T) {
	a := newRestoreClaimAgent(t, "")
	a.adoptRestoreIfServing(reconcile.Observation{Local: reconcile.LocalState{}})
	if _, err := os.Stat(a.cfg.RestoreStatusPath()); !os.IsNotExist(err) {
		t.Errorf("a record was created for a volume that was never restored (stat err=%v)", err)
	}
}

// mustSlots reads the physical slot list the way a primary tick does, so the #308 tests exercise
// assertSyncStandbySlots with exactly what slotsTick would hand it (rebase review: the function
// takes the list instead of re-querying inside the fence budget).
func mustSlots(t *testing.T, a *agent) []pg.SlotState {
	t.Helper()
	slots, err := a.prober.PhysicalSlots(context.Background(), a.selfConn())
	if err != nil {
		t.Fatalf("PhysicalSlots: %v", err)
	}
	return slots
}
