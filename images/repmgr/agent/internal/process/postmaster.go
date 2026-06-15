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
	"syscall"
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
	exited chan error // single waiter delivers the child's exit here
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
		select {
		case <-p.exited:
			p.cmd, p.exited = nil, nil // exited on its own; fall through to a fresh start
		default:
			return nil // still running
		}
	}
	cmd := exec.Command(p.PostgresBin, "-D", p.DataDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start postgres: %w", err)
	}
	p.cmd = cmd
	p.exited = make(chan error, 1)
	go func() { p.exited <- cmd.Wait() }() // the single Wait owner (also reaps the child)
	return nil
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
		_ = cmd.Process.Kill()
		<-exited // reap the killed child
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
	p.mu.Unlock()
}
