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
	for _, want := range []string{"-h pg-0.h", "-D " + dataDir, "-X stream", "-R", "--checkpoint=fast", "--no-password"} {
		if !strings.Contains(got, want) {
			t.Errorf("pg_basebackup args %q missing %q", got, want)
		}
	}
	// -R is what makes a fresh clone stream immediately without a separate Follow (#181).
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
