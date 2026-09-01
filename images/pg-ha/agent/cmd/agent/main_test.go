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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"net/http/httptest"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cagriekin/pg-ha-agent/internal/config"
	"github.com/cagriekin/pg-ha-agent/internal/dcs"
	"github.com/cagriekin/pg-ha-agent/internal/k8s"
	"github.com/cagriekin/pg-ha-agent/internal/mechanism"
	"github.com/cagriekin/pg-ha-agent/internal/observe"
	"github.com/cagriekin/pg-ha-agent/internal/pg"
	"github.com/cagriekin/pg-ha-agent/internal/pgbackrest"
	"github.com/cagriekin/pg-ha-agent/internal/podname"
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
	// ONE temp dir shared by the mechanism and the config: Native writes its managed
	// fragment into PGDATA and reads postgresql.conf from it, so two directories would make
	// every Follow fail on a missing file. Seed the file the include-management reads --
	// a real PGDATA always has one (initdb writes it) -- and PG_VERSION, without which the
	// directory reads as pre-clone debris and BootstrapClone's clear empties it (#298 review).
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "postgresql.conf"), []byte("# test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("18\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := mechanism.NewNative(dir, "/usr/lib/postgresql/18/bin", "pw", "pg_ha_slot_0", "pg-0")
	m.Runner = ex
	return &agent{
		cfg: &config.Config{
			PGDATA:          dir,
			HeadlessService: "h",
			RepmgrUser:      "repmgr",
			RepmgrDB:        "repmgr",
			RepmgrPassword:  "pw",
			RenewDeadline:   2 * time.Second,
		},
		base:   "pg", // production sets this from podname.Base(cfg.PodName); the target guard needs it
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
// / newMechanism has one implementation since #294 deleted mechanism.Repmgr. The assertion
// worth keeping is that it is the native one -- the Mechanism INTERFACE survives (it is the
// seam a second implementation would use), so a future addition that forgot to wire the
// factory would otherwise only surface at runtime. Rejecting an unrecognised MECHANISM value
// is config.Load's job, and config's own tests cover it.
func TestNewMechanismBuildsTheNativeImplementation(t *testing.T) {
	cfg := &config.Config{PGDATA: t.TempDir(), PodName: "pg-0", Mechanism: config.MechanismNative}
	m := newMechanism(cfg, "/usr/lib/postgresql/18/bin")
	if got := fmt.Sprintf("%T", m); got != "*mechanism.Native" {
		t.Errorf("newMechanism built %s, want *mechanism.Native", got)
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

// A standby not yet streaming (or being repointed to a new upstream) must actually follow,
// then latch.
//
// Asserted on the OBSERVABLE EFFECT rather than a CLI call count (#294): the native mechanism
// writes primary_conninfo into its managed fragment and relies on the caller's reload to make
// the walreceiver pick it up, so "did a follow happen" is "does the config now point at the
// target, and was the postmaster told". Counting `repmgr standby follow` invocations was only
// ever meaningful for the deleted implementation.
func TestActFollowRunsWhenNotStreaming(t *testing.T) {
	ex := &scriptedExec{walRcv: ""} // no walreceiver row
	pm := &fakePostmaster{}
	a := newFollowTestAgentWithPM(t, ex, pm)
	dec := reconcile.Decision{Action: reconcile.Follow, Target: "pg-0"}
	if err := a.act(context.Background(), dec, reconcile.Observation{}); err != nil {
		t.Fatalf("act: %v", err)
	}
	assertFollowsTarget(t, a, "pg-0")
	if !pm.reloaded {
		t.Error("the postmaster must be reloaded, or the written conninfo never takes effect")
	}
	if a.followUpstream != "pg-0" {
		t.Fatal("followUpstream must latch after a successful follow")
	}
}

// assertFollowsTarget reads the agent-managed fragment in PGDATA and asserts primary_conninfo
// points at target. This is the file native mode writes; postgresql.auto.conf holds only what
// ALTER SYSTEM put there.
func assertFollowsTarget(t *testing.T, a *agent, target string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(a.cfg.PGDATA, "pg-ha-agent.conf"))
	if err != nil {
		t.Fatalf("the managed conf was never written: %v", err)
	}
	if !strings.Contains(string(b), a.fqdn(target)) {
		t.Errorf("primary_conninfo does not point at %s:\n%s", target, b)
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

// The follow latch is invalidated on ENTRY to rejoinOnto, so it is cleared even when the
// rejoin fails before reconfiguring anything (here: Demote fails). Clearing it late would
// leave a stale latch behind a failed rejoin. Nothing depends on that today -- the
// escalation is only reachable when the latch already differs from the target -- but the
// invariant "an attempt to rejoin invalidates the latch" is unconditional, and this pins it
// so a future edit that reads the latch earlier cannot silently rely on a stale value.
func TestRejoinOntoClearsFollowLatchEvenWhenItFailsEarly(t *testing.T) {
	// Driven through RejoinForward directly: it is the decision that reaches rejoinOnto, and the
	// invariant under test -- the latch is invalidated on ENTRY, not on success -- is unchanged.
	ex := &scriptedExec{walRcv: ""}
	pm := &fakePostmaster{stopErr: errors.New("stop failed")}
	a := newFollowTestAgentWithPM(t, ex, pm)
	a.followUpstream = "stale-upstream"
	dec := reconcile.Decision{Action: reconcile.RejoinForward, Target: "pg-1"}
	if err := a.act(context.Background(), dec, reconcile.Observation{}); err == nil {
		t.Fatal("a rejoin whose demote fails must surface an error")
	}
	if a.followUpstream != "" {
		t.Fatalf("the follow latch must be cleared on entry to a rejoin, got %q", a.followUpstream)
	}
}

// #298 review: a rejoin that SUCCEEDS must re-latch the upstream it just pointed replication
// at. The latch is invalidated on entry, and leaving it empty leaks a slot: both rejoin
// outcomes provision this node's slot on the target, so the next tick that re-homes the node
// calls releaseSlotOnFormerUpstream with an EMPTY former and returns at its guard -- an
// inactive slot then pins WAL on the rejoin target until max_slot_wal_keep_size invalidates it.
// It also re-arms observeStandbyStall, which requires a non-empty latch.
func TestRejoinOntoRelatchesTheFollowUpstreamOnSuccess(t *testing.T) {
	a := newFollowTestAgentWithPM(t, &scriptedExec{walRcv: ""}, &fakePostmaster{})
	a.mech = &rewindStubMech{} // an empty script: the rewind succeeds
	a.followUpstream = "stale-upstream"
	if err := a.rejoinOnto(context.Background(), "pg-1"); err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if a.followUpstream != "pg-1" {
		t.Fatalf("a successful rejoin must latch its target as the upstream, got %q", a.followUpstream)
	}
}

// #298 review: RestartLocal must not report success when it could not prove the postmaster is
// dead. Stop has a documented path that leaves the child supervised -- SIGKILL is undeliverable
// to a process in uninterruptible sleep, so on a wedged PV it gives up without clearing its
// handle -- and Start then returns nil ("still running"). With the error discarded, act()
// returned nil: no IncReconcileError, no log, and a single-node primary on a hung volume
// reported a successful restart every tick while the database was down.
func TestActRestartLocalFailsWhenTheStopCouldNotBeProven(t *testing.T) {
	pm := &fakePostmaster{running: true, stopErr: fmt.Errorf("postmaster did not exit after SIGKILL (PGDATA I/O wedged?): %w", context.DeadlineExceeded)}
	a := newTestAgent(t, pm, &fakeDCS{})
	err := a.act(context.Background(), reconcile.Decision{Action: reconcile.RestartLocal}, reconcile.Observation{})
	if err == nil {
		t.Fatal("a restart whose stop left the postmaster supervised must surface an error, not report success")
	}
	if !strings.Contains(err.Error(), "could not prove the postmaster is dead") {
		t.Errorf("the error must say what could not be established, got %v", err)
	}
}

// ...but the ORDINARY escalation -- deadline hit, killed, reaped -- is not a failure: the child
// is provably gone, so the restart proceeds.
func TestActRestartLocalProceedsWhenTheKillWasReaped(t *testing.T) {
	pm := &fakePostmaster{running: true, stopErr: context.DeadlineExceeded, deadOnStop: true}
	a := newTestAgent(t, pm, &fakeDCS{})
	if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.RestartLocal}, reconcile.Observation{}); err != nil {
		t.Fatalf("a killed-and-reaped stop must not fail the restart: %v", err)
	}
	if !pm.started {
		t.Fatal("the postmaster must be started back up")
	}
}

// Streaming from a DIFFERENT host than the target (a stale upstream after a leader
// change) must NOT be mistaken for already-following: the agent repoints via follow.
func TestActFollowRepointsWhenStreamingFromWrongUpstream(t *testing.T) {
	// Streaming, but from the OLD leader: the already-streaming shortcut must not fire, and the
	// config must end up pointing at the new target (#294: asserted on the written conninfo
	// rather than a CLI call count).
	ex := &scriptedExec{walRcv: "pg-9.h|streaming"}
	pm := &fakePostmaster{}
	a := newFollowTestAgentWithPM(t, ex, pm)
	dec := reconcile.Decision{Action: reconcile.Follow, Target: "pg-0"}
	if err := a.act(context.Background(), dec, reconcile.Observation{}); err != nil {
		t.Fatalf("act: %v", err)
	}
	assertFollowsTarget(t, a, "pg-0")
	if !pm.reloaded {
		t.Error("a repoint must reload, or the standby keeps streaming from the old upstream")
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

// A step-down whose demote was killed AND REAPED still clears the read-write latch (#298
// review). The handoff is deliberately abandoned -- a SIGKILL'd postmaster skipped its
// shutdown checkpoint, so promoting a peer now would drop WAL this node had written but not
// yet streamed -- but the writer is provably gone, and leaving servingRW armed is the
// spurious-fence bug the rest of this change removes: a lease lapse before the next tick
// would take OnLost's fence branch on a node with nothing running, incrementing
// pg_ha_agent_fences_total and paging PGHAAgentFlapping. A Fast/SIGINT shutdown of a large
// busy database routinely outruns renewDeadline (10s on chart defaults), so this is the
// ordinary switchover of a loaded primary, not an exotic case.
func TestActReleaseLeaseClearsTheLatchWhenTheKilledDemoteWasReaped(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stopErr    error
		deadOnStop bool
		wantLatch  bool
	}{
		{"killed and reaped: the writer is provably gone", context.DeadlineExceeded, true, false},
		// The wedged-PV arm: SIGKILL was undeliverable, the child is still supervised, so the
		// latch MUST stay armed or the next lease loss skips the fence on the one node that
		// needs it.
		{"still supervised: no proof, keep the latch", fmt.Errorf("leaving it supervised: %w", context.DeadlineExceeded), false, true},
		{"not a deadline expiry: never proof", errors.New("signal: operation not permitted"), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pm := &fakePostmaster{running: true, stopErr: tc.stopErr, deadOnStop: tc.deadOnStop}
			d := &fakeDCS{}
			a := newTestAgent(t, pm, d)
			a.servingRW.Store(true)
			obs := reconcile.Observation{Local: reconcile.LocalState{HasData: true, Running: true}, LocalProcessAlive: true}
			if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.ReleaseLease}, obs); err == nil {
				t.Fatal("a failed demote must still surface as an error: the handoff is abandoned")
			}
			if got := a.servingRW.Load(); got != tc.wantLatch {
				t.Errorf("servingRW = %v, want %v", got, tc.wantLatch)
			}
			// The Lease is kept either way: releasing it is what a CLEAN step-down earns.
			if d.released {
				t.Error("the Lease must not be released when the demote did not complete cleanly")
			}
		})
	}
}

// #298 review: a read-write observation that a demote OVERTOOK must be discarded. tick()
// samples the role with observe() -- multi-second and network-bound -- and stores the derived
// value outside opMu, while OnLost demotes under opMu and clears the latch. So a tick that saw a
// still-read-write postmaster could land its Store(true) after the fence had already cleared it.
// That was benign while the latch only gated the fence; it stopped being benign once
// SafeToRelease made it gate the LOCK RELEASE, where a stale true vetoes a release that was safe
// -- the peer then waits out leaseDuration instead of milliseconds, and the operator reads "may
// still be a read-write primary" about a fence that completed cleanly.
func TestDeriveServingRWDiscardsAnObservationADemoteOvertook(t *testing.T) {
	a := newTestAgent(t, &fakePostmaster{}, &fakeDCS{})
	rwPrimary := reconcile.Observation{Local: reconcile.LocalState{HasData: true, Running: true, InRecovery: false}, LocalProcessAlive: true}

	// Baseline: no demote in flight, so the observation applies.
	a.deriveServingRW(a.fenceGen.Load(), rwPrimary)
	if !a.servingRW.Load() {
		t.Fatal("a read-write primary must arm the latch when no demote intervened")
	}

	// Now the race: capture the generation as tick() does, let a demote complete, then apply.
	gen := a.fenceGen.Load()
	a.clearServingRWForPlannedStepDown() // stands in for OnLost's fence or a step-down
	a.deriveServingRW(gen, rwPrimary)
	if a.servingRW.Load() {
		t.Error("a read-write sample taken BEFORE the demote must not resurrect the latch: SafeToRelease would then veto a release that was safe")
	}

	// The safe direction is never gated -- a demote racing a standby/dead-process observation
	// must still be able to clear the latch, or the veto would outlive the writer.
	a.servingRW.Store(true)
	gen = a.fenceGen.Load()
	a.clearServingRWForPlannedStepDown()
	a.deriveServingRW(gen, reconcile.Observation{Local: reconcile.LocalState{HasData: true, Running: true, InRecovery: true}, LocalProcessAlive: true})
	if a.servingRW.Load() {
		t.Error("an observed STANDBY must clear the latch regardless of the generation")
	}
	a.servingRW.Store(true)
	gen = a.fenceGen.Load()
	a.clearServingRWForPlannedStepDown()
	a.deriveServingRW(gen, reconcile.Observation{Local: reconcile.LocalState{HasData: true}, LocalProcessAlive: false})
	if a.servingRW.Load() {
		t.Error("a dead postmaster must clear the latch regardless of the generation")
	}
}

// NOTE on what is NOT tested here (#298 review). deriveServingRW re-checks the generation AFTER
// its store, to catch a demote that lands in the window between the pre-store check and the store
// itself. That window is a few instructions wide and there is no seam to drive it from a test:
// passing a stale generation trips the FIRST check instead, and a background demote spinning
// concurrently makes the generation move continuously, so "it moved by the time I sampled" can no
// longer distinguish a demote that landed DURING the call from one that landed just after it --
// any assertion built on that is a false positive waiting to happen.
//
// A 200,000-pair sequential stress loop does catch the pre-fix code (measured: first violation
// around iteration 14k), but it misses entirely at 100k, so it is unreliable as a regression
// test and costs ~8s under -race. The post-store re-check is therefore argued from the ordering
// -- both demote sites bump fenceGen BEFORE clearing servingRW, so a bump observed after the
// store means the false has landed or is about to -- and pinned only in the deterministic
// demote-before-derive direction, above.

// ...and the uncertainty case still HOLDS the latch: SQL unreachable while the process is alive
// is the wedged-writer state, and neither arm of deriveServingRW may touch it.
func TestDeriveServingRWHoldsTheLatchOnAnUnreachableProbe(t *testing.T) {
	a := newTestAgent(t, &fakePostmaster{}, &fakeDCS{})
	a.servingRW.Store(true)
	a.deriveServingRW(a.fenceGen.Load(), reconcile.Observation{
		Local:             reconcile.LocalState{HasData: true, Running: false},
		LocalProcessAlive: true,
	})
	if !a.servingRW.Load() {
		t.Error("an unreachable probe on a live postmaster must leave the latch armed (fail-safe: it may still be a writer)")
	}
}

// #298 review: a PLANNED step-down must not read as a split-brain fence. dcs.OnLost fires for
// a voluntary Release() too and gates only on servingRW, so a latch left set here counted an
// IncFence() and logged "lost leadership; demoting (fence)" -- three switchovers in a
// maintenance window trip the chart's PGHAAgentFlapping page for work an operator asked for.
func TestActReleaseLeaseClearsServingRWAfterACompletedDemote(t *testing.T) {
	pm := &fakePostmaster{}
	d := &fakeDCS{}
	a := newTestAgent(t, pm, d)
	a.servingRW.Store(true)
	obs := reconcile.Observation{Local: reconcile.LocalState{HasData: true, Running: true, InRecovery: false}}
	if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.ReleaseLease}, obs); err != nil {
		t.Fatalf("act: %v", err)
	}
	if !pm.stopped {
		t.Fatal("a read-write primary must be demoted before the lease is released")
	}
	if a.servingRW.Load() {
		t.Fatal("a completed demote is proof this node is no longer a writer: the fence latch must be clear before Release")
	}
}

// The self-health arm is the same: the force-stop returned nil, so the node is provably down.
func TestActReleaseLeaseClearsServingRWAfterAForceStop(t *testing.T) {
	pm := &fakePostmaster{}
	a := newTestAgent(t, pm, &fakeDCS{})
	a.servingRW.Store(true)
	obs := reconcile.Observation{LocalStuck: true, Local: reconcile.LocalState{HasData: true, Running: false, InRecovery: false}}
	if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.ReleaseLease}, obs); err != nil {
		t.Fatalf("act: %v", err)
	}
	if a.servingRW.Load() {
		t.Fatal("a completed force-stop must clear the fence latch")
	}
}

// ...but the clear is NOT unconditional, and this is the case that makes it matter. Local.Running
// is SQL reachability, not process liveness: a read-write postmaster that is wedged (or merely
// slow) fails the probe while still being a writer, and until it passes the stuck grace
// LocalStuck is false too -- so NEITHER arm runs and nothing stopped it. Decide still reaches
// ReleaseLease here via the highwater guard on stopped primary-state data. Clearing the latch on
// that uncertainty would release the Lease AND disarm the OnLost fence for the one node that
// most needs it, letting a peer promote beside a writer that may thaw. tick() refuses to clear
// on the same evidence; so must this.
func TestActReleaseLeaseKeepsServingRWWhenNothingWasStopped(t *testing.T) {
	pm := &fakePostmaster{}
	d := &fakeDCS{}
	a := newTestAgent(t, pm, d)
	a.servingRW.Store(true)
	obs := reconcile.Observation{Local: reconcile.LocalState{HasData: true, Running: false, InRecovery: false}}
	if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.ReleaseLease}, obs); err != nil {
		t.Fatalf("act: %v", err)
	}
	if pm.stopped {
		t.Fatal("neither arm applies here; nothing should have been stopped")
	}
	if !d.released {
		t.Fatal("lease must still be released")
	}
	if !a.servingRW.Load() {
		t.Fatal("no demote or stop ran, so this node may still be a writer: the fence latch must stay armed")
	}
}

// Switchover clears it too -- the demote above it succeeded -- so a controlled handoff is not
// counted as a fence either.
func TestActSwitchoverClearsServingRWAfterTheDemote(t *testing.T) {
	pm := &fakePostmaster{}
	a := newTestAgent(t, pm, &fakeDCS{})
	a.kube = k8s.NewWithClient(k8sfake.NewSimpleClientset(), "ns")
	a.servingRW.Store(true)
	obs := reconcile.Observation{Local: reconcile.LocalState{HasData: true, Running: true, InRecovery: false}}
	if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.Switchover, Target: "pg-1"}, obs); err != nil {
		t.Fatalf("act: %v", err)
	}
	if !pm.stopped {
		t.Fatal("the handoff must demote before releasing")
	}
	if a.servingRW.Load() {
		t.Fatal("a controlled switchover must not leave the node looking read-write to OnLost")
	}
}

// promoteTimeoutExec fails `pg_ctl promote` the way PGCTLTIMEOUT does -- non-zero exit,
// "server did not promote in time" -- while the promotion itself carries on in the postmaster.
type promoteTimeoutExec struct{}

func (promoteTimeoutExec) Run(_ context.Context, _ []string, name string, args ...string) (string, error) {
	if strings.Contains(strings.Join(args, " "), "promote") || strings.HasSuffix(name, "pg_ctl") {
		return "pg_ctl: server did not promote in time", fmt.Errorf("exit status 1")
	}
	return "", nil
}

// #298 review: a non-zero `pg_ctl -w promote` is NOT proof this node did not become a writer.
// pg_ctl gives up at PGCTLTIMEOUT (60s) on a standby with a large unreplayed backlog while the
// promotion signal is already written and the postmaster finishes promoting anyway. Arming the
// latch only on the success path left servingRW false on a node that was becoming read-write,
// so the lease loss that followed took OnLost's "no fence needed" branch -- two primaries.
func TestActPromoteArmsServingRWBeforeThePromoteCanTimeOut(t *testing.T) {
	a := newSlotTestAgent(t, &slotExec{}, config.MechanismNative)
	a.mech = mechanism.NewNative(a.cfg.PGDATA, "/usr/lib/postgresql/18/bin", "pw", "pg_ha_slot_0", "pg-0")
	a.mech.(*mechanism.Native).Runner = promoteTimeoutExec{}
	err := a.act(context.Background(), reconcile.Decision{Action: reconcile.Promote}, reconcile.Observation{
		HoldLease: true,
		Local:     reconcile.LocalState{HasData: true, Running: true, InRecovery: true},
	})
	if err == nil {
		t.Fatal("the promote was supposed to report the pg_ctl timeout")
	}
	if !a.servingRW.Load() {
		t.Fatal("servingRW must be armed BEFORE the promote: a pg_ctl timeout does not mean the node stayed read-only, and an unarmed latch skips the fence on a node that is becoming a writer")
	}
}

// #298 review: Wait must not retract the slot gauges. Wait is what Decide returns when there is
// no known leader -- a step-down cooldown, an apiserver or etcd partition -- and a standby
// sitting on an inactive, WAL-pinning slot through one of those had all three slot alerts
// resolved and their for: clocks (5m/15m/1h) restarted on EVERY tick, a blind window over the
// disk-fill hazard #289 exists to catch, opened exactly when WAL accumulates.
func TestActWaitKeepsTheSlotGaugesStanding(t *testing.T) {
	a := newTestAgent(t, &fakePostmaster{}, &fakeDCS{})
	a.metr.SetSlots(observe.SlotStats{Total: 3, Inactive: 1, MaxRetainedWALBytes: 4096})
	a.metr.SetTopology(observe.TopologyStats{Streaming: 2, Expected: 2})
	if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.Wait}, reconcile.Observation{}); err != nil {
		t.Fatalf("act: %v", err)
	}
	rec := httptest.NewRecorder()
	a.metr.Handler(time.Minute).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "pg_ha_agent_replication_slot_max_retained_wal_bytes 4096") {
		t.Errorf("Wait retracted the slot gauges, silencing the slot alerts for the whole partition:\n%s", body)
	}
	if !strings.Contains(body, "pg_ha_agent_replication_slots_inactive 1") {
		t.Errorf("Wait retracted the inactive-slot gauge:\n%s", body)
	}
	// Topology IS still retracted: it is the PRIMARY's connection list, has no standby
	// equivalent, and a stale one latched under max() is worse than none.
	if !strings.Contains(body, "pg_ha_agent_replicas_expected 0") {
		t.Errorf("Wait must still retract the topology gauges:\n%s", body)
	}
}

// #298 review: StartLocal's standby arm ASSERTS standby.signal instead of trusting it. The
// branch was asymmetric -- it cleared the file for primary-state data but never created it for
// standby-state data -- and InRecovery comes from pg_controldata, not from the file. A control
// file reading "in archive recovery" with the signal gone (the RejoinForceRewind window, an
// unclean node reboot) was started READ-WRITE on the live primary's timeline.
func TestActStartLocalAssertsStandbySignalForStandbyState(t *testing.T) {
	pm := &fakePostmaster{}
	a := newTestAgent(t, pm, &fakeDCS{})
	sig := filepath.Join(a.cfg.PGDATA, "standby.signal")
	if _, err := os.Stat(sig); !os.IsNotExist(err) {
		t.Fatalf("the fixture must start without a signal file: %v", err)
	}
	obs := reconcile.Observation{Local: reconcile.LocalState{HasData: true, Running: false, InRecovery: true}}
	if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.StartLocal}, obs); err != nil {
		t.Fatalf("act: %v", err)
	}
	if _, err := os.Stat(sig); err != nil {
		t.Fatalf("standby-state data was started with no standby.signal, i.e. read-write: %v", err)
	}
	if !pm.started {
		t.Fatal("postmaster must be started")
	}
	if a.servingRW.Load() {
		t.Fatal("a standby start must not arm servingRW")
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

func TestBaseName(t *testing.T) {
	// nodeIDBase lives only inside slotOrdinal, which has its own tests.
	if got := podname.Base("my-pg-0"); got != "my-pg" {
		t.Errorf("baseName = %q, want my-pg", got)
	}
}

// #308: no-op entirely (byte-identical to today) unless cfg.SyncReplicationSlots is set,
// even with active standby slots to reconcile.
func TestAssertSyncStandbySlotsNoOpWhenDisabled(t *testing.T) {
	ex := &scriptedExec{slots: "repmgr_slot_1001|t|0|reserved|t\n"}
	a := newFollowTestAgent(t, ex)
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), nil, true)
	if len(ex.slotSyncSQL) != 0 {
		t.Fatalf("expected no SQL when disabled, got %v", ex.slotSyncSQL)
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
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), nil, true)
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
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), nil, true)
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
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), nil, true)
	if len(ex.slotSyncSQL) != 2 {
		t.Fatalf("first call: expected 2 SQL statements, got %v", ex.slotSyncSQL)
	}
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), nil, true)
	if len(ex.slotSyncSQL) != 2 {
		t.Fatalf("second call with an unchanged slot set must not issue more SQL, got %v", ex.slotSyncSQL)
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
		// Legacy repmgr slots: reclaimable when the pod is DEPARTED or the ordinal has MIGRATED
		// (its new-scheme slot is active, proving the standby moved). An earlier round made them
		// unconditional, reasoning that scoping to departed ordinals leaves a permanent orphan
		// per surviving node -- true, and answered by the migrated signal rather than by
		// reverting. `AND NOT active` alone is NOT sufficient protection: mid-restart the old
		// slot is inactive and the new one may not exist yet, so dropping it there costs the
		// re-clone #292 exists to avoid.
		{"legacy slot, departed ordinal", "repmgr_slot_1003", live3, true, "no pod-3 exists"},
		{"legacy slot, live ordinal, not yet migrated", "repmgr_slot_1001", live3, false, "pod-1 may still be mid-restart; dropping now risks a WAL gap"},
		{"legacy slot for self", "repmgr_slot_1000", live3, true, "nobody streams from a node to itself"},
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
		{"empty pod list, legacy slot", "repmgr_slot_1009", map[int]bool{}, false, "no authoritative pod set and no migration proof"},
		{"empty pod list, own slot", "pg_ha_slot_0", map[int]bool{}, true, "self is unused regardless of the pod set"},
	} {
		if got := orphanSlot(tc.slot, self, tc.live, nil); got != tc.want {
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
	if orphanSlot("pg_ha_slot_2", "pg-0", live, nil) {
		t.Error("pg_ha_slot_2 reclaimed during a scale-up: a live standby's slot must never be dropped, even when the primary's NodeCount is stale")
	}
}

// A promoted pod's own slot becomes reclaimable, and the ex-primary's does not: ownership
// follows whoever currently holds the lease, so the set shifts on failover.
func TestOrphanSlotSelfSlotFollowsTheCurrentPrimary(t *testing.T) {
	live := map[int]bool{0: true, 1: true, 2: true}
	if !orphanSlot("pg_ha_slot_1", "pg-1", live, nil) {
		t.Error("pg-1 as primary should reclaim its own slot pg_ha_slot_1")
	}
	if orphanSlot("pg_ha_slot_0", "pg-1", live, nil) {
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
	// createErr fails the create statement. The recycle path needs it: its drop and its create
	// are one psql call and the drop is not transactional, so "the create failed" and "the slot
	// may now be gone" are the same state.
	createErr error
}

func (s *slotExec) Run(_ context.Context, _ []string, name string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	switch {
	case name == "psql" && strings.Contains(joined, "pg_create_physical_replication_slot"):
		s.created = append(s.created, slotArg(joined))
		return "", s.createErr
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

// #298 review: a promote that outlives the lease must not claim the routing. `pg_ctl -w
// promote` is bounded only by PGCTLTIMEOUT (60s) while the default LeaseDuration is 15s, so on
// the ordinary failover case -- a standby with a large unreplayed backlog -- the lease can lapse
// and be won by a peer mid-promote. OnLost cannot correct it first: it blocks on opMu, which
// tick() holds for all of act(). Continuing anyway pointed the write Service selector and the
// pg-role=primary label at a pod that no longer held the lease, on top of the genuine new
// primary. servingRW stays armed (this node really is read-write) -- the fence is OnLost's job.
func TestActPromoteDoesNotClaimRoutingAfterLosingTheLease(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pg-0", Namespace: "ns", Labels: map[string]string{"app": "pg"}}},
	)
	a := &agent{
		cfg: &config.Config{
			PodName: "pg-0", PGDATA: t.TempDir(), HeadlessService: "h",
			RepmgrUser: "repmgr", RepmgrDB: "repmgr", RepmgrPassword: "pw",
			Namespace: "ns", MarkerName: "pg-primary",
			LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second,
		},
		base:   "pg",
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		dcs:    &fakeDCS{leader: false}, // the lease lapsed while pg_ctl was still promoting
		mech:   &rewindStubMech{},       // Promote succeeds
		prober: &pg.Prober{Exec: &slotExec{}, Timeout: time.Second},
		sup:    process.NewSupervisor(&fakePostmaster{running: true}),
		kube:   k8s.NewWithClient(cs, "ns"),
		metr:   observe.New(),
	}
	obs := reconcile.Observation{Local: reconcile.LocalState{HasData: true, Running: true, InRecovery: true, Timeline: 7, TimelineOK: true}}
	if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.Promote}, obs); err != nil {
		t.Fatalf("act: %v", err)
	}
	// The highwater marker is the durable half of the claim: writing it from a node that no
	// longer holds the lease advances the cluster's recorded timeline on the strength of a
	// promotion nobody asked this node to publish.
	if _, err := cs.CoreV1().ConfigMaps("ns").Get(context.Background(), "pg-primary", metav1.GetOptions{}); err == nil {
		t.Error("a node that lost the lease mid-promote must not advance the highwater marker")
	}
	// The label is the live half: it is what the write Service selects on.
	pod, err := cs.CoreV1().Pods("ns").Get(context.Background(), "pg-0", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Labels["pg-role"] == "primary" {
		t.Error("a node that lost the lease mid-promote must not claim the write Service")
	}
	// It IS read-write, though, so the fence must still be armed for OnLost.
	if !a.servingRW.Load() {
		t.Error("the node really did promote: servingRW must stay armed so OnLost fences it")
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
	// initdbEnv records the extra env of the `entrypoint.sh initdb` call. Exec.Run strips
	// every *PASSWORD* variable from the inherited environment (#298 security review), so
	// the passwords bootstrap_initdb hard-requires reach it ONLY through this slice.
	initdbEnv []string
	// dataDir lets the fake do what a real `entrypoint.sh initdb` does: LEAVE A CLUSTER
	// BEHIND. Seeding it up front in the fixture would not model the branch --
	// bootstrapInitdbNative clears non-database debris from a PG_VERSION-less PGDATA before
	// shelling out (#298), exactly as BootstrapClone
	// does, so anything staged before the call is debris by that same definition.
	dataDir string
}

func (e *initdbExec) Run(_ context.Context, env []string, name string, args ...string) (string, error) {
	e.calls = append(e.calls, append([]string{name}, args...))
	if name == entrypointPath && len(args) > 0 && args[0] == "initdb" {
		e.initdbEnv = env
	}
	if e.err != nil {
		return "boom", e.err
	}
	// A successful bootstrap leaves PG_VERSION and a postgresql.conf for the agent's own
	// GenerateConfig/ensureInclude to append to.
	if e.dataDir != "" && name == entrypointPath && len(args) > 0 && args[0] == "initdb" {
		_ = os.WriteFile(filepath.Join(e.dataDir, "PG_VERSION"), []byte("18\n"), 0o600)
		_ = os.WriteFile(filepath.Join(e.dataDir, "postgresql.conf"), []byte("# seeded\n"), 0o600)
	}
	// Answer the post-initdb timeline read (#288 review, round 3): the marker write now POLLS
	// for the postmaster to accept SQL, because sup.Start is fire-and-forget. A fake that never
	// answers would make every one of these tests sit out the whole budget.
	if name == "psql" && strings.Contains(strings.Join(args, " "), "pg_walfile_name") {
		return "00000001|0/3000000", nil
	}
	return "", nil
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
	// The passwords must ride the explicit extra-env slice: Exec.Run strips every
	// *PASSWORD* variable from the inherited environment (#298 security review), and
	// bootstrap_initdb's first act is `: "${REPMGR_PASSWORD:?}"` -- inherited-only
	// passwords made every fresh native install fail that guard forever.
	if !slices.Contains(ex.initdbEnv, "REPMGR_PASSWORD=pw") {
		t.Errorf("initdb env %v lacks REPMGR_PASSWORD=pw", ex.initdbEnv)
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
	// The fake exec creates PGDATA's contents when `entrypoint.sh initdb` is "run", the way a
	// real bootstrap does -- not up front, which the pre-initdb debris clear would (correctly)
	// wipe.
	ex.dataDir = dataDir
	m := mechanism.NewNative(dataDir, "/usr/lib/postgresql/18/bin", "pw", "pg_ha_slot_0", "pg-0")
	return &agent{
		cfg: &config.Config{
			PGDATA: dataDir, PodName: "pg-0", HeadlessService: "h",
			RepmgrUser: "repmgr", RepmgrDB: "repmgr", RepmgrPassword: "pw",
			Mechanism: mech, PgHbaPeerCIDR: "10.0.0.0/8", RenewDeadline: 2 * time.Second,
			Namespace: "ns", MarkerName: "pg-primary",
		},
		base: "pg", // production sets this from podname.Base(cfg.PodName); the target guard needs it
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
func TestTopologyTickPublishesTheFullPicture(t *testing.T) {
	// The expected/gap half is unconditional. It needs the live pod set, and reconcileSlots is
	// already making that apiserver LIST on this path, so it costs nothing extra.
	//
	// Two streaming rows (the catchup one is not counted), one of them unidentifiable.
	const rows = "pg-1|pg_ha_slot_1|streaming\nwalreceiver||streaming\npg-9|pg_ha_slot_9|catchup\n"

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
			t.Errorf("missing %q in:\n%s", want, body)
		}
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
	// The initdb attempt is recorded; anything after it belongs to the discard the sibling
	// test asserts (the fake now leaves a real PG_VERSION behind, so discardFreshDataDir
	// reaps the bootstrap postmaster first). What must NOT appear is a start.
	if len(ex.calls) == 0 || ex.calls[0][0] != entrypointPath || ex.calls[0][1] != "initdb" {
		t.Fatalf("want the initdb attempt recorded first, got %v", ex.calls)
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
// rewind, a wipe. Adoption does NOT come through here (it stamps adoptedAt instead, see
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

// --- #293: repmgr preload removal + presence check ---

// newPreloadTestAgent builds the minimum agent the #293 helpers touch: a PGDATA on disk,
// a mechanism, and a repmgr.so location the test controls.
func newPreloadTestAgent(t *testing.T, mech, pgdata, modulePath string) *agent {
	t.Helper()
	return &agent{
		cfg:              &config.Config{PGDATA: pgdata, Mechanism: mech},
		log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		repmgrModulePath: modulePath,
	}
}

// writePGDATAConf lays down a PGDATA/postgresql.conf and returns its path.
func writePGDATAConf(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, "postgresql.conf")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDropRepmgrPreloadStripsUnderNative(t *testing.T) {
	dir := t.TempDir()
	conf := writePGDATAConf(t, dir, "wal_level = replica\nshared_preload_libraries = 'repmgr,pgaudit'\n")
	// The module is present, so the presence check must not fire on the way out.
	so := filepath.Join(dir, "repmgr.so")
	if err := os.WriteFile(so, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newPreloadTestAgent(t, config.MechanismNative, dir, so)
	if err := a.dropRepmgrPreload(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "repmgr") {
		t.Errorf("repmgr survived in PGDATA under native:\n%s", got)
	}
	if !strings.Contains(string(got), "shared_preload_libraries = 'pgaudit'") {
		t.Errorf("the other libraries were not preserved:\n%s", got)
	}
}

func TestDropRepmgrPreloadIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	conf := writePGDATAConf(t, dir, "shared_preload_libraries = 'repmgr'\n")
	so := filepath.Join(dir, "repmgr.so")
	if err := os.WriteFile(so, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newPreloadTestAgent(t, config.MechanismNative, dir, so)
	for i := 0; i < 3; i++ {
		if err := a.dropRepmgrPreload(); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	got, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "repmgr") {
		t.Errorf("repmgr survived:\n%s", got)
	}
}

func TestDropRepmgrPreloadRefusesToStartWhenTheModuleIsAbsent(t *testing.T) {
	// An image that no longer ships repmgr.so while PGDATA still requests it: the presence
	// check is the only thing between the operator and an opaque `could not access file
	// "repmgr"` crash-loop on every pod at once.
	dir := t.TempDir()
	writePGDATAConf(t, dir, "shared_preload_libraries = 'repmgr'\n")
	a := newPreloadTestAgent(t, config.MechanismNative, dir, filepath.Join(dir, "absent.so"))
	err := a.assertPreloadedLibsPresent()
	if err == nil {
		t.Fatal("expected a refusal when the requested module is absent")
	}
	// The message must name the value, the file, and the migration -- the whole point of
	// preferring it to PostgreSQL's own error.
	for _, want := range []string{"shared_preload_libraries", "repmgr", "#293", "postgresql.conf"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message does not mention %q: %v", want, err)
		}
	}
}

// There is deliberately no second, mechanism-specific remediation ("do NOT just drop the
// library") for a repmgr-MECHANISM node: it would be unreachable, because config.Load rejects
// MECHANISM=repmgr outright, so no agent can
// be constructed with a non-native mechanism. Asserting unreachability directly is what keeps
// the deletion honest -- if Load ever starts accepting it again, the remediation has to come
// back, and this is the test that will say so.
func TestConfigLoadRejectsTheRepmgrMechanism(t *testing.T) {
	load := func(mechanism string) error {
		_, err := config.Load(func(k string) string {
			if k == "MECHANISM" {
				return mechanism
			}
			return ""
		})
		return err
	}
	// Load also fails on every other missing required variable, so "it returned an error" is
	// no evidence at all -- the assertion has to be on the repmgr-specific sentence. The
	// native control below is what proves the marker is not simply always present.
	const marker = "was removed in chart 2.0.0"
	err := load(config.MechanismRepmgr)
	if err == nil || !strings.Contains(err.Error(), marker) {
		t.Fatalf("config.Load must reject MECHANISM=repmgr with the removal notice (%q); the mechanism-specific preload remediation in assertPreloadedLibsPresent would be reachable again and must be restored. got: %v", marker, err)
	}
	if nerr := load(config.MechanismNative); nerr != nil && strings.Contains(nerr.Error(), marker) {
		t.Fatalf("the removal notice fires under MECHANISM=native too, so the assertion above proves nothing: %v", nerr)
	}
}

func TestAssertPreloadedLibsPresentAdvisesRemovalOnANativeNode(t *testing.T) {
	// Under native the library genuinely is dead weight, so removing it IS the fix -- the
	// opposite advice from the repmgr-mechanism case above.
	dir := t.TempDir()
	writePGDATAConf(t, dir, "shared_preload_libraries = 'repmgr'\n")
	a := newPreloadTestAgent(t, config.MechanismNative, dir, filepath.Join(dir, "absent.so"))
	err := a.assertPreloadedLibsPresent()
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "restart the pod") {
		t.Errorf("native remediation should say to remove it and restart: %v", err)
	}
}

func TestDropRepmgrPreloadAlsoCleansPostgresqlAutoConf(t *testing.T) {
	// postgresql.auto.conf is read LAST, so it beats postgresql.conf and every conf.d
	// fragment, and it is where ALTER SYSTEM lands. Leaving it while the presence check
	// treats it as fatal turns one admin ALTER SYSTEM into an unremediable CrashLoopBackOff
	// on the repmgr-free image (#293 review).
	dir := t.TempDir()
	writePGDATAConf(t, dir, "wal_level = replica\n")
	auto := filepath.Join(dir, "postgresql.auto.conf")
	if err := os.WriteFile(auto, []byte("# Do not edit this file manually!\nshared_preload_libraries = 'repmgr,pgaudit'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newPreloadTestAgent(t, config.MechanismNative, dir, filepath.Join(dir, "absent.so"))
	// preflightPreload must now come up clean: the strip covers the file the check scans.
	if err := a.preflightPreload(); err != nil {
		t.Fatalf("auto.conf must be stripped before the check refuses it: %v", err)
	}
	got, err := os.ReadFile(auto)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "repmgr") {
		t.Errorf("repmgr survived in postgresql.auto.conf:\n%s", got)
	}
	if !strings.Contains(string(got), "shared_preload_libraries = 'pgaudit'") {
		t.Errorf("auto.conf lost its other libraries:\n%s", got)
	}
	if !strings.Contains(string(got), "Do not edit this file manually") {
		t.Errorf("auto.conf's header was dropped:\n%s", got)
	}
}

func TestAssertPreloadedLibsPresentIsDisarmedByDynamicLibraryPath(t *testing.T) {
	// PostgreSQL resolves an unqualified entry against dynamic_library_path, so a cluster
	// loading repmgr.so from an extra directory has a working library the pkglibdir stat
	// cannot see. Refusing there would hard-exit every pod over a library that loads fine --
	// the asymmetric false positive this check is built to avoid (#293 review).
	dir := t.TempDir()
	writePGDATAConf(t, dir, "dynamic_library_path = '$libdir:/opt/pg/lib'\nshared_preload_libraries = 'repmgr'\n")
	a := newPreloadTestAgent(t, config.MechanismRepmgr, dir, filepath.Join(dir, "absent.so"))
	if err := a.assertPreloadedLibsPresent(); err != nil {
		t.Fatalf("an overridden dynamic_library_path must disarm the refusal: %v", err)
	}
}

func TestDropRepmgrPreloadStartsCleanlyWhenNothingRequestsTheAbsentModule(t *testing.T) {
	// The #290 end state: no repmgr.so in the image and no configuration asking for it.
	dir := t.TempDir()
	writePGDATAConf(t, dir, "wal_level = replica\nshared_preload_libraries = 'pgaudit'\n")
	a := newPreloadTestAgent(t, config.MechanismNative, dir, filepath.Join(dir, "absent.so"))
	if err := a.assertPreloadedLibsPresent(); err != nil {
		t.Fatalf("a clean cluster on a repmgr-free image must boot: %v", err)
	}
}

func TestDropRepmgrPreloadStripThenPassesItsOwnPresenceCheck(t *testing.T) {
	// The sequence that makes a direct 1.x -> #290 jump survivable: the strip runs first,
	// so the presence check it feeds finds nothing requesting the missing module.
	dir := t.TempDir()
	writePGDATAConf(t, dir, "shared_preload_libraries = 'repmgr'\n")
	a := newPreloadTestAgent(t, config.MechanismNative, dir, filepath.Join(dir, "absent.so"))
	// The order run() uses: boot() strips, then the fatal check runs.
	if err := a.dropRepmgrPreload(); err != nil {
		t.Fatalf("strip: %v", err)
	}
	if err := a.assertPreloadedLibsPresent(); err != nil {
		t.Fatalf("the strip must leave nothing for the presence check to refuse: %v", err)
	}
}

func TestAssertPreloadedLibsPresentDoesNotRefuseOnAWrongModuleDirectory(t *testing.T) {
	// The asymmetric false positive: if the module directory itself is missing, that is
	// evidence the PATH is wrong (a distro layout change, a PG_MAJOR the image was not built
	// for), not that repmgr.so is absent. Refusing there would take down every
	// repmgr-mechanism pod on a cluster whose repmgr.so is present and working -- strictly
	// worse than the crash-loop this check exists to explain.
	dir := t.TempDir()
	writePGDATAConf(t, dir, "shared_preload_libraries = 'repmgr'\n")
	a := newPreloadTestAgent(t, config.MechanismRepmgr, dir, filepath.Join(dir, "no-such-dir", "repmgr.so"))
	if err := a.assertPreloadedLibsPresent(); err != nil {
		t.Fatalf("a missing module DIRECTORY must not refuse the boot: %v", err)
	}
}

func TestPreflightPreloadStripsThenVerifies(t *testing.T) {
	// The order run() depends on: the strip removes the request before the check looks for
	// it, which is what makes a direct 1.x -> repmgr-free-image jump survivable rather than
	// a refusal on the very cluster the strip would have fixed (#293 review).
	dir := t.TempDir()
	writePGDATAConf(t, dir, "shared_preload_libraries = 'repmgr'\n")
	a := newPreloadTestAgent(t, config.MechanismNative, dir, filepath.Join(dir, "absent.so"))
	if err := a.preflightPreload(); err != nil {
		t.Fatalf("strip must precede the check: %v", err)
	}
}

func TestPreflightPreloadNeedsNoDataDirectory(t *testing.T) {
	// It runs before boot()'s HasData gate now, so an empty PGDATA must be a clean no-op
	// rather than a fatal read error.
	dir := t.TempDir()
	a := newPreloadTestAgent(t, config.MechanismNative, dir, filepath.Join(dir, "absent.so"))
	if err := a.preflightPreload(); err != nil {
		t.Fatalf("an uninitialized PGDATA must not fail the preflight: %v", err)
	}
}

// --- #294: synchronized_standby_slots under the NATIVE mechanism ---
//
// Before this, the reconcile resolved standbys from repmgr.nodes and named slots
// repmgr_slot_<id>, so on a native cluster it errored every tick while the chart still
// rendered sync_replication_slots = on -- #308's protection silently absent. The native path
// consumes the slot set reconcileSlots already owns, so the creator and the waiter cannot
// disagree.

func newNativeSyncTestAgent(t *testing.T, ex *scriptedExec) *agent {
	t.Helper()
	a := newFollowTestAgent(t, ex)
	a.cfg.Mechanism = config.MechanismNative
	a.cfg.SyncReplicationSlots = true
	return a
}

func TestAssertSyncStandbySlotsUsesTheAgentOwnedSetUnderNative(t *testing.T) {
	ex := &scriptedExec{slots: "pg_ha_slot_1|t|0|reserved|t\npg_ha_slot_2|t|0|reserved|t\n"}
	a := newNativeSyncTestAgent(t, ex)
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), []string{"pg_ha_slot_1", "pg_ha_slot_2"}, true)
	if len(ex.slotSyncSQL) != 2 {
		t.Fatalf("expected ALTER SYSTEM + reload, got %v", ex.slotSyncSQL)
	}
	if !strings.Contains(ex.slotSyncSQL[0], "pg_ha_slot_1,pg_ha_slot_2") {
		t.Errorf("did not name the agent-owned slots: %q", ex.slotSyncSQL[0])
	}
	// repmgr.nodes must not be consulted at all: it does not exist on a native cluster, and
	// reading it fails every tick.
	if ex.nodesQueries != 0 {
		t.Errorf("native path queried repmgr.nodes %d time(s)", ex.nodesQueries)
	}
	if a.lastSyncStandbySlots == nil || *a.lastSyncStandbySlots != "pg_ha_slot_1,pg_ha_slot_2" {
		t.Errorf("cache = %v", a.lastSyncStandbySlots)
	}
}

func TestAssertSyncStandbySlotsUnderNativeNamesOnlySlotsThatExist(t *testing.T) {
	// A slot reconcileSlots only just created is not in `existing` yet (that read happened
	// first). Naming a missing slot in synchronized_standby_slots makes the primary hold WAL
	// and log repeatedly, so it must wait a tick.
	ex := &scriptedExec{slots: "pg_ha_slot_1|t|0|reserved|t\n"}
	a := newNativeSyncTestAgent(t, ex)
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), []string{"pg_ha_slot_1", "pg_ha_slot_2"}, true)
	if len(ex.slotSyncSQL) != 2 {
		t.Fatalf("expected ALTER SYSTEM + reload, got %v", ex.slotSyncSQL)
	}
	if strings.Contains(ex.slotSyncSQL[0], "pg_ha_slot_2") {
		t.Errorf("named a slot that does not exist yet: %q", ex.slotSyncSQL[0])
	}
	if !strings.Contains(ex.slotSyncSQL[0], "pg_ha_slot_1") {
		t.Errorf("dropped the slot that does exist: %q", ex.slotSyncSQL[0])
	}
}

func TestAssertSyncStandbySlotsUnderNativeReconcilesAnEmptySet(t *testing.T) {
	// A single-node native cluster owns no standby slots. That is a real value, not a skip:
	// it must clear whatever a previous topology left in the GUC.
	ex := &scriptedExec{}
	a := newNativeSyncTestAgent(t, ex)
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), nil, true)
	if len(ex.slotSyncSQL) != 2 {
		t.Fatalf("an empty desired set must still reconcile, got %v", ex.slotSyncSQL)
	}
	if a.lastSyncStandbySlots == nil || *a.lastSyncStandbySlots != "" {
		t.Errorf("cache = %v, want a pointer to \"\"", a.lastSyncStandbySlots)
	}
}

func TestAssertSyncStandbySlotsUnderNativeIsStableAcrossTicks(t *testing.T) {
	// reconcileSlots walks ordinals ascending, so the owned set is already ordered; an
	// unchanged topology must not re-run ALTER SYSTEM every 5s.
	ex := &scriptedExec{slots: "pg_ha_slot_1|t|0|reserved|t\npg_ha_slot_2|t|0|reserved|t\n"}
	a := newNativeSyncTestAgent(t, ex)
	owned := []string{"pg_ha_slot_1", "pg_ha_slot_2"}
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), owned, true)
	first := len(ex.slotSyncSQL)
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), owned, true)
	if len(ex.slotSyncSQL) != first {
		t.Errorf("steady-state tick re-ran the GUC write: %v", ex.slotSyncSQL)
	}
}

func TestReconcileSlotsReportsTheSlotsItOwns(t *testing.T) {
	// The contract the native sync path depends on: reconcileSlots reports the slots PRESENT
	// on this node that back a live standby, ascending by ordinal, excluding the primary's
	// own. Present, not merely expected -- naming a slot that does not exist makes the
	// primary refuse to release WAL.
	ex := &slotExec{}
	a := newSlotTestAgent(t, ex, config.MechanismNative)
	a.cfg.NodeCount = 2
	observed := []pg.SlotState{
		{Name: "pg_ha_slot_1", Active: true, Reserving: true},
		{Name: "pg_ha_slot_0", Active: false, Reserving: true}, // the primary's own: never waited on
		{Name: "operator_slot", Active: true, Reserving: true}, // not ours to touch or wait on
	}
	owned, ownedOK := a.reconcileSlots(context.Background(), observed)
	if !ownedOK {
		t.Fatal("the owned set must be trustworthy when the pod list succeeds")
	}
	if len(owned) != 1 || owned[0] != "pg_ha_slot_1" {
		t.Fatalf("owned = %v, want [pg_ha_slot_1] (pod-0 is self, operator_slot is not ours)", owned)
	}
}

func TestReconcileSlotsReportsNothingItCannotSee(t *testing.T) {
	// A slot the create pass only just minted is not in the observed list yet, and must not be
	// named until a later tick confirms it exists.
	ex := &slotExec{}
	a := newSlotTestAgent(t, ex, config.MechanismNative)
	a.cfg.NodeCount = 2
	if owned, _ := a.reconcileSlots(context.Background(), nil); len(owned) != 0 {
		t.Errorf("owned = %v, want none when nothing is observed yet", owned)
	}
}

func TestReconcileSlotsSkipsPreCreationUnderCascade(t *testing.T) {
	// Cascading: a standby streams from a PEER, so its slot belongs on that peer. A slot
	// minted here would sit inactive forever, retaining WAL until max_slot_wal_keep_size
	// invalidates it -- the #289 failure, on a healthy cluster. Followers self-provision via
	// ensureSlotOnUpstream, so skipping creation loses nothing (#294).
	ex := &slotExec{}
	a := newSlotTestAgent(t, ex, config.MechanismNative)
	a.cfg.NodeCount = 2
	a.cfg.CascadeReplication = true
	a.reconcileSlots(context.Background(), nil)
	if len(ex.created) != 0 {
		t.Errorf("pre-created %v under cascading replication", ex.created)
	}
}

func TestReconcileSlotsStillPreCreatesWithoutCascade(t *testing.T) {
	// The star topology keeps its pre-creation: it is what covers the tick of latency between
	// a pod appearing and its own Clone/Follow ensuring the slot.
	ex := &slotExec{}
	a := newSlotTestAgent(t, ex, config.MechanismNative)
	a.cfg.NodeCount = 2
	a.reconcileSlots(context.Background(), nil)
	if len(ex.created) == 0 {
		t.Error("expected the star topology to pre-create the expected standby's slot")
	}
}

// An INVALIDATED slot for a live standby must be recycled by the primary's create pass
// (#298 review). It IS a slot, so a bare presence check skipped the create -- and nothing
// else on the primary reclaims it, because orphanSlot deliberately keeps any slot whose
// ordinal still has a live pod. The dead reservation stood forever with the invalidated
// gauge and its alert latched, when one statement (drop, then create) repairs it.
func TestReconcileSlotsRecyclesAnInvalidatedSlotForALiveStandby(t *testing.T) {
	ex := &slotExec{}
	a := newSlotTestAgent(t, ex, config.MechanismNative)
	a.cfg.NodeCount = 2
	observed := []pg.SlotState{{Name: "pg_ha_slot_1", Active: false, WALStatus: "lost"}}
	a.reconcileSlots(context.Background(), observed)
	if len(ex.created) != 1 || ex.created[0] != "pg_ha_slot_1" {
		t.Errorf("created = %v, want [pg_ha_slot_1] recycled", ex.created)
	}
}

// ... but only when nothing holds it. An invalidated slot that is still active is left to
// its holder; the drop predicate inside the create statement is the atomic backstop.
func TestReconcileSlotsLeavesAnActiveInvalidatedSlotAlone(t *testing.T) {
	ex := &slotExec{}
	a := newSlotTestAgent(t, ex, config.MechanismNative)
	a.cfg.NodeCount = 2
	observed := []pg.SlotState{{Name: "pg_ha_slot_1", Active: true, WALStatus: "lost"}}
	a.reconcileSlots(context.Background(), observed)
	if len(ex.created) != 0 {
		t.Errorf("created = %v, want none: the slot is held", ex.created)
	}
}

// A recycle whose CREATE fails must not leave the slot in the owned set (#298 review). The
// recycle's drop and create are two statements in one psql call and pg_drop_replication_slot is
// not transactional, so a failure between them removes the slot -- while ownedStandbySlots
// derives #308's synchronized_standby_slots from the PRE-PASS snapshot, which still lists it.
// Naming a slot that does not exist is the one thing that GUC must never do: the primary then
// refuses to release WAL and logs about it every checkpoint.
func TestReconcileSlotsDropsAFailedRecycleFromTheOwnedSet(t *testing.T) {
	observed := []pg.SlotState{{Name: "pg_ha_slot_1", Active: false, WALStatus: "lost"}}

	ex := &slotExec{createErr: errors.New("all replication slots are in use")}
	a := newSlotTestAgent(t, ex, config.MechanismNative)
	a.cfg.NodeCount = 2
	owned, ok := a.reconcileSlots(context.Background(), observed)
	if !ok {
		t.Fatal("the live pod set is readable here; reconcileSlots must still return an answer")
	}
	if len(owned) != 0 {
		t.Errorf("owned = %v, want none: the failed recycle may have removed the slot", owned)
	}

	// ...and the filter is not over-broad: a recycle that SUCCEEDED leaves a usable slot, which
	// must stay in the owned set or the GUC churns off a live standby every time one is repaired.
	okEx := &slotExec{}
	b := newSlotTestAgent(t, okEx, config.MechanismNative)
	b.cfg.NodeCount = 2
	owned, ok = b.reconcileSlots(context.Background(), observed)
	if !ok || len(owned) != 1 || owned[0] != "pg_ha_slot_1" {
		t.Errorf("owned = %v (ok=%v), want [pg_ha_slot_1] after a successful recycle", owned, ok)
	}
}

func TestStandbyReclaimIsCascadeAware(t *testing.T) {
	// Without cascade a standby cannot be anyone's upstream, so every agent-minted slot found
	// locally is a leftover. With cascade on, its children's slots live here and must survive
	// -- reclaiming them every tick would delete exactly what cascading depends on (#294).
	a := &agent{cfg: &config.Config{PodName: "pg-0"}}
	live := map[int]bool{1: true, 2: true}

	a.cfg.CascadeReplication = false
	if !a.reclaimableOnStandby("pg_ha_slot_1", nil, nil, false) {
		t.Error("without cascade a standby's agent-minted slot is a leftover")
	}

	a.cfg.CascadeReplication = true
	if a.reclaimableOnStandby("pg_ha_slot_1", live, nil, false) {
		t.Error("with cascade on, a live child's slot must not be reclaimed")
	}
	if !a.reclaimableOnStandby("pg_ha_slot_7", live, nil, false) {
		t.Error("with cascade on, a slot whose pod is gone is still reclaimable")
	}
	// This node's own slot is never used by anyone locally, on either setting.
	if !a.reclaimableOnStandby("pg_ha_slot_0", live, nil, false) {
		t.Error("self's own slot is always reclaimable locally")
	}
	// An operator's slot stays out of reach regardless.
	if a.reclaimableOnStandby("operator_slot", live, nil, false) {
		t.Error("an operator's slot must never be reclaimable")
	}
}

func TestReleaseSlotOnFormerUpstream(t *testing.T) {
	// #294 live-cluster finding: a cascaded standby clones from the PRIMARY (provisioning its
	// slot there) and then re-homes onto an intermediate, leaving an inactive WAL-retaining
	// slot on the primary that the primary's own conservative policy will never reclaim. The
	// owner releases it, because only the owner knows it stopped using it.
	newAgent := func(cascade bool) (*agent, *slotExec) {
		ex := &slotExec{}
		a := newSlotTestAgent(t, ex, config.MechanismNative)
		a.cfg.CascadeReplication = cascade
		return a, ex
	}

	// Re-homed under cascade: the old upstream's copy of this node's slot goes.
	a, ex := newAgent(true)
	a.releaseSlotOnFormerUpstream(context.Background(), "pg-1", "pg-2")
	if len(ex.dropped) != 1 || ex.dropped[0] != slotNameFor(a.cfg.PodName) {
		t.Errorf("dropped = %v, want [%s]", ex.dropped, slotNameFor(a.cfg.PodName))
	}

	// Without cascade a re-home is a failover: the old upstream is the demoted ex-primary,
	// standbySlotsTick already reclaims there, and reaching for a dying node would add a
	// timeout to every failover.
	a, ex = newAgent(false)
	a.releaseSlotOnFormerUpstream(context.Background(), "pg-1", "pg-2")
	if len(ex.dropped) != 0 {
		t.Errorf("dropped %v without cascading replication", ex.dropped)
	}

	// No-ops that must not issue a drop at all.
	for _, tc := range []struct{ name, former, target string }{
		{"first follow (no former upstream)", "", "pg-2"},
		{"same upstream re-confirmed", "pg-1", "pg-1"},
		{"former upstream is self", "pg-0", "pg-2"},
	} {
		a, ex = newAgent(true)
		a.releaseSlotOnFormerUpstream(context.Background(), tc.former, tc.target)
		if len(ex.dropped) != 0 {
			t.Errorf("%s: dropped %v, want none", tc.name, ex.dropped)
		}
	}
}

func TestBootstrapCloneLatchesTheCloneSourceAsUpstream(t *testing.T) {
	// #294, second live finding: Clone provisions this node's slot ON THE SOURCE (always the
	// lease holder). Under cascading replication the node then re-homes onto an intermediate,
	// stranding that slot -- and releaseSlotOnFormerUpstream can only reclaim it if the clone
	// source was latched, because it reads followUpstream. Observed live: a node whose FIRST
	// post-clone Follow was already the cascade hop left an inactive slot on the primary,
	// while one that transited the leader first was cleaned up.
	ex := &scriptedExec{}
	a := newFollowTestAgent(t, ex)
	a.cfg.PodName = "pg-2"
	a.followUpstream = "" // act() clears it for every non-Follow action, including BootstrapClone
	if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.BootstrapClone, Target: "pg-0"}, reconcile.Observation{}); err != nil {
		t.Fatalf("bootstrap clone: %v", err)
	}
	if a.followUpstream != "pg-0" {
		t.Errorf("followUpstream = %q after cloning from pg-0, want \"pg-0\" -- the release path reads this", a.followUpstream)
	}
}

func TestLatchFollowRestartsTheStallWindow(t *testing.T) {
	// #294 review: a counter that carries across a repoint makes the ordinary failover
	// sequence -- primary dies, this standby climbs past standbyStallTicks while no peer is on
	// a newer timeline, then a peer promotes -- arm StandbyStalled on the very FIRST tick
	// after the Follow, turning a standby that would have streamed seconds later into a
	// pg_rewind and possibly a full re-clone.
	a := &agent{cfg: &config.Config{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a.standbyNoReceiverTicks = standbyStallTicks + 5
	a.standbyLastProgressLSN = pg.LSN{Hi: 1, Lo: 2}
	a.latchFollow("pg-1")
	if a.standbyNoReceiverTicks != 0 {
		t.Errorf("standbyNoReceiverTicks = %d after a repoint, want 0", a.standbyNoReceiverTicks)
	}
	if a.standbyLastProgressLSN != (pg.LSN{}) {
		t.Errorf("standbyLastProgressLSN = %v after a repoint, want zero", a.standbyLastProgressLSN)
	}
	if a.followUpstream != "pg-1" {
		t.Errorf("followUpstream = %q, want pg-1", a.followUpstream)
	}
}

func TestSlotsTickFailsClosedWhenThePodListFails(t *testing.T) {
	// #294 review: reconcileSlots returned a bare nil when livePodOrdinals failed, which
	// syncSlotCandidates could not tell apart from "this primary owns no standby slots" -- so a
	// single apiserver blip on a healthy 3-node cluster ran
	// `ALTER SYSTEM SET synchronized_standby_slots = ''` plus a reload, dropping #308's
	// guarantee and reinstating it the next tick. The pre-#294 code failed closed here.
	ex := &slotExec{rows: "pg_ha_slot_1|t|0|reserved|t\n"}
	a := newSlotTestAgent(t, ex, config.MechanismNative)
	a.cfg.NodeCount = 2
	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	a.kube = k8s.NewWithClient(cs, "ns")

	_, owned, ok := a.slotsTick(context.Background())
	if ok {
		t.Fatal("slotsTick must report the owned set as untrustworthy when the pod list fails")
	}
	if owned != nil {
		t.Errorf("owned = %v, want nil", owned)
	}
}

func TestAssertSyncStandbySlotsLeavesTheGUCAloneOnAnUntrustworthyInput(t *testing.T) {
	// The other half of the same fix: an untrustworthy owned set must not be written.
	ex := &scriptedExec{}
	a := newFollowTestAgent(t, ex)
	a.cfg.Mechanism = config.MechanismNative
	a.cfg.SyncReplicationSlots = true
	a.assertSyncStandbySlots(context.Background(), nil, nil, false)
	if len(ex.slotSyncSQL) != 0 {
		t.Errorf("wrote synchronized_standby_slots on an untrustworthy input: %v", ex.slotSyncSQL)
	}
	if a.lastSyncStandbySlots != nil {
		t.Error("the cache must not be updated from an input that was never applied")
	}
}

// #298, found by the first live run of the repmgrd->2.0.0 roll: stripping repmgr's
// primary_conninfo must CARRY IT FORWARD into the agent's fragment, not orphan the standby. The
// roll replaces the highest ordinal first, so that pod is the only agent in the cluster while the
// real primary is still a 1.x repmgrd pod holding no lease -- Decide returns Wait ("no known
// leader"), nothing writes the fragment, and the node is left with standby.signal and no upstream
// at all: never streams, never Ready, rollout stops with the cluster half migrated.
func TestMigrateForeignRecoveryConfigCarriesTheUpstreamForward(t *testing.T) {
	dir := t.TempDir()
	a := newPreloadTestAgent(t, config.MechanismNative, dir, filepath.Join(dir, "repmgr.so"))
	// The seeding goes through the mechanism, so the harness needs a real one.
	a.mech = mechanism.NewNative(dir, "/usr/lib/postgresql/18/bin", "pw", "pg_ha_slot_2", "pgm-2")
	auto := filepath.Join(dir, "postgresql.auto.conf")
	if err := os.WriteFile(auto, []byte(
		"# Do not edit this file manually!\nprimary_conninfo = 'host=pgm-0 port=5432 user=repmgr'\nprimary_slot_name = 'repmgr_slot_2'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// This node is a STANDBY: the carry-forward is gated on standby.signal, because a promoted
	// node keeps a STALE primary_conninfo in auto.conf that must NOT be seeded (#298 review).
	if err := os.WriteFile(filepath.Join(dir, "standby.signal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.migrateForeignRecoveryConfig(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Removed from auto.conf, as before.
	b, err := os.ReadFile(auto)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "primary_conninfo") {
		t.Errorf("repmgr's primary_conninfo must be removed from auto.conf:\n%s", b)
	}
	// ...and present in the agent's own fragment, which is what keeps the node streaming.
	frag, err := os.ReadFile(filepath.Join(dir, "pg-ha-agent.conf"))
	if err != nil {
		t.Fatalf("the agent fragment must exist after the migration: %v", err)
	}
	if !strings.Contains(string(frag), "host=pgm-0") {
		t.Errorf("the inherited upstream was not carried into the agent fragment:\n%s", frag)
	}
	// repmgr's OWN slot, which exists on the still-repmgrd primary -- NOT this agent's ordinal
	// slot, which does not. A walreceiver pointed at a missing slot never streams, so seeding
	// pg_ha_slot_2 here would stall the rollout exactly as the missing conninfo did (#298).
	//
	// This asserts THIS FUNCTION's output only, and holds for a window measured in milliseconds
	// (#298 review): boot() runs GenerateConfig right after, and writeManagedConf then rewrites
	// primary_slot_name to pg_ha_slot_2 unconditionally. That is why the pre-create of
	// pg_ha_slot_2 on the upstream is retried and loud rather than best-effort -- the seeded
	// repmgr slot is NOT a durable fallback, so do not read this assertion as one.
	if !strings.Contains(string(frag), "primary_slot_name = 'repmgr_slot_2'") {
		t.Errorf("the inherited slot was not carried forward:\n%s", frag)
	}
	if strings.Contains(string(frag), "pg_ha_slot_2") {
		t.Errorf("seeded this agent's own slot, which does not exist on a repmgrd primary:\n%s", frag)
	}
}

// #298 review: a PRIMARY's data directory also carries repmgr's primary_conninfo -- PostgreSQL
// does not strip it on promotion -- and it points at whoever this node followed BEFORE it was
// promoted. Seeding that dead upstream, and pre-creating this node's slot on a peer that is a
// standby by now, would leave a permanently inactive slot pinning WAL on a node whose
// reconcileSlots only runs while it is primary. standby.signal is the gate.
func TestMigrateForeignRecoveryConfigDoesNotCarryAPrimarysStaleUpstream(t *testing.T) {
	dir := t.TempDir()
	a := newPreloadTestAgent(t, config.MechanismNative, dir, filepath.Join(dir, "repmgr.so"))
	a.mech = mechanism.NewNative(dir, "/usr/lib/postgresql/18/bin", "pw", "pg_ha_slot_0", "pgm-0")
	auto := filepath.Join(dir, "postgresql.auto.conf")
	if err := os.WriteFile(auto, []byte(
		"primary_conninfo = 'host=pgm-1 port=5432 user=repmgr'\nprimary_slot_name = 'repmgr_slot_1'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// No standby.signal: this data directory belongs to a primary.
	if err := a.migrateForeignRecoveryConfig(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	b, err := os.ReadFile(auto)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "primary_conninfo") {
		t.Errorf("the stale settings must still be removed from auto.conf:\n%s", b)
	}
	if frag, err := os.ReadFile(filepath.Join(dir, "pg-ha-agent.conf")); err == nil {
		if strings.Contains(string(frag), "host=pgm-1") {
			t.Errorf("a primary's stale upstream was seeded into the agent fragment:\n%s", frag)
		}
	}
}

func TestMigrateForeignRecoveryConfig(t *testing.T) {
	// #292: a 1.x volume carries repmgr's primary_conninfo and primary_slot_name in
	// postgresql.auto.conf, which PostgreSQL reads after every include and which therefore
	// outranks the agent's fragment. #294 could only refuse to start on it; this migrates it.
	dir := t.TempDir()
	a := newPreloadTestAgent(t, config.MechanismNative, dir, filepath.Join(dir, "repmgr.so"))

	// A fresh install has no auto.conf at all.
	if err := a.migrateForeignRecoveryConfig(); err != nil {
		t.Fatalf("a fresh install must not be touched: %v", err)
	}

	// Only the agent's own ALTER SYSTEM (#308): nothing to migrate, nothing lost.
	auto := filepath.Join(dir, "postgresql.auto.conf")
	own := "synchronized_standby_slots = 'pg_ha_slot_1'\n"
	if err := os.WriteFile(auto, []byte(own), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.migrateForeignRecoveryConfig(); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, auto); got != own {
		t.Errorf("the agent's own ALTER SYSTEM was disturbed: %q", got)
	}

	// The real case: repmgr-shaped. Both settings go; the agent's own survives.
	if err := os.WriteFile(auto, []byte(own+
		"primary_conninfo = 'host=''pg-0.h'' user=repmgr'\n"+
		"primary_slot_name = 'repmgr_slot_1001'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.migrateForeignRecoveryConfig(); err != nil {
		t.Fatalf("the migration must succeed on a repmgr-shaped volume: %v", err)
	}
	got := mustRead(t, auto)
	for _, gone := range []string{"primary_conninfo", "primary_slot_name"} {
		if strings.Contains(got, gone) {
			t.Errorf("%s survived the migration:\n%s", gone, got)
		}
	}
	if !strings.Contains(got, "synchronized_standby_slots") {
		t.Errorf("the agent's own ALTER SYSTEM was lost:\n%s", got)
	}

	// Idempotent: booting again finds nothing to do.
	if err := a.migrateForeignRecoveryConfig(); err != nil {
		t.Fatalf("second boot: %v", err)
	}
	if after := mustRead(t, auto); after != got {
		t.Error("a second boot rewrote the file")
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPausedAgentKeepsTheSlotGauges(t *testing.T) {
	// #294 review: pause routes through Decide's NoOp, which fell into act()'s retract branch
	// and cleared the slot gauges every tick -- silencing all three slot alerts and resetting
	// their `for:` clocks on a paused primary that is still serving and still retaining WAL.
	ex := &slotExec{rows: "pg_ha_slot_1|f|4194304|reserved|t\n"}
	a := newSlotTestAgent(t, ex, config.MechanismNative)
	a.cfg.NodeCount = 2
	a.sup = process.NewSupervisor(&fakePostmaster{})
	a.dcs = &fakeDCS{}

	// Publish a slot observation, then take the NoOp path.
	if _, ok := a.observeSlots(context.Background()); !ok {
		t.Fatal("observeSlots failed")
	}
	before := scrapeMetrics(t, a)
	if !strings.Contains(before, "pg_ha_agent_replication_slots 1") {
		t.Fatalf("slot gauge was not published:\n%s", before)
	}
	if err := a.act(context.Background(), reconcile.Decision{Action: reconcile.NoOp}, reconcile.Observation{}); err != nil {
		t.Fatalf("act(NoOp): %v", err)
	}
	after := scrapeMetrics(t, a)
	if !strings.Contains(after, "pg_ha_agent_replication_slots 1") {
		t.Errorf("a pause retracted the slot gauges:\n%s", after)
	}
}

func TestLegacySlotReclaimWaitsForMigrationProof(t *testing.T) {
	// #292: the migration window is the dangerous moment. A live standby's legacy slot must
	// survive until its new-scheme slot is ACTIVE -- proof it moved -- because in between the
	// old slot is inactive and the new one may not exist, and dropping there lets the primary
	// recycle WAL the standby still needs.
	live := map[int]bool{0: true, 1: true}

	// Mid-restart: nothing proves pod-1 moved yet.
	if orphanSlot("repmgr_slot_1001", "pg-0", live, map[int]bool{}) {
		t.Error("a live standby's legacy slot must not be reclaimed before it has migrated")
	}
	// The new slot exists but is not yet carrying a stream (reconcileSlots pre-creates it), so
	// presence alone is not proof.
	notYet := migratedOrdinals([]pg.SlotState{{Name: "pg_ha_slot_1", Active: false, Reserving: true}})
	if orphanSlot("repmgr_slot_1001", "pg-0", live, notYet) {
		t.Error("a pre-created but inactive new slot is not proof of migration")
	}
	// Streaming through the new slot: the legacy one is provably finished with.
	moved := migratedOrdinals([]pg.SlotState{{Name: "pg_ha_slot_1", Active: true, Reserving: true}})
	if !orphanSlot("repmgr_slot_1001", "pg-0", live, moved) {
		t.Error("once the new slot is active the legacy one must be reclaimed")
	}
	// A departed pod needs no proof at all.
	if !orphanSlot("repmgr_slot_1009", "pg-0", live, map[int]bool{}) {
		t.Error("a legacy slot whose pod is gone must be reclaimed")
	}
	// And the primary's own legacy slot is always dead weight -- nobody streams from a node to
	// itself, so it can never earn migration proof and must not wait for any.
	if !orphanSlot("repmgr_slot_1000", "pg-0", live, map[int]bool{}) {
		t.Error("self's own legacy slot must be reclaimable without migration proof")
	}
}

func TestMigratedOrdinalsIgnoresLegacyAndForeignSlots(t *testing.T) {
	m := migratedOrdinals([]pg.SlotState{
		{Name: "pg_ha_slot_1", Active: true},
		{Name: "repmgr_slot_1002", Active: true}, // legacy: never evidence of migration
		{Name: "my_own_slot", Active: true},      // an operator's: not ours to read
		{Name: "pg_ha_slot_abc", Active: true},   // unparseable ordinal
	})
	if len(m) != 1 || !m[1] {
		t.Errorf("migratedOrdinals = %v, want only ordinal 1", m)
	}
}

func TestDemotedPrimaryReclaimsLegacySlotsUnderCascade(t *testing.T) {
	// #290 review: routing legacy slots through orphanSlot's migration gate pinned them forever
	// on a demoted ex-primary with cascade on. The gate asks whether the ordinal's new-scheme
	// slot is active HERE; the child has moved to the new primary, so it never is. The
	// discriminator is isUpstream=false -- nothing streams from this node, so a legacy slot on it
	// cannot be in use. (Round 2 corrected the overcorrection: declaring ALL legacy slots residue
	// broke the cascade-upstream case, which TestCascadeUpstreamKeepsAGrandchildsLegacySlot
	// pins.)
	a := &agent{cfg: &config.Config{PodName: "pg-0", CascadeReplication: true}}
	live := map[int]bool{0: true, 1: true}
	if !a.reclaimableOnStandby("repmgr_slot_1001", live, map[int]bool{}, false) {
		t.Error("a demoted primary must reclaim its leftover legacy slots even with cascade on")
	}
	// Cascade off behaves the same -- the point is that the two settings agree here.
	a.cfg.CascadeReplication = false
	if !a.reclaimableOnStandby("repmgr_slot_1001", live, map[int]bool{}, false) {
		t.Error("legacy slots are leftovers on a standby regardless of cascade")
	}
	// The agent-minted path keeps its cascade-aware protection: a live child's slot survives.
	a.cfg.CascadeReplication = true
	if a.reclaimableOnStandby("pg_ha_slot_1", live, map[int]bool{}, false) {
		t.Error("a live cascade child's own slot must still be protected on its upstream")
	}
}

func TestCascadeUpstreamKeepsAGrandchildsLegacySlot(t *testing.T) {
	// #290 review, round 2: with cascade on, a legacy slot on a standby has two possible owners
	// and they must not be conflated.
	a := &agent{cfg: &config.Config{PodName: "pg-1", CascadeReplication: true}}
	live := map[int]bool{0: true, 1: true, 2: true}

	// A cascade UPSTREAM: something streams from here, so a grandchild's repmgr_slot_<id> may be
	// the slot it is actually using mid-migration. Dropping it forces the re-clone the migration
	// exists to avoid.
	if a.reclaimableOnStandby("repmgr_slot_1002", live, map[int]bool{}, true) {
		t.Error("a cascade upstream must keep a live, unmigrated grandchild's legacy slot")
	}
	// Once that grandchild is provably streaming through its NEW slot, the legacy one goes.
	moved := migratedOrdinals([]pg.SlotState{{Name: "pg_ha_slot_2", Active: true}})
	if !a.reclaimableOnStandby("repmgr_slot_1002", live, moved, true) {
		t.Error("a migrated grandchild's legacy slot must be reclaimed")
	}
	// A DEMOTED ex-primary: nothing streams from here, so every legacy slot is residue --
	// otherwise it sits inactive pinning WAL on this volume forever.
	if !a.reclaimableOnStandby("repmgr_slot_1002", live, map[int]bool{}, false) {
		t.Error("a demoted ex-primary must reclaim its leftover legacy slots")
	}
	// And a departed pod needs no reasoning at all, either way.
	for _, up := range []bool{true, false} {
		if !a.reclaimableOnStandby("repmgr_slot_1009", live, map[int]bool{}, up) {
			t.Errorf("isUpstream=%v: a legacy slot whose pod is gone must be reclaimed", up)
		}
	}
}

// #298 review: a Wait or NoOp tick must NOT erase the follow latch. Both are "observe again,
// touch nothing" -- Decide's own reason for the reachable-standby Wait is "keep the current
// upstream" -- yet act() cleared followUpstream on every non-Follow action. One routine
// no-leader Wait tick mid-failover (the lease is briefly empty after ReleaseOnCancel) was
// enough to blank it, after which releaseSlotOnFormerUpstream had no former upstream to drop
// this node's slot on: an inactive, WAL-pinning slot stayed on the old cascade intermediate
// until max_slot_wal_keep_size invalidated it. cascadeFollowTarget's anti-thrash stickiness
// reads the same field and lost its hysteresis the same way.
func TestActWaitAndNoOpKeepTheFollowLatch(t *testing.T) {
	for _, action := range []reconcile.Action{reconcile.Wait, reconcile.NoOp} {
		ex := &scriptedExec{walRcv: "pg-0.h|streaming"}
		a := newFollowTestAgent(t, ex)
		a.followUpstream = "pg-1"
		if err := a.act(context.Background(), reconcile.Decision{Action: action}, reconcile.Observation{}); err != nil {
			t.Fatalf("act(%s): %v", action, err)
		}
		if a.followUpstream != "pg-1" {
			t.Errorf("act(%s) cleared the follow latch: got %q, want it kept as pg-1", action, a.followUpstream)
		}
	}
}

// ...while an action that genuinely ends this node's standby identity still clears it, so the
// next Follow re-points and re-registers rather than trusting a stale upstream.
func TestActPromoteStillClearsTheFollowLatch(t *testing.T) {
	ex := &scriptedExec{walRcv: "pg-0.h|streaming"}
	a := newFollowTestAgent(t, ex)
	a.followUpstream = "pg-1"
	// RestartLocal is the cheapest such action to drive against this fixture: it only stops
	// and starts the supervised (fake) postmaster, needing no apiserver client, and it is
	// unambiguously not a standby-preserving action.
	_ = a.act(context.Background(), reconcile.Decision{Action: reconcile.RestartLocal}, reconcile.Observation{})
	if a.followUpstream != "" {
		t.Errorf("a primary-role action must clear the follow latch, got %q", a.followUpstream)
	}
}

// rewindStubMech drives rejoinOnto's escalation branch: RejoinForceRewind returns the
// scripted errors in order (nil once the script runs out), and ReclonePreserving only
// records that it ran. A stub rather than Native-over-scriptedExec because what is under
// test is the CALLER's consecutive-failure accounting, not pg_rewind's classification --
// which native_test.go already covers against real pg_rewind output.
type rewindStubMech struct {
	rejoinErrs []error
	rejoins    int
	reclones   int
}

func (m *rewindStubMech) GenerateConfig(context.Context, mechanism.NodeIdentity, mechanism.ConfigOpts) error {
	return nil
}
func (m *rewindStubMech) Promote(context.Context) error                { return nil }
func (m *rewindStubMech) Follow(context.Context, mechanism.Conn) error { return nil }
func (m *rewindStubMech) Clone(context.Context, mechanism.Conn) error  { return nil }
func (m *rewindStubMech) ReclonePreserving(context.Context, mechanism.Conn) error {
	m.reclones++
	return nil
}
func (m *rewindStubMech) RejoinForceRewind(context.Context, mechanism.Conn) error {
	i := m.rejoins
	m.rejoins++
	if i < len(m.rejoinErrs) {
		return m.rejoinErrs[i]
	}
	return nil
}

// #298 review: a rewind that SUCCEEDS ends the non-divergence failure streak. Two failures
// then a success left the counter at 2, so the next unrelated blip against the same primary
// read as the third consecutive failure and bought a full ReclonePreserving -- the backstop
// firing for a transient error, which is precisely what the fail-safe classification exists
// to avoid. Six ticks here (fail, fail, ok, fail, fail, ok) must re-clone zero times.
func TestRejoinRewindBackstopResetsAfterASuccess(t *testing.T) {
	transient := errors.New("connection to server failed")
	m := &rewindStubMech{rejoinErrs: []error{transient, transient, nil, transient, transient, nil}}
	a := newFollowTestAgentWithPM(t, &scriptedExec{walRcv: ""}, &fakePostmaster{})
	a.mech = m
	for i, wantErr := range []bool{true, true, false, true, true, false} {
		err := a.rejoinOnto(context.Background(), "pg-1")
		if wantErr && err == nil {
			t.Fatalf("tick %d: a failed rewind must surface an error", i)
		}
		if !wantErr && err != nil {
			t.Fatalf("tick %d: rejoin: %v", i, err)
		}
	}
	if m.reclones != 0 {
		t.Fatalf("no streak reached %d consecutive failures, so nothing may re-clone; got %d re-clones", rewindFailureLimit, m.reclones)
	}
	if a.rewindFailures != 0 || a.rewindFailureTarget != "" {
		t.Fatalf("a successful rewind must clear the streak, got %d against %q", a.rewindFailures, a.rewindFailureTarget)
	}
}

// #298 review: an UNREACHABLE target is exempt from the backstop entirely. Escalating on it
// converges on nothing -- ReclonePreserving dials the same target with the same credentials, so
// it fails where the rewind just failed to connect, after renaming PGDATA aside and leaving an
// unreaped .diverged.<ts> copy on the PVC. Ten ticks of "could not connect" (a postmaster
// restarting, a pod name not yet propagated, a connect that times out) must re-clone zero times.
func TestRejoinRewindBackstopExemptsAnUnreachableTarget(t *testing.T) {
	unreachable := fmt.Errorf("%w: could not connect to server: Connection refused", mechanism.ErrRewindUnreachable)
	errs := make([]error, 10)
	for i := range errs {
		errs[i] = unreachable
	}
	m := &rewindStubMech{rejoinErrs: errs}
	a := newFollowTestAgentWithPM(t, &scriptedExec{walRcv: ""}, &fakePostmaster{})
	a.mech = m
	for i := range errs {
		if err := a.rejoinOnto(context.Background(), "pg-1"); err == nil {
			t.Fatalf("tick %d: an unreachable target must still surface an error", i)
		}
		if m.reclones != 0 {
			t.Fatalf("tick %d: escalated on an unreachable target; a re-clone cannot reach it either", i)
		}
	}
	// And the streak is not silently accumulating either -- an unreachable tick is no evidence
	// about a local refusal, so it must not push a later genuine one over the limit.
	if a.rewindFailures != 0 {
		t.Fatalf("unreachable ticks must not accumulate toward the backstop, got %d", a.rewindFailures)
	}
	// A genuine non-divergence refusal after all that still needs its full three ticks.
	local := errors.New("pg_rewind: error: something permanent but not divergence")
	m.rejoinErrs = append(m.rejoinErrs, local, local, local)
	for i := 0; i < 2; i++ {
		if err := a.rejoinOnto(context.Background(), "pg-1"); err == nil {
			t.Fatalf("local refusal tick %d: expected an error", i)
		}
		if m.reclones != 0 {
			t.Fatalf("local refusal tick %d: escalated before %d failures", i, rewindFailureLimit)
		}
	}
	if err := a.rejoinOnto(context.Background(), "pg-1"); err != nil {
		t.Fatalf("the escalating tick must recover, got %v", err)
	}
	if m.reclones != 1 {
		t.Fatalf("a genuine streak of %d must still escalate once, got %d", rewindFailureLimit, m.reclones)
	}
}

// The backstop still fires when the failures really are consecutive: three in a row against
// one target escalates to a data-preserving re-clone rather than retrying forever, and the
// counter resets so the next stall starts a fresh streak.
func TestRejoinRewindBackstopFiresOnThreeConsecutiveFailures(t *testing.T) {
	transient := errors.New("pg_rewind: error: something permanent but not divergence")
	m := &rewindStubMech{rejoinErrs: []error{transient, transient, transient}}
	a := newFollowTestAgentWithPM(t, &scriptedExec{walRcv: ""}, &fakePostmaster{})
	a.mech = m
	for i := 0; i < 2; i++ {
		if err := a.rejoinOnto(context.Background(), "pg-1"); err == nil {
			t.Fatalf("tick %d: a failed rewind must surface an error", i)
		}
		if m.reclones != 0 {
			t.Fatalf("tick %d: escalated before %d failures", i, rewindFailureLimit)
		}
	}
	if err := a.rejoinOnto(context.Background(), "pg-1"); err != nil {
		t.Fatalf("the escalating tick must recover, got %v", err)
	}
	if m.reclones != 1 {
		t.Fatalf("the %dth consecutive failure must re-clone once, got %d", rewindFailureLimit, m.reclones)
	}
	if a.rewindFailures != 0 || a.rewindFailureTarget != "" {
		t.Fatalf("escalating must clear the streak, got %d against %q", a.rewindFailures, a.rewindFailureTarget)
	}
}

// A failure against a DIFFERENT target restarts the count: the streak is per-target, so a
// leader change mid-stall must not let two unrelated primaries' failures add up to an
// escalation neither of them earned.
func TestRejoinRewindBackstopIsPerTarget(t *testing.T) {
	transient := errors.New("connection to server failed")
	m := &rewindStubMech{rejoinErrs: []error{transient, transient, transient, transient}}
	a := newFollowTestAgentWithPM(t, &scriptedExec{walRcv: ""}, &fakePostmaster{})
	a.mech = m
	for _, target := range []string{"pg-1", "pg-1", "pg-2", "pg-2"} {
		if err := a.rejoinOnto(context.Background(), target); err == nil {
			t.Fatalf("target %s: a failed rewind must surface an error", target)
		}
	}
	if m.reclones != 0 {
		t.Fatalf("two failures against each of two targets must not escalate, got %d re-clones", m.reclones)
	}
	if a.rewindFailures != 2 || a.rewindFailureTarget != "pg-2" {
		t.Fatalf("the streak must follow the latest target, got %d against %q", a.rewindFailures, a.rewindFailureTarget)
	}
}

// #298 review: the legacy slot name -> ordinal mapping is bounded at the top, not just the
// bottom. Unbounded, an operator's hand-made `repmgr_slot_9999` mapped to ordinal 8999 --
// an ordinal no pod can ever hold, so orphanSlot read it as a DEPARTED node and made someone
// else's slot reclaimable. "Self-healing" is no comfort when the healing is a dropped slot.
func TestSlotOrdinalBoundsTheLegacyMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int
		ok   bool
		why  string
	}{
		{"repmgr_slot_1000", 0, true, "the first node_id the chart mints"},
		{"repmgr_slot_1009", 9, true, "a plausible ordinal"},
		{"repmgr_slot_1999", 999, true, "the largest representable ordinal"},
		{"repmgr_slot_2000", 0, false, "past the base range: node_ids would stop being unique"},
		{"repmgr_slot_9999", 0, false, "somebody else's slot, not ordinal 8999"},
		{"repmgr_slot_999", 0, false, "below the base: not a node_id this chart mints"},
		{"pg_ha_slot_7", 7, true, "the native scheme is ordinal-native and needs no offset"},
		{"my_own_slot", 0, false, "not a name this agent minted"},
	} {
		got, ok := slotOrdinal(tc.name)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("slotOrdinal(%q) = (%d, %v), want (%d, %v) -- %s", tc.name, got, ok, tc.want, tc.ok, tc.why)
		}
	}
}

// The bound has to hold through orphanSlot, which is where an out-of-range mapping did damage:
// a stranger's slot must never look departed, whatever the live pod set says.
func TestOrphanSlotLeavesOutOfRangeLegacyNamesAlone(t *testing.T) {
	live := map[int]bool{0: true, 1: true}
	for _, name := range []string{"repmgr_slot_9999", "repmgr_slot_2000"} {
		if orphanSlot(name, "pg-0", live, nil) {
			t.Errorf("%s is not this chart's slot and must never be reclaimable", name)
		}
		if orphanSlot(name, "pg-0", live, map[int]bool{8999: true, 1000: true}) {
			t.Errorf("%s must stay out of reach even with migration proof for the mapped ordinal", name)
		}
	}
}

// blockingExec makes every probe take real time and reports the peer unreachable, which is
// the scenario that made sequential probing dangerous: a dead peer costs a full connect
// timeout. Its own type rather than scriptedExec, which mutates counters without a lock and
// would race under a fan-out.
type blockingExec struct{ onProbe func() }

func (b *blockingExec) Run(_ context.Context, _ []string, _ string, _ ...string) (string, error) {
	if b.onProbe != nil {
		b.onProbe()
	}
	return "", errors.New("connection to server failed")
}

// #298 review: peers are probed concurrently, and the resulting slice must still be ordered
// by ordinal. Sequential probing made a tick cost (dead peers) x (connect timeout), which on
// a partitioned cluster exceeded the /healthz staleness window and got the agent
// liveness-killed. Both properties are asserted together because fixing the first by
// appending results as they arrive would silently break the second, and peer ORDER feeds the
// promote-distance ranking -- two identical clusters must not disagree about it.
func TestObservePeersAreProbedConcurrentlyAndStayOrdered(t *testing.T) {
	const peers = 4
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	ex := &blockingExec{onProbe: func() {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		time.Sleep(60 * time.Millisecond) // stands in for a connect that will time out
		mu.Lock()
		inFlight--
		mu.Unlock()
	}}
	a := &agent{
		cfg: &config.Config{
			PGDATA: t.TempDir(), HeadlessService: "h", PodName: "pg-0", NodeCount: peers + 1,
			RepmgrUser: "repmgr", RepmgrDB: "repmgr", RenewDeadline: 2 * time.Second,
		},
		base:   "pg",
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		dcs:    &fakeDCS{}, // not the holder, so the gossip read is skipped entirely
		kube:   k8s.NewWithClient(k8sfake.NewSimpleClientset(), "ns"),
		prober: &pg.Prober{Exec: ex, Timeout: time.Second},
		sup:    process.NewSupervisor(&fakePostmaster{}),
		metr:   observe.New(),
		health: &selfHealthTracker{grace: 15 * time.Second},
	}

	start := time.Now()
	o := a.observe(context.Background())
	elapsed := time.Since(start)

	if maxInFlight < 2 {
		t.Errorf("probes ran sequentially (max in flight = %d); a partitioned cluster would spend peers x connect-timeout per tick", maxInFlight)
	}
	// Four 60ms probes plus the local one: ~120ms concurrent, ~300ms sequential. Loose on
	// purpose -- the assertion above is the precise one.
	if elapsed > 250*time.Millisecond {
		t.Errorf("observe took %v for %d peers; concurrent probing should be about one probe deep", elapsed, peers)
	}
	var got []string
	for _, p := range o.Peers {
		got = append(got, p.Name)
	}
	want := []string{"pg-1", "pg-2", "pg-3", "pg-4"}
	if len(got) != len(want) {
		t.Fatalf("peers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("peer order = %v, want %v (ordinal order, not completion order)", got, want)
		}
	}
}

// --- #335: server TLS is verified from the postmaster, not from the rendered config ---

// tlsExec answers `SHOW ssl` with a scripted value (or a failure) and counts the calls, so a
// test can assert both the verdict and that the throttle actually throttles.
type tlsExec struct {
	ssl   string
	err   error
	calls int
}

func (e *tlsExec) Run(_ context.Context, _ []string, name string, args ...string) (string, error) {
	if name == "psql" && strings.Contains(strings.Join(args, " "), "SHOW ssl") {
		e.calls++
		return e.ssl, e.err
	}
	return "", nil
}

func newTLSTestAgent(ex *tlsExec, tlsEnabled bool) *agent {
	return &agent{
		cfg: &config.Config{
			PodName: "pg-0", Namespace: "ns", TLSEnabled: tlsEnabled,
			RepmgrUser: "repmgr", RepmgrDB: "repmgr", RepmgrPassword: "pw",
		},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		prober: &pg.Prober{Exec: ex, Timeout: time.Second},
		metr:   observe.New(),
	}
}

// The whole point of #335 is that a server reporting `ssl = off` while the operator asked for
// TLS must become loud. The gauge is the channel an alert can page on.
func TestVerifyTLSActiveRaisesTheGaugeOnPlaintext(t *testing.T) {
	a := newTLSTestAgent(&tlsExec{ssl: "off"}, true)
	a.verifyTLSActive(context.Background())
	if !strings.Contains(scrapeMetrics(t, a), "pg_ha_agent_tls_inactive 1") {
		t.Errorf("a plaintext server must publish tls_inactive=1:\n%s", scrapeMetrics(t, a))
	}
}

func TestVerifyTLSActiveClearsTheGaugeWhenTLSIsOn(t *testing.T) {
	a := newTLSTestAgent(&tlsExec{ssl: "on"}, true)
	a.verifyTLSActive(context.Background())
	if !strings.Contains(scrapeMetrics(t, a), "pg_ha_agent_tls_inactive 0") {
		t.Errorf("a TLS server must publish tls_inactive=0:\n%s", scrapeMetrics(t, a))
	}
}

// A failed query is uncertainty, not evidence of plaintext. Treating it as evidence would fire
// the alarm every time the server is busy or mid-restart, which is how an alarm stops meaning
// anything -- so the previous verdict must survive it.
func TestVerifyTLSActiveKeepsThePreviousVerdictOnAQueryFailure(t *testing.T) {
	ex := &tlsExec{ssl: "off"}
	a := newTLSTestAgent(ex, true)
	a.verifyTLSActive(context.Background())
	ex.err = errors.New("server closed the connection")
	a.tlsCheckedAt = time.Time{} // re-arm the throttle
	a.verifyTLSActive(context.Background())
	if !strings.Contains(scrapeMetrics(t, a), "pg_ha_agent_tls_inactive 1") {
		t.Errorf("an unreachable server must not clear a standing TLS alarm:\n%s", scrapeMetrics(t, a))
	}
}

// Nothing at all when TLS was never requested: no probe, and a gauge that stays 0 so a
// release without TLS cannot contribute to an alert that aggregates across the fleet.
func TestVerifyTLSActiveIsInertWhenTLSWasNeverRequested(t *testing.T) {
	ex := &tlsExec{ssl: "off"}
	a := newTLSTestAgent(ex, false)
	a.verifyTLSActive(context.Background())
	if ex.calls != 0 {
		t.Errorf("calls = %d, want no probe at all when postgresql.tls.enabled is unset", ex.calls)
	}
	if !strings.Contains(scrapeMetrics(t, a), "pg_ha_agent_tls_inactive 0") {
		t.Errorf("tls_inactive must stay 0 when TLS was never requested:\n%s", scrapeMetrics(t, a))
	}
}

// tick() calls this on every reconcile (5s on chart defaults); the throttle is what keeps that
// from being a psql per tick forever, for a value that only changes across a reload.
func TestVerifyTLSActiveThrottlesRepeatedProbes(t *testing.T) {
	ex := &tlsExec{ssl: "on"}
	a := newTLSTestAgent(ex, true)
	for i := 0; i < 5; i++ {
		a.verifyTLSActive(context.Background())
	}
	if ex.calls != 1 {
		t.Errorf("calls = %d, want 1 within one tlsVerifyInterval", ex.calls)
	}
	a.tlsCheckedAt = time.Now().Add(-2 * tlsVerifyInterval)
	a.verifyTLSActive(context.Background())
	if ex.calls != 2 {
		t.Errorf("calls = %d, want a re-probe once the interval has elapsed", ex.calls)
	}
}

// --- #288: what the streaming-replica count may and may not include ---

// pg_basebackup -X stream opens a SECOND streaming connection for the whole duration
// of every clone -- fresh install, scale-up, re-clone -- and counting it inflates
// pg_ha_agent_replicas_streaming while the pod it belongs to is not replicating at all.
// The gauge feeds a replica-shortfall alert, so an inflated count is the direction that
// hides a real problem.
func TestCountStreamingReplicasExcludesCloneConnections(t *testing.T) {
	rows := []pg.ReplicaRow{
		{AppName: "pg-1", SlotName: "pg_ha_slot_1", State: "streaming"},
		{AppName: "pg-2", SlotName: "pg_ha_slot_2", State: "streaming"},
		// The clone in progress: streaming, but not a standby.
		{AppName: "pg_basebackup", SlotName: "pg_ha_slot_3", State: "streaming"},
	}
	if got := countStreamingReplicas(rows); got != 2 {
		t.Errorf("countStreamingReplicas = %d, want 2 (the base backup is not a replica)", got)
	}
}

// Only "streaming" counts. A standby in catchup exists but cannot yet serve or be
// promoted safely, and a backup/startup row is not a standby at all -- treating any of
// them as caught up would report a redundancy the cluster does not have.
func TestCountStreamingReplicasCountsOnlyTheStreamingState(t *testing.T) {
	rows := []pg.ReplicaRow{
		{AppName: "pg-1", State: "streaming"},
		{AppName: "pg-2", State: "catchup"},
		{AppName: "pg-3", State: "startup"},
		{AppName: "pg-4", State: "backup"},
		{AppName: "pg-5", State: ""},
	}
	if got := countStreamingReplicas(rows); got != 1 {
		t.Errorf("countStreamingReplicas = %d, want 1", got)
	}
	if got := countStreamingReplicas(nil); got != 0 {
		t.Errorf("countStreamingReplicas(nil) = %d, want 0", got)
	}
}

// --- gossip freshness ---

// A peer's gossiped status is only usable while it is RECENT: survivor ranking reads
// LSNs from it, and acting on a stale position is how a node that has since fallen
// behind gets promoted. The window is 4 reconcile intervals, wide enough that one
// missed tick does not blind the leader.
func TestGossipFreshWindow(t *testing.T) {
	a := newTestAgent(t, &fakePostmaster{}, &fakeDCS{})
	a.cfg.ReconcileInterval = 5 * time.Second
	a.cfg.RenewDeadline = 10 * time.Second
	now := time.Now().Unix()
	for _, c := range []struct {
		why   string
		at    int64
		fresh bool
	}{
		{"just published", now, true},
		{"one interval old", now - 5, true},
		{"at the edge of the window", now - 20, true},
		{"past the window", now - 21, false},
		{"long stale", now - 3600, false},
		// A pod that has never gossiped carries the zero value, which must never be
		// read as "published at the epoch and therefore ancient-but-parseable".
		{"never published", 0, false},
	} {
		if got := a.gossipFresh(k8s.NodeStatus{UpdatedAtUnix: c.at}); got != c.fresh {
			t.Errorf("%s: gossipFresh = %v, want %v", c.why, got, c.fresh)
		}
	}
}

// Pod clocks drift, and a peer whose clock runs slightly ahead publishes a timestamp
// in the FUTURE. That must stay usable within the renew deadline -- discarding it
// would blind the leader to the very peer it is ranking -- but a wildly future stamp
// (a badly wrong clock, or a forged annotation) must not be trusted indefinitely.
func TestGossipFreshToleratesBoundedClockSkew(t *testing.T) {
	a := newTestAgent(t, &fakePostmaster{}, &fakeDCS{})
	a.cfg.ReconcileInterval = 5 * time.Second
	a.cfg.RenewDeadline = 10 * time.Second
	now := time.Now().Unix()
	if !a.gossipFresh(k8s.NodeStatus{UpdatedAtUnix: now + 5}) {
		t.Error("a peer whose clock is a few seconds ahead must still be readable")
	}
	if a.gossipFresh(k8s.NodeStatus{UpdatedAtUnix: now + 3600}) {
		t.Error("an hour into the future is not clock skew; it must not be trusted")
	}
}

// --- assertPrimaryRouting: both halves, and what a partial failure leaves behind ---

// assertPrimaryRouting is what makes a promotion visible to clients: the write Service
// selector moves to this pod, and every pod's pg-role label is republished so
// service-readonly.yaml selects the right set. It is called on both Promote and
// StayPrimary, so it must be idempotent and self-healing.
func TestAssertPrimaryRoutingMovesTheWriteSelectorAndEveryLabel(t *testing.T) {
	mkPod := func(name, role string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns",
			Labels: map[string]string{"app.kubernetes.io/component": "postgresql", "pg-role": role},
		}}
	}
	cs := k8sfake.NewSimpleClientset(
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: "ns"},
			Spec:       corev1.ServiceSpec{Selector: map[string]string{"statefulset.kubernetes.io/pod-name": "pg-1"}},
		},
		mkPod("pg-0", "standby"), mkPod("pg-1", "primary"), mkPod("pg-2", "standby"),
	)
	a := newTestAgent(t, &fakePostmaster{}, &fakeDCS{})
	a.kube = k8s.NewWithClient(cs, "ns")
	a.cfg.MasterService = "pg"
	a.cfg.PodName = "pg-0"
	a.cfg.PodSelector = "app.kubernetes.io/component=postgresql"

	obs := reconcile.Observation{Peers: []reconcile.PeerState{
		{Name: "pg-1", Reachable: true, Role: pg.RoleStandby},
		{Name: "pg-2", Reachable: true, Role: pg.RoleStandby},
	}}
	if err := a.assertPrimaryRouting(context.Background(), obs); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	svc, _ := cs.CoreV1().Services("ns").Get(ctx, "pg", metav1.GetOptions{})
	if got := svc.Spec.Selector["statefulset.kubernetes.io/pod-name"]; got != "pg-0" {
		t.Errorf("write Service still points at %q: writes go to the old primary", got)
	}
	for name, want := range map[string]string{"pg-0": "primary", "pg-1": "standby", "pg-2": "standby"} {
		p, _ := cs.CoreV1().Pods("ns").Get(ctx, name, metav1.GetOptions{})
		if got := p.Labels["pg-role"]; got != want {
			t.Errorf("%s pg-role = %q, want %q", name, got, want)
		}
	}
}

// The write selector moves FIRST, and a failure there returns before any label is
// touched: publishing "pg-0 is primary" on the pods while the write Service still
// points elsewhere advertises a primary that receives no writes.
func TestAssertPrimaryRoutingStopsWhenTheWriteSelectorCannotMove(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: "ns"},
			Spec:       corev1.ServiceSpec{Selector: map[string]string{"statefulset.kubernetes.io/pod-name": "pg-1"}},
		},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "pg-0", Namespace: "ns",
			Labels: map[string]string{"app.kubernetes.io/component": "postgresql", "pg-role": "standby"},
		}},
	)
	cs.PrependReactor("patch", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("injected apiserver failure")
	})
	a := newTestAgent(t, &fakePostmaster{}, &fakeDCS{})
	a.kube = k8s.NewWithClient(cs, "ns")
	a.cfg.MasterService = "pg"
	a.cfg.PodName = "pg-0"
	a.cfg.PodSelector = "app.kubernetes.io/component=postgresql"

	if err := a.assertPrimaryRouting(context.Background(), reconcile.Observation{}); err == nil {
		t.Fatal("a failed write-selector patch must be reported")
	}
	p, _ := cs.CoreV1().Pods("ns").Get(context.Background(), "pg-0", metav1.GetOptions{})
	if p.Labels["pg-role"] == "primary" {
		t.Error("this pod was labelled primary while the write Service still points elsewhere")
	}
}

// An unreachable peer is OMITTED rather than classified, so ReconcilePodLabels leaves
// its label untouched: the primary cannot tell a node it cannot reach from a node that
// is fine but partitioned from it, and churning the label either way moves real client
// traffic on a guess.
func TestAssertPrimaryRoutingLeavesAnUnreachablePeerAlone(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: "ns"},
			Spec:       corev1.ServiceSpec{Selector: map[string]string{"statefulset.kubernetes.io/pod-name": "pg-0"}},
		},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "pg-1", Namespace: "ns",
			Labels: map[string]string{"app.kubernetes.io/component": "postgresql", "pg-role": "standby"},
		}},
	)
	a := newTestAgent(t, &fakePostmaster{}, &fakeDCS{})
	a.kube = k8s.NewWithClient(cs, "ns")
	a.cfg.MasterService = "pg"
	a.cfg.PodName = "pg-0"
	a.cfg.PodSelector = "app.kubernetes.io/component=postgresql"

	obs := reconcile.Observation{Peers: []reconcile.PeerState{{Name: "pg-1", Reachable: false}}}
	if err := a.assertPrimaryRouting(context.Background(), obs); err != nil {
		t.Fatal(err)
	}
	p, _ := cs.CoreV1().Pods("ns").Get(context.Background(), "pg-1", metav1.GetOptions{})
	if got := p.Labels["pg-role"]; got != "standby" {
		t.Errorf("an unreachable peer's label was rewritten to %q", got)
	}
}

// stopProvedDead separates Stop's two deadline outcomes (#298 review): the reaped SIGKILL
// (p.clear() ran, so Running() is false) is positive evidence the writer is gone, while the
// "leaving it supervised" arm -- SIGKILL undeliverable on a wedged PV -- is not. Both return a
// context.DeadlineExceeded-wrapping error, so err != nil cannot tell them apart, and the fence
// and shutdown paths used to read the first as "the demote did not complete": servingRW stayed
// set, SafeToRelease vetoed the release, and a peer waited out the whole LeaseDuration.
func TestStopProvedDead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		running bool
		want    bool
	}{
		{"clean stop", nil, false, true},
		{"deadline hit, killed and reaped", context.DeadlineExceeded, false, true},
		{"deadline hit, child still supervised", fmt.Errorf("leaving it supervised: %w", context.DeadlineExceeded), true, false},
		{"some other failure", errors.New("signal: operation not permitted"), false, false},
		// Not a deadline expiry, so it says nothing about the postmaster even with no child.
		{"other failure, child gone", errors.New("boom"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &agent{sup: process.NewSupervisor(&fakePostmaster{running: tc.running})}
			if got := a.stopProvedDead(tc.err); got != tc.want {
				t.Errorf("stopProvedDead(%v) with running=%v = %v, want %v", tc.err, tc.running, got, tc.want)
			}
		})
	}
}
