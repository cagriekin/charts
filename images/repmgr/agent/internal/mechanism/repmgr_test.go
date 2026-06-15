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

type recordedCall struct {
	env  []string
	args []string
}

type fakeRunner struct {
	calls  []recordedCall
	failOn string // if a call's args contain this substring, it errors
}

func (f *fakeRunner) Run(_ context.Context, env []string, _ string, args ...string) (string, error) {
	f.calls = append(f.calls, recordedCall{env: env, args: args})
	if f.failOn != "" && strings.Contains(strings.Join(args, " "), f.failOn) {
		return "simulated failure", errors.New("exit status 1")
	}
	return "ok", nil
}

func (f *fakeRunner) lastArgs() string {
	if len(f.calls) == 0 {
		return ""
	}
	return strings.Join(f.calls[len(f.calls)-1].args, " ")
}

func newTestRepmgr(fr *fakeRunner) *Repmgr {
	return &Repmgr{Runner: fr, Bin: "repmgr", ConfPath: "/etc/repmgr/repmgr.conf", Now: time.Now}
}

func TestCLICommands(t *testing.T) {
	ctx := context.Background()
	src := Conn{Host: "pg-1.h", Port: 5432, User: "repmgr", DB: "repmgr", Password: "secret"}

	t.Run("promote", func(t *testing.T) {
		fr := &fakeRunner{}
		if err := newTestRepmgr(fr).Promote(ctx); err != nil {
			t.Fatal(err)
		}
		if got := fr.lastArgs(); !strings.Contains(got, "-f /etc/repmgr/repmgr.conf standby promote") {
			t.Errorf("argv = %q", got)
		}
	})

	t.Run("follow", func(t *testing.T) {
		fr := &fakeRunner{}
		if err := newTestRepmgr(fr).Follow(ctx, 1001); err != nil {
			t.Fatal(err)
		}
		if got := fr.lastArgs(); !strings.Contains(got, "standby follow --upstream-node-id=1001") {
			t.Errorf("argv = %q", got)
		}
	})

	t.Run("clone passes PGPASSWORD via env, not argv", func(t *testing.T) {
		fr := &fakeRunner{}
		if err := newTestRepmgr(fr).Clone(ctx, src); err != nil {
			t.Fatal(err)
		}
		got := fr.lastArgs()
		if !strings.Contains(got, "standby clone -h pg-1.h -p 5432 -U repmgr -d repmgr --force") {
			t.Errorf("argv = %q", got)
		}
		if strings.Contains(got, "secret") {
			t.Errorf("password leaked into argv: %q", got)
		}
		if env := fr.calls[len(fr.calls)-1].env; len(env) == 0 || env[0] != "PGPASSWORD=secret" {
			t.Errorf("PGPASSWORD not in env: %v", env)
		}
	})

	t.Run("rejoin failure maps to ErrRewindDiverged", func(t *testing.T) {
		fr := &fakeRunner{failOn: "rejoin"}
		err := newTestRepmgr(fr).RejoinForceRewind(ctx, src)
		if !errors.Is(err, ErrRewindDiverged) {
			t.Errorf("want ErrRewindDiverged, got %v", err)
		}
		if got := fr.lastArgs(); !strings.Contains(got, "node rejoin -d host=pg-1.h") || !strings.Contains(got, "--force-rewind") {
			t.Errorf("argv = %q", got)
		}
	})

	t.Run("register", func(t *testing.T) {
		fr := &fakeRunner{}
		r := newTestRepmgr(fr)
		if err := r.RegisterPrimary(ctx); err != nil {
			t.Fatal(err)
		}
		if got := fr.lastArgs(); !strings.Contains(got, "primary register --force") {
			t.Errorf("primary argv = %q", got)
		}
		if err := r.RegisterStandby(ctx, 1002); err != nil {
			t.Fatal(err)
		}
		if got := fr.lastArgs(); !strings.Contains(got, "standby register --upstream-node-id=1002 --force") {
			t.Errorf("standby argv = %q", got)
		}
	})
}

func TestGenerateConfig(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "repmgr.conf")
	r := &Repmgr{Runner: &fakeRunner{}, Bin: "repmgr", ConfPath: conf, Now: time.Now}
	n := NodeIdentity{NodeID: 1000, NodeName: "pg-0", FQDN: "pg-0.h", DataDir: "/pgdata", PGBindir: "/usr/lib/postgresql/18/bin", ReplUser: "repmgr", ReplDB: "repmgr", ReplPassword: "pw"}
	if err := r.GenerateConfig(context.Background(), n, ConfigOpts{Failover: "manual", UseReplicationSlots: true}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"node_id=1000", "failover='manual'", "use_replication_slots=1", "replication_type='physical'"} {
		if !strings.Contains(s, want) {
			t.Errorf("config missing %q in:\n%s", want, s)
		}
	}
	if info, _ := os.Stat(conf); info.Mode().Perm() != 0o600 {
		t.Errorf("repmgr.conf mode = %v, want 0600 (carries the conninfo password)", info.Mode().Perm())
	}
}

func TestReclonePreservingKeepsDataOnCloneFailure(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "pgdata")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dataDir, "PG_VERSION")
	if err := os.WriteFile(sentinel, []byte("18"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	r := &Repmgr{Runner: &fakeRunner{failOn: "clone"}, Bin: "repmgr", ConfPath: "x", DataDir: dataDir, Now: func() time.Time { return fixed }}

	err := r.ReclonePreserving(context.Background(), Conn{Host: "src", User: "u", DB: "d"})
	if err == nil {
		t.Fatal("expected clone failure to surface")
	}
	backup := dataDir + ".diverged.20260615T120000Z"
	if _, statErr := os.Stat(filepath.Join(backup, "PG_VERSION")); statErr != nil {
		t.Errorf("diverged data not preserved at %s: %v", backup, statErr)
	}
}

func TestReclonePreservingDropsBackupOnSuccess(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "pgdata")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	r := &Repmgr{Runner: &fakeRunner{}, Bin: "repmgr", ConfPath: "x", DataDir: dataDir, Now: func() time.Time { return fixed }}

	if err := r.ReclonePreserving(context.Background(), Conn{Host: "src", User: "u", DB: "d"}); err != nil {
		t.Fatal(err)
	}
	backup := dataDir + ".diverged.20260615T120000Z"
	if _, statErr := os.Stat(backup); !os.IsNotExist(statErr) {
		t.Errorf("backup should be removed on clone success, stat err = %v", statErr)
	}
}
