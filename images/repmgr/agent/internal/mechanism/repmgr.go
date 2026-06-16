package mechanism

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Runner executes a command (with extra env appended) and returns its combined
// output. Injectable so the repmgr CLI calls are unit-testable.
type Runner interface {
	Run(ctx context.Context, env []string, name string, args ...string) (string, error)
}

// OSRunner is the production Runner backed by os/exec (combined stdout+stderr so
// repmgr diagnostics surface in errors).
type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Clock returns the current time; injectable so the .diverged.<ts> suffix is
// deterministic in tests.
type Clock func() time.Time

// Repmgr drives repmgr as the HA mechanism.
type Repmgr struct {
	Runner   Runner
	Bin      string // repmgr binary (default "repmgr")
	ConfPath string // /etc/repmgr/repmgr.conf
	DataDir  string // PGDATA (for ReclonePreserving)
	Password string // cluster-wide repmgr password, supplied to libpq via PGPASSWORD
	Now      Clock
}

// NewRepmgr returns a Repmgr with production defaults.
func NewRepmgr(confPath, dataDir, password string) *Repmgr {
	return &Repmgr{Runner: OSRunner{}, Bin: "repmgr", ConfPath: confPath, DataDir: dataDir, Password: password, Now: time.Now}
}

// run invokes repmgr with -f <conf> and PGPASSWORD set to the cluster-wide repmgr
// password (libpq uses it for every connection). The password is never written to
// repmgr.conf or passed in argv (security H1).
func (r *Repmgr) run(ctx context.Context, args ...string) (string, error) {
	var env []string
	if r.Password != "" {
		env = []string{"PGPASSWORD=" + r.Password}
	}
	full := append([]string{"-f", r.ConfPath}, args...)
	return r.Runner.Run(ctx, env, r.Bin, full...)
}

func (c Conn) port() int {
	if c.Port == 0 {
		return 5432
	}
	return c.Port
}

// conninfo builds a libpq conninfo string WITHOUT the password (that goes via
// PGPASSWORD), so it never lands in argv or logs.
func (c Conn) conninfo() string {
	ct := int(c.ConnectTimeout.Seconds())
	if ct <= 0 {
		ct = 10
	}
	return fmt.Sprintf("host=%s port=%d user=%s dbname=%s connect_timeout=%d", c.Host, c.port(), c.User, c.DB, ct)
}

func (r *Repmgr) Promote(ctx context.Context) error {
	if out, err := r.run(ctx, "standby", "promote"); err != nil {
		return fmt.Errorf("repmgr standby promote: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

func (r *Repmgr) Follow(ctx context.Context, upstreamNodeID int) error {
	if out, err := r.run(ctx, "standby", "follow", "--upstream-node-id="+strconv.Itoa(upstreamNodeID)); err != nil {
		return fmt.Errorf("repmgr standby follow: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

func (r *Repmgr) Clone(ctx context.Context, source Conn) error {
	args := []string{"standby", "clone", "-h", source.Host, "-p", strconv.Itoa(source.port()), "-U", source.User, "-d", source.DB, "--force"}
	if out, err := r.run(ctx, args...); err != nil {
		return fmt.Errorf("repmgr standby clone from %s: %w: %s", source.Host, err, strings.TrimSpace(out))
	}
	return nil
}

func (r *Repmgr) RejoinForceRewind(ctx context.Context, target Conn) error {
	args := []string{"node", "rejoin", "-d", target.conninfo(), "--force-rewind", "--config-files=postgresql.conf,pg_hba.conf"}
	if out, err := r.run(ctx, args...); err != nil {
		// Any rejoin failure means pg_rewind could not proceed; the caller falls
		// back to ReclonePreserving (the data-safe path, #175).
		return fmt.Errorf("%w: repmgr node rejoin onto %s: %v: %s", ErrRewindDiverged, target.Host, err, strings.TrimSpace(out))
	}
	// `node rejoin` starts Postgres as its final step to verify the node attaches,
	// but that postmaster is NOT the agent's supervised child -- it cannot be
	// fenced on lease loss (two-writer risk) and would collide on the pid lock with
	// the child the caller is about to Start. Stop it (best-effort, mirroring
	// entrypoint.sh:170-175) so the agent owns the postmaster it then starts. A
	// stop failure here is non-fatal: rejoin itself succeeded, so do NOT report
	// ErrRewindDiverged (that would trigger an unnecessary full re-clone); the next
	// reconcile tick reconciles the running state.
	r.stopServer(ctx, "fast")
	return nil
}

// stopServer stops the local postmaster (best-effort) so the agent can (re)start
// it as its supervised child. repmgr starts Postgres as the final step of `node
// rejoin` (daemonized and untracked); the agent must stop it and run its own
// child to retain the soft-fence guarantee. pg_ctl resolves via PATH (like
// repmgr). Errors are ignored: if the server is already down there is nothing to
// stop, and a genuine stop failure self-corrects on the next tick.
func (r *Repmgr) stopServer(ctx context.Context, mode string) {
	if r.DataDir == "" {
		return
	}
	_, _ = r.Runner.Run(ctx, nil, "pg_ctl", "-D", r.DataDir, "-m", mode, "-w", "stop")
}

func (r *Repmgr) RegisterPrimary(ctx context.Context) error {
	if out, err := r.run(ctx, "primary", "register", "--force"); err != nil {
		return fmt.Errorf("repmgr primary register: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

func (r *Repmgr) RegisterStandby(ctx context.Context, upstreamNodeID int) error {
	if out, err := r.run(ctx, "standby", "register", "--upstream-node-id="+strconv.Itoa(upstreamNodeID), "--force"); err != nil {
		return fmt.Errorf("repmgr standby register: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// GenerateConfig writes repmgr.conf (mode 0600 — it carries the conninfo password,
// security review H1). Idempotent: it always rewrites the file from the inputs.
func (r *Repmgr) GenerateConfig(ctx context.Context, n NodeIdentity, o ConfigOpts) error {
	failover := o.Failover
	if failover == "" {
		failover = "manual"
	}
	slots := 0
	if o.UseReplicationSlots {
		slots = 1
	}
	// The password is NOT written to repmgr.conf (security H1); libpq picks it up
	// from PGPASSWORD on each repmgr invocation (see run).
	conninfo := fmt.Sprintf("host=%s port=5432 user=%s dbname=%s connect_timeout=10",
		n.FQDN, n.ReplUser, n.ReplDB)
	var b strings.Builder
	fmt.Fprintf(&b, "node_id=%d\n", n.NodeID)
	fmt.Fprintf(&b, "node_name='%s'\n", n.NodeName)
	fmt.Fprintf(&b, "conninfo='%s'\n", conninfo)
	fmt.Fprintf(&b, "data_directory='%s'\n", n.DataDir)
	fmt.Fprintf(&b, "pg_bindir='%s'\n", n.PGBindir)
	fmt.Fprintf(&b, "replication_user='%s'\n", n.ReplUser)
	b.WriteString("replication_type='physical'\n")
	fmt.Fprintf(&b, "failover='%s'\n", failover)
	fmt.Fprintf(&b, "use_replication_slots=%d\n", slots)
	b.WriteString("log_level=INFO\n")
	if err := os.MkdirAll(filepath.Dir(r.ConfPath), 0o755); err != nil {
		return fmt.Errorf("repmgr.conf dir: %w", err)
	}
	if err := os.WriteFile(r.ConfPath, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("write repmgr.conf: %w", err)
	}
	return nil
}

// ReclonePreserving moves the diverged PGDATA to a sibling .diverged.<ts> backup,
// clones fresh from source, and removes the backup ONLY on clone success. On
// failure the backup is kept and an error returned — diverged data is never
// destroyed before a successful clone (#175).
func (r *Repmgr) ReclonePreserving(ctx context.Context, source Conn) error {
	if r.DataDir == "" {
		return fmt.Errorf("reclone: DataDir not set")
	}
	// A prior `node rejoin` (or an earlier start) may have left a postmaster holding
	// the data directory open; stop it immediate before moving PGDATA aside so the
	// rename cannot race a running server (mirrors entrypoint.sh:179).
	r.stopServer(ctx, "immediate")
	backup := fmt.Sprintf("%s.diverged.%s", strings.TrimRight(r.DataDir, "/"), r.Now().UTC().Format("20060102T150405Z"))
	if err := os.Rename(r.DataDir, backup); err != nil {
		return fmt.Errorf("reclone: move PGDATA aside to %s: %w", backup, err)
	}
	if err := r.Clone(ctx, source); err != nil {
		// Keep the backup; the operator (or a later retry) can recover from it.
		return fmt.Errorf("reclone: clone failed, diverged data preserved at %s: %w", backup, err)
	}
	if err := os.RemoveAll(backup); err != nil {
		// Clone succeeded; a leftover backup is harmless but noisy.
		return fmt.Errorf("reclone: clone ok but could not remove backup %s: %w", backup, err)
	}
	return nil
}
