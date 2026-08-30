// Package process supervises PostgreSQL as a child of the agent (PID 1 in the
// container). Running Postgres as a child — not exec-replacing into it and not
// pg_ctl-daemonizing it — is what lets the agent authoritatively demote/stop it on
// lease loss (the soft-fence guarantee).
package process

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/cagriekin/pg-ha-agent/internal/childenv"
)

// StopMode selects the PostgreSQL shutdown signal.
type StopMode int

const (
	// Fast = SIGINT: roll back active transactions, clean shutdown.
	Fast StopMode = iota
	// Immediate = SIGQUIT: abort without a clean shutdown (crash recovery on next
	// start). Used on the fence path so the stop is bounded and decoupled from
	// checkpoint load.
	Immediate
)

// Postmaster controls a PostgreSQL server process.
type Postmaster interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context, mode StopMode) error
	Reload(ctx context.Context) error
	Running() bool
}

// HasData reports whether dataDir holds an initialized cluster (PG_VERSION present).
func HasData(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, "PG_VERSION"))
	return err == nil
}

// ChildPostmaster runs `postgres -D <dataDir>` as a direct child and signals it.
type ChildPostmaster struct {
	PostgresBin string
	DataDir     string

	mu     sync.Mutex
	cmd    *exec.Cmd
	exited chan error  // single waiter delivers the child's exit here
	done   atomic.Bool // true once the child's Wait has returned (race-free liveness)
}

// NewChildPostmaster builds a ChildPostmaster (PostgresBin e.g. /usr/lib/postgresql/18/bin/postgres).
func NewChildPostmaster(postgresBin, dataDir string) *ChildPostmaster {
	return &ChildPostmaster{PostgresBin: postgresBin, DataDir: dataDir}
}

func (p *ChildPostmaster) Start(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil {
		// Distinguish a still-running postmaster (Start is then an idempotent no-op,
		// so a reconcile tick that fires while postgres is mid-startup does not log a
		// spurious error) from one that has already exited on its own (a crashed
		// postgres -- clear the stale handle and start fresh, so the next reconcile
		// tick actually recovers it instead of looping on "already started").
		//
		// Keyed on p.done, NOT on a non-blocking receive from p.exited (#298 review). The
		// Wait goroutine publishes the exit in two steps -- done.Store(true), then the
		// channel send -- and Running() reads only the first. Between them a tick sees
		// Running()==false, Decide routes to StartLocal, and Start found an EMPTY channel:
		// it took the default arm and returned nil, reporting a successful start with no
		// postmaster running. done is the same flag Running() answers from, so the two can
		// no longer disagree; the receive that follows is blocking, and safe because done
		// is only ever set immediately before that send.
		if !p.done.Load() {
			return nil // still running
		}
		// Drain non-blockingly: done is the authority, the channel value is only being
		// reaped. A blocking receive here would hold p.mu while waiting, and a concurrent
		// Stop that had already taken the single queued value (but not yet reached its own
		// mu-taking clear()) would deadlock the two. opMu serialises every caller today, so
		// that cannot happen -- this just declines to depend on it.
		select {
		case <-p.exited:
		default:
		}
		p.cmd, p.exited = nil, nil // exited on its own; fall through to a fresh start
	}
	cmd := exec.Command(p.PostgresBin, "-D", p.DataDir)
	// Strip the agent's credential env from the postmaster (#298 security review):
	// postgres authenticates nothing from its environment (roles live in pg_authid),
	// but every archive_command/restore_command child it forks inherits this env --
	// pgBackRest in particular parses all of it -- so REPMGR_PASSWORD/POSTGRES_PASSWORD
	// must not reach the longest-lived child of all. The PGBACKREST_* config vars
	// (STANZA, CIPHER_PASS, S3_KEY_SECRET) carry no "PASSWORD" substring and pass through.
	cmd.Env = childenv.Filtered(os.Environ(), nil)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start postgres: %w", err)
	}
	p.cmd = cmd
	p.exited = make(chan error, 1)
	p.done.Store(false)
	go func() { // the single Wait owner (also reaps the child)
		err := cmd.Wait()
		p.done.Store(true) // set before the channel send so Running() never reports a dead child as alive
		p.exited <- err
	}()
	return nil
}

// Running reports whether the supervised postmaster process is currently alive
// (started and not yet exited). This is process liveness, distinct from SQL
// readiness: a postmaster replaying WAL toward consistency is Running()=true but
// still rejects connections. The reconcile loop uses it so a starting standby is
// not misclassified as stopped (#181).
func (p *ChildPostmaster) Running() bool {
	p.mu.Lock()
	alive := p.cmd != nil
	p.mu.Unlock()
	return alive && !p.done.Load()
}

func (p *ChildPostmaster) Stop(ctx context.Context, mode StopMode) error {
	p.mu.Lock()
	cmd, exited := p.cmd, p.exited
	p.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	sig := syscall.SIGINT
	if mode == Immediate {
		sig = syscall.SIGQUIT
	}
	_ = cmd.Process.Signal(sig)
	select {
	case <-ctx.Done():
		// Last-resort SIGKILL when a wedged postmaster ignores SIGINT/SIGQUIT past
		// the deadline. SIGKILL denies the postmaster the chance to reap its
		// backends, so they reparent to this agent (PID 1). They are not reaped here
		// (the agent runs no generic Wait4(-1) reaper -- it would race the
		// single-Wait-owner model below and corrupt crash detection). They exit
		// promptly once they observe the postmaster gone and linger only as zombies
		// (a PID-table slot, no memory) until the container restarts. Acceptable: a
		// SIGKILL fence is rare and bounded; a full PID-1 reaper is a known follow-up.
		_ = cmd.Process.Kill()
		<-exited // reap the killed postmaster (the direct child)
		p.clear()
		return ctx.Err()
	case <-exited:
		p.clear()
		return nil
	}
}

func (p *ChildPostmaster) Reload(_ context.Context) error {
	p.mu.Lock()
	cmd := p.cmd
	p.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return fmt.Errorf("postmaster not running")
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	return nil
}

// Exited returns a channel that receives the child's exit (for the main loop to
// detect an unexpected postmaster crash). nil when not started.
func (p *ChildPostmaster) Exited() <-chan error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exited
}

func (p *ChildPostmaster) clear() {
	p.mu.Lock()
	p.cmd, p.exited = nil, nil
	p.done.Store(false)
	p.mu.Unlock()
}
