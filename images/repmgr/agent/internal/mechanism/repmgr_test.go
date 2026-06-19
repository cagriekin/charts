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
	name string
	env  []string
	args []string
}

type fakeRunner struct {
	calls   []recordedCall
	failOn  string // if a call's args contain this substring, it errors
	failOut string // combined output returned alongside the error on a failing call
}

func (f *fakeRunner) Run(_ context.Context, env []string, name string, args ...string) (string, error) {
	f.calls = append(f.calls, recordedCall{name: name, env: env, args: args})
	if f.failOn != "" && strings.Contains(strings.Join(args, " "), f.failOn) {
		out := "simulated failure"
		if f.failOut != "" {
			out = f.failOut
		}
		return out, errors.New("exit status 23")
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
	return &Repmgr{Runner: fr, Bin: "repmgr", ConfPath: "/etc/repmgr/repmgr.conf", Password: "secret", Now: time.Now}
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

	t.Run("follow treats already-following exit as a no-op (#182)", func(t *testing.T) {
		// A healthy standby already streaming from the target: repmgr exits non-zero
		// but there is nothing to do. Must be a successful no-op so the agent latches
		// and does not re-run follow every tick.
		fr := &fakeRunner{failOn: "standby follow", failOut: "INFO: timelines are same, this server is not ahead\n" +
			"DETAIL: local node lsn is 0/3000000, follow target lsn is 0/3000000\n" +
			"ERROR: slot \"repmgr_slot_1000\" already exists as an active slot\n" +
			"NOTICE: STANDBY FOLLOW failed"}
		if err := newTestRepmgr(fr).Follow(ctx, 1000); err != nil {
			t.Fatalf("an already-following standby must be a no-op, got %v", err)
		}
	})

	t.Run("follow surfaces a genuine failure", func(t *testing.T) {
		// A real failure (slot active but NOT the benign already-following case) must
		// still surface so it is not silently swallowed.
		fr := &fakeRunner{failOn: "standby follow", failOut: "ERROR: connection to upstream node failed"}
		if err := newTestRepmgr(fr).Follow(ctx, 1000); err == nil {
			t.Fatal("a genuine follow failure must surface as an error")
		}
	})

	t.Run("follow does not swallow slot-active without the not-ahead signal", func(t *testing.T) {
		// Both conditions are required: a slot-active error WITHOUT same-timeline/
		// not-ahead may mean real work is pending (e.g. a stale slot on a divergent
		// upstream), so it must surface, not be treated as already-following.
		fr := &fakeRunner{failOn: "standby follow", failOut: "ERROR: slot \"repmgr_slot_1000\" already exists as an active slot"}
		if err := newTestRepmgr(fr).Follow(ctx, 1000); err == nil {
			t.Fatal("slot-active alone (no same-timeline/not-ahead) must surface as an error")
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

	t.Run("unregister targets the node-id and keeps PGPASSWORD off argv (#139)", func(t *testing.T) {
		fr := &fakeRunner{}
		if err := newTestRepmgr(fr).Unregister(ctx, 1003); err != nil {
			t.Fatal(err)
		}
		got := fr.lastArgs()
		if !strings.Contains(got, "-f /etc/repmgr/repmgr.conf standby unregister --node-id=1003") {
			t.Errorf("argv = %q", got)
		}
		if strings.Contains(got, "secret") {
			t.Errorf("password leaked into argv: %q", got)
		}
		if env := fr.calls[len(fr.calls)-1].env; len(env) == 0 || env[0] != "PGPASSWORD=secret" {
			t.Errorf("PGPASSWORD not in env: %v", env)
		}
	})

	t.Run("unregister surfaces a repmgr error", func(t *testing.T) {
		fr := &fakeRunner{failOn: "standby unregister"}
		if err := newTestRepmgr(fr).Unregister(ctx, 1003); err == nil {
			t.Fatal("a failed unregister must surface as an error")
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
		// rejoin failed -> must NOT attempt a post-rejoin stop (no postmaster started)
		for _, c := range fr.calls {
			if c.name == "pg_ctl" {
				t.Errorf("unexpected pg_ctl stop after a failed rejoin: %v", c.args)
			}
		}
	})

	t.Run("rejoin success stops the repmgr-started postmaster", func(t *testing.T) {
		fr := &fakeRunner{}
		r := newTestRepmgr(fr)
		r.DataDir = "/pgdata"
		if err := r.RejoinForceRewind(ctx, src); err != nil {
			t.Fatal(err)
		}
		// repmgr node rejoin starts Postgres; the agent must stop that untracked
		// postmaster (fast) so it can supervise its own child (two-writer safety).
		last := fr.calls[len(fr.calls)-1]
		if last.name != "pg_ctl" {
			t.Fatalf("last call should be pg_ctl stop, got name=%q args=%v", last.name, last.args)
		}
		if got := strings.Join(last.args, " "); got != "-D /pgdata -m fast -w stop" {
			t.Errorf("post-rejoin stop argv = %q", got)
		}
		if first := strings.Join(fr.calls[0].args, " "); !strings.Contains(first, "node rejoin") {
			t.Errorf("rejoin must run before the stop, first argv = %q", first)
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
	if strings.Contains(s, "password=") || strings.Contains(s, "pw") {
		t.Errorf("repmgr.conf must not contain the password (security H1):\n%s", s)
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

	fr := &fakeRunner{}
	r.Runner = fr
	if err := r.ReclonePreserving(context.Background(), Conn{Host: "src", User: "u", DB: "d"}); err != nil {
		t.Fatal(err)
	}
	backup := dataDir + ".diverged.20260615T120000Z"
	if _, statErr := os.Stat(backup); !os.IsNotExist(statErr) {
		t.Errorf("backup should be removed on clone success, stat err = %v", statErr)
	}
	// the immediate stop must run before the data dir is moved aside
	if len(fr.calls) == 0 || fr.calls[0].name != "pg_ctl" || strings.Join(fr.calls[0].args, " ") != "-D "+dataDir+" -m immediate -w stop" {
		t.Errorf("expected an immediate pg_ctl stop first, calls = %+v", fr.calls)
	}
}
