package mechanism

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestNative wires a Native against a fresh PGDATA containing a minimal
// postgresql.conf, mirroring what the chart's initdb step always leaves behind.
func newTestNative(t *testing.T, fr *fakeRunner) (*Native, string) {
	t.Helper()
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "postgresql.conf"), []byte("# initial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Native{Runner: fr, DataDir: dataDir, PGBindir: "/usr/lib/postgresql/18/bin", Password: "secret", Now: time.Now}, dataDir
}

func TestNativeBinResolvesUnderPGBindir(t *testing.T) {
	n := &Native{PGBindir: "/usr/lib/postgresql/18/bin"}
	if got := n.bin("pg_ctl"); got != "/usr/lib/postgresql/18/bin/pg_ctl" {
		t.Errorf("bin(pg_ctl) = %q, want versioned path (#269)", got)
	}
	// No PGBindir (e.g. a test or an image without PG_MAJOR): last resort is PATH.
	if got := (&Native{}).bin("pg_ctl"); got != "pg_ctl" {
		t.Errorf("bin(pg_ctl) with no PGBindir = %q, want bare name", got)
	}
}

func TestNativeGenerateConfigIsIdempotent(t *testing.T) {
	n, dataDir := newTestNative(t, &fakeRunner{})
	if err := n.GenerateConfig(context.Background(), NodeIdentity{}, ConfigOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := n.GenerateConfig(context.Background(), NodeIdentity{}, ConfigOpts{}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dataDir, "postgresql.conf"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if n := strings.Count(s, "include 'pg-ha-agent.conf'"); n != 1 {
		t.Errorf("postgresql.conf has %d include lines after two GenerateConfig calls, want exactly 1 (no duplicate accumulation)", n)
	}
	managed, err := os.ReadFile(filepath.Join(dataDir, managedConfName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(managed), "wal_log_hints = on") {
		t.Errorf("managed conf missing wal_log_hints (required for pg_rewind):\n%s", managed)
	}
	if strings.Contains(string(managed), "primary_conninfo") {
		t.Errorf("GenerateConfig must not write primary_conninfo -- Follow owns it:\n%s", managed)
	}
}

// GenerateConfig runs once per agent process boot (main.go's boot()) -- including every
// pod restart on an already-following standby, not just a fresh node. It must preserve
// whatever primary_conninfo Follow already wrote rather than reverting it to empty, or a
// routine pod restart would silently interrupt replication until the next reconcile tick's
// Follow re-establishes it.
func TestNativeGenerateConfigPreservesExistingConninfo(t *testing.T) {
	n, dataDir := newTestNative(t, &fakeRunner{})
	upstream := Conn{Host: "pg-1.h", Port: 5432, User: "repmgr"}
	if err := n.Follow(context.Background(), upstream); err != nil {
		t.Fatal(err)
	}
	// Simulate an agent process restart on the same PGDATA.
	if err := n.GenerateConfig(context.Background(), NodeIdentity{}, ConfigOpts{}); err != nil {
		t.Fatal(err)
	}
	managed, err := os.ReadFile(filepath.Join(dataDir, managedConfName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(managed), "pg-1.h") {
		t.Errorf("GenerateConfig dropped the existing primary_conninfo after a simulated restart:\n%s", managed)
	}
}

func TestNativePromote(t *testing.T) {
	fr := &fakeRunner{}
	n, dataDir := newTestNative(t, fr)
	if err := n.Promote(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fr.lastArgs(); !strings.Contains(got, "-D "+dataDir+" -w promote") {
		t.Errorf("Promote args = %q, want pg_ctl -D <datadir> -w promote", got)
	}
}

func TestNativeFollowRequiresHost(t *testing.T) {
	n, _ := newTestNative(t, &fakeRunner{})
	err := n.Follow(context.Background(), Conn{NodeID: 1001})
	if err == nil {
		t.Fatal("Follow with no Host must fail loudly rather than write a broken conninfo")
	}
}

func TestNativeFollowWritesConninfoAndStandbySignal(t *testing.T) {
	n, dataDir := newTestNative(t, &fakeRunner{})
	upstream := Conn{Host: "pg-0.headless", Port: 5432, User: "repmgr", Password: "s3cret"}
	if err := n.Follow(context.Background(), upstream); err != nil {
		t.Fatal(err)
	}
	managed, err := os.ReadFile(filepath.Join(dataDir, managedConfName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(managed), "primary_conninfo = '") || !strings.Contains(string(managed), "pg-0.headless") {
		t.Errorf("managed conf missing primary_conninfo for the upstream:\n%s", managed)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "standby.signal")); err != nil {
		t.Errorf("standby.signal not created: %v", err)
	}
}

func TestNativeCloneRequiresHost(t *testing.T) {
	n, _ := newTestNative(t, &fakeRunner{})
	if err := n.Clone(context.Background(), Conn{}); err == nil {
		t.Fatal("Clone with no source.Host must fail loudly")
	}
}

func TestNativeCloneArgs(t *testing.T) {
	fr := &fakeRunner{}
	n, dataDir := newTestNative(t, fr)
	source := Conn{Host: "pg-0.h", Port: 5432, User: "repmgr", DB: "repmgr"}
	if err := n.Clone(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	got := fr.lastArgs()
	for _, want := range []string{"-h pg-0.h", "-D " + dataDir, "-X stream", "--checkpoint=fast", "--no-password"} {
		if !strings.Contains(got, want) {
			t.Errorf("pg_basebackup args %q missing %q", got, want)
		}
	}
	// No -R: Clone calls Follow itself instead (below), so primary_conninfo/standby.signal
	// go through the one managed-fragment path Follow/GenerateConfig also use, rather than
	// postgresql.auto.conf, which would silently outrank a later Follow (postgresql.auto.conf
	// is included last).
	if strings.Contains(got, "-R") {
		t.Errorf("pg_basebackup args %q must not use -R", got)
	}
}

// Clone must leave the node streaming immediately without a separate caller-side Follow
// (#181) -- achieved by calling Follow itself rather than pg_basebackup -R (see
// TestNativeCloneArgs).
func TestNativeCloneCallsFollow(t *testing.T) {
	n, dataDir := newTestNative(t, &fakeRunner{})
	source := Conn{Host: "pg-0.h", Port: 5432, User: "repmgr"}
	if err := n.Clone(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	managed, err := os.ReadFile(filepath.Join(dataDir, managedConfName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(managed), "pg-0.h") {
		t.Errorf("managed conf missing primary_conninfo for the clone source:\n%s", managed)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "standby.signal")); err != nil {
		t.Errorf("standby.signal not created by Clone: %v", err)
	}
}

// #178: a transient failure to REACH the rewind source must not be reported as
// divergence -- that would trigger a needless full re-clone of a node whose history is
// probably fine. Only a genuine pg_rewind failure (histories diverged beyond repair)
// must return ErrRewindDiverged.
func TestNativeRejoinForceRewindClassifiesConnectionFailure(t *testing.T) {
	fr := &fakeRunner{failOn: "--restore-target-wal", failOut: "could not connect to server: Connection refused"}
	n, _ := newTestNative(t, fr)
	err := n.RejoinForceRewind(context.Background(), Conn{Host: "pg-0.h", User: "repmgr", DB: "repmgr"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrRewindDiverged) {
		t.Errorf("a connection failure must NOT be classified as divergence (#178), got %v", err)
	}
}

func TestNativeRejoinForceRewindClassifiesDivergence(t *testing.T) {
	fr := &fakeRunner{failOn: "--restore-target-wal", failOut: "target server needs to exit backup mode"}
	n, _ := newTestNative(t, fr)
	err := n.RejoinForceRewind(context.Background(), Conn{Host: "pg-0.h", User: "repmgr", DB: "repmgr"})
	if !errors.Is(err, ErrRewindDiverged) {
		t.Errorf("a genuine rewind failure must be ErrRewindDiverged so the caller falls back to ReclonePreserving (#175), got %v", err)
	}
}

func TestNativeRejoinForceRewindConfiguresRecoveryOnSuccess(t *testing.T) {
	fr := &fakeRunner{}
	n, dataDir := newTestNative(t, fr)
	target := Conn{Host: "pg-0.h", User: "repmgr", DB: "repmgr"}
	if err := n.RejoinForceRewind(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	// pg_rewind alone leaves the node needing standby.signal + primary_conninfo to come
	// back as a standby, not a second read-write primary (two-writer risk).
	if _, err := os.Stat(filepath.Join(dataDir, "standby.signal")); err != nil {
		t.Errorf("standby.signal not created after a successful rewind: %v", err)
	}
}

// #175: never rm -rf before a clone succeeds. A failed reclone must leave the diverged
// data recoverable, named in the error.
func TestNativeReclonePreservingKeepsDataOnCloneFailure(t *testing.T) {
	fr := &fakeRunner{failOn: "--checkpoint=fast"}
	n, dataDir := newTestNative(t, fr)
	sentinel := filepath.Join(dataDir, "PG_VERSION")
	if err := os.WriteFile(sentinel, []byte("18"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	n.Now = func() time.Time { return fixed }

	err := n.ReclonePreserving(context.Background(), Conn{Host: "src", User: "u", DB: "d"})
	if err == nil {
		t.Fatal("expected clone failure to surface")
	}
	backup := strings.TrimRight(dataDir, "/") + ".diverged.20260615T120000Z"
	if _, statErr := os.Stat(filepath.Join(backup, "PG_VERSION")); statErr != nil {
		t.Errorf("diverged data not preserved at %s: %v", backup, statErr)
	}
}

func TestNativeReclonePreservingDropsBackupOnSuccess(t *testing.T) {
	fr := &fakeRunner{}
	n, dataDir := newTestNative(t, fr)
	fixed := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	n.Now = func() time.Time { return fixed }

	if err := n.ReclonePreserving(context.Background(), Conn{Host: "src", User: "u", DB: "d"}); err != nil {
		t.Fatal(err)
	}
	backup := strings.TrimRight(dataDir, "/") + ".diverged.20260615T120000Z"
	if _, statErr := os.Stat(backup); !os.IsNotExist(statErr) {
		t.Errorf("backup should be removed on clone success, stat err = %v", statErr)
	}
}

// RegisterPrimary/RegisterStandby/Unregister are no-ops in native mode (no repmgr.nodes
// to maintain, #288) -- reconcile calls them unconditionally, so "nothing to do" must
// never surface as an error.
func TestNativeRegistrationMethodsAreNoOps(t *testing.T) {
	n, _ := newTestNative(t, &fakeRunner{})
	ctx := context.Background()
	if err := n.RegisterPrimary(ctx); err != nil {
		t.Errorf("RegisterPrimary must be a no-op, got %v", err)
	}
	if err := n.RegisterStandby(ctx, 1001); err != nil {
		t.Errorf("RegisterStandby must be a no-op, got %v", err)
	}
	if err := n.Unregister(ctx, 1001); err != nil {
		t.Errorf("Unregister must be a no-op, got %v", err)
	}
}

func TestNativeRunNeverPutsPasswordInArgv(t *testing.T) {
	fr := &fakeRunner{}
	n, _ := newTestNative(t, fr)
	if _, err := n.run(context.Background(), "pg_ctl", "-D", n.DataDir, "-w", "promote"); err != nil {
		t.Fatal(err)
	}
	call := fr.calls[len(fr.calls)-1]
	for _, a := range call.args {
		if strings.Contains(a, "secret") {
			t.Fatalf("password leaked into argv (#167): %v", call.args)
		}
	}
	found := false
	for _, e := range call.env {
		if e == "PGPASSWORD=secret" {
			found = true
		}
	}
	if !found {
		t.Errorf("PGPASSWORD not set in env: %v", call.env)
	}
}

func TestIsConnectionFailure(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"connection refused", "could not connect to server: Connection refused", true},
		{"no route to host", "could not connect to server: No route to host", true},
		{"timeout", "connection timed out", true},
		{"admin shutdown", "terminating connection due to administrator command", true},
		{"genuine divergence", "target server needs to exit backup mode", false},
		{"generic pg_rewind failure", "source and target cluster are on the same timeline", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isConnectionFailure(c.out); got != c.want {
				t.Errorf("isConnectionFailure(%q) = %v, want %v", c.out, got, c.want)
			}
		})
	}
}

func TestEscapeSingleQuoted(t *testing.T) {
	if got := escapeSingleQuoted("it's"); got != "it''s" {
		t.Errorf("escapeSingleQuoted = %q, want it''s", got)
	}
}

func TestHasActiveDirective(t *testing.T) {
	const directive = "include 'pg-ha-agent.conf'"
	cases := []struct {
		name string
		conf string
		want bool
	}{
		{"active, no indentation", "foo\n" + directive + "\nbar", true},
		{"active, surrounded by whitespace", "foo\n  " + directive + "  \nbar", true},
		{"commented out", "foo\n# " + directive + "\nbar", false},
		{"substring inside an unrelated comment", "# see " + directive + " for detail\n", false},
		{"absent", "foo\nbar", false},
		{"empty file", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasActiveDirective(c.conf, directive); got != c.want {
				t.Errorf("hasActiveDirective(%q) = %v, want %v", c.conf, got, c.want)
			}
		})
	}
}

// A commented-out include line must not be mistaken for an active one -- ensureInclude
// would otherwise report success while wal_log_hints/hot_standby/primary_conninfo are
// never actually included.
func TestNativeEnsureIncludeReAddsWhenOnlyCommentedOut(t *testing.T) {
	n, dataDir := newTestNative(t, &fakeRunner{})
	confPath := filepath.Join(dataDir, "postgresql.conf")
	if err := os.WriteFile(confPath, []byte("# include 'pg-ha-agent.conf'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := n.ensureInclude(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if !hasActiveDirective(string(b), "include 'pg-ha-agent.conf'") {
		t.Errorf("ensureInclude did not add an active include line when only a commented-out one existed:\n%s", b)
	}
}

// --- #289: replication slot ownership ---

// newTestNativeWithSlot is newTestNative plus this node's own slot name, i.e. how
// newMechanism builds it in native mode once an ordinal is known.
func newTestNativeWithSlot(t *testing.T, fr *fakeRunner, slot string) (*Native, string) {
	t.Helper()
	n, dataDir := newTestNative(t, fr)
	n.SlotName = slot
	return n, dataDir
}

// The base backup must stream through the node's own named slot, and that slot must be
// created on the upstream FIRST -- otherwise the source can recycle a segment the new
// standby still needs before the copy finishes, and the clone comes up permanently behind.
func TestNativeCloneCreatesSlotOnUpstreamBeforeStreamingThroughIt(t *testing.T) {
	fr := &fakeRunner{}
	n, _ := newTestNativeWithSlot(t, fr, "pg_ha_slot_1")
	if err := n.Clone(context.Background(), Conn{Host: "pg-0.hl", User: "repmgr", DB: "repmgr"}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	var createIdx, backupIdx = -1, -1
	for i, c := range fr.calls {
		joined := strings.Join(c.args, " ")
		if strings.HasSuffix(c.name, "psql") && strings.Contains(joined, "pg_create_physical_replication_slot") {
			createIdx = i
			if !strings.Contains(joined, "WHERE NOT EXISTS") {
				t.Errorf("slot create is not idempotent; a re-clone would fail: %s", joined)
			}
			if !strings.Contains(joined, "pg-0.hl") {
				t.Errorf("slot must be created on the UPSTREAM, not locally: %v", c.args)
			}
		}
		if strings.HasSuffix(c.name, "pg_basebackup") {
			backupIdx = i
			if !strings.Contains(joined, "--slot pg_ha_slot_1") {
				t.Errorf("pg_basebackup does not stream through the slot: %s", joined)
			}
			// -C would fail on an existing slot, which is the normal re-clone case.
			for _, a := range c.args {
				if a == "-C" || a == "--create-slot" {
					t.Errorf("pg_basebackup must not use -C (fails when the slot already exists): %v", c.args)
				}
			}
		}
	}
	if createIdx < 0 {
		t.Fatal("Clone never created the slot on the upstream")
	}
	if backupIdx < 0 {
		t.Fatal("Clone never ran pg_basebackup")
	}
	if createIdx > backupIdx {
		t.Errorf("slot created AFTER the base backup (idx %d > %d): the WAL gap this closes is still open", createIdx, backupIdx)
	}
}

// primary_slot_name is what holds the slot for the ONGOING stream; without it the
// walreceiver reconnects slotless and the upstream may recycle WAL again.
func TestNativeFollowWritesPrimarySlotName(t *testing.T) {
	n, dataDir := newTestNativeWithSlot(t, &fakeRunner{}, "pg_ha_slot_2")
	if err := n.Follow(context.Background(), Conn{Host: "pg-0.hl", User: "repmgr", DB: "repmgr"}); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dataDir, managedConfName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "primary_slot_name = 'pg_ha_slot_2'") {
		t.Errorf("managed conf missing primary_slot_name:\n%s", b)
	}
}

// A primary has no upstream, so a primary_slot_name would reserve WAL for a stream it
// never opens. It must appear only alongside primary_conninfo.
func TestNativePrimarySlotNameOnlyAppearsWithAnUpstream(t *testing.T) {
	n, dataDir := newTestNativeWithSlot(t, &fakeRunner{}, "pg_ha_slot_2")
	if err := n.GenerateConfig(context.Background(), NodeIdentity{}, ConfigOpts{}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dataDir, managedConfName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "primary_slot_name") {
		t.Errorf("primary_slot_name written with no primary_conninfo:\n%s", b)
	}
}

// An empty SlotName is the pre-#289 behaviour and must stay slotless rather than emitting
// an empty GUC (which the postmaster would reject) or an empty --slot argument.
func TestNativeNoSlotNameStaysSlotless(t *testing.T) {
	fr := &fakeRunner{}
	n, dataDir := newTestNativeWithSlot(t, fr, "")
	if err := n.Clone(context.Background(), Conn{Host: "pg-0.hl", User: "repmgr", DB: "repmgr"}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	for _, c := range fr.calls {
		joined := strings.Join(c.args, " ")
		if strings.Contains(joined, "pg_create_physical_replication_slot") {
			t.Errorf("created a slot with no SlotName set: %v", c.args)
		}
		if strings.Contains(joined, "--slot") {
			t.Errorf("passed --slot with no SlotName set: %v", c.args)
		}
	}
	b, err := os.ReadFile(filepath.Join(dataDir, managedConfName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "primary_slot_name") {
		t.Errorf("wrote primary_slot_name with no SlotName set:\n%s", b)
	}
}

// A slot-create failure must abort the clone: proceeding without the slot silently
// reintroduces the WAL gap the slot exists to prevent.
func TestNativeCloneFailsWhenSlotCreateFails(t *testing.T) {
	fr := &fakeRunner{failOn: "pg_create_physical_replication_slot"}
	n, _ := newTestNativeWithSlot(t, fr, "pg_ha_slot_1")
	err := n.Clone(context.Background(), Conn{Host: "pg-0.hl", User: "repmgr", DB: "repmgr"})
	if err == nil {
		t.Fatal("Clone succeeded despite a failed slot create")
	}
	for _, c := range fr.calls {
		if strings.HasSuffix(c.name, "pg_basebackup") {
			t.Error("ran pg_basebackup after the slot create failed")
		}
	}
}

// The slot name is interpolated into SQL, so a name outside PostgreSQL's own character
// class must be refused before any command runs.
func TestNativeCloneRejectsAnInvalidSlotName(t *testing.T) {
	fr := &fakeRunner{}
	n, _ := newTestNativeWithSlot(t, fr, "bad'; DROP TABLE x; --")
	if err := n.Clone(context.Background(), Conn{Host: "pg-0.hl", User: "repmgr", DB: "repmgr"}); err == nil {
		t.Fatal("Clone accepted an invalid slot name")
	}
	if len(fr.calls) != 0 {
		t.Errorf("an invalid slot name reached a command: %v", fr.calls)
	}
}

// The slot create must connect to a real database even when the caller supplied no DB.
func TestConnDatabaseDefaultsToPostgres(t *testing.T) {
	if got := (Conn{}).database(); got != "postgres" {
		t.Errorf("database() with no DB = %q, want postgres", got)
	}
	if got := (Conn{DB: "repmgr"}).database(); got != "repmgr" {
		t.Errorf("database() = %q, want repmgr", got)
	}
}

// The password must reach the slot-create psql via PGPASSWORD, never argv (#167).
func TestNativeSlotCreatePassesPasswordViaEnvNotArgv(t *testing.T) {
	fr := &fakeRunner{}
	n, _ := newTestNativeWithSlot(t, fr, "pg_ha_slot_1")
	if err := n.Clone(context.Background(), Conn{Host: "pg-0.hl", User: "repmgr", DB: "repmgr"}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	for _, c := range fr.calls {
		if strings.Contains(strings.Join(c.args, " "), "secret") {
			t.Errorf("password leaked into argv: %v", c.args)
		}
		if strings.HasSuffix(c.name, "psql") {
			var found bool
			for _, e := range c.env {
				if e == "PGPASSWORD=secret" {
					found = true
				}
			}
			if !found {
				t.Errorf("slot-create psql did not get PGPASSWORD: %v", c.env)
			}
		}
	}
}
