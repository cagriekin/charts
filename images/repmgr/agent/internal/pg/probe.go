package pg

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Exec runs an external command and returns its trimmed stdout. It is an
// interface so probes are unit-testable with a fake and the psql backend can
// later be swapped for a Go driver without touching callers.
type Exec interface {
	Run(ctx context.Context, env []string, name string, args ...string) (string, error)
}

// OSExec is the production Exec backed by os/exec.
type OSExec struct{}

// Run executes name with args, appending env to the current environment, and
// returns trimmed stdout. Stderr is captured into the error (psql diagnostics).
func (OSExec) Run(ctx context.Context, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// ConnInfo is how to reach one PostgreSQL node. Password is passed via PGPASSWORD,
// never on the command line, so it never appears in argv or logs.
type ConnInfo struct {
	Host     string
	Port     int
	User     string
	DB       string
	Password string
}

// Role is a node's observed replication role.
type Role int

const (
	RoleUnknown Role = iota
	RolePrimary
	RoleStandby
)

func (r Role) String() string {
	switch r {
	case RolePrimary:
		return "primary"
	case RoleStandby:
		return "standby"
	default:
		return "unknown"
	}
}

// NodeState is a point-in-time observation of one node. A node that cannot be
// reached (down, auth failure, timeout) has Reachable=false and every other field
// at its zero value — callers must never treat an unreachable node as a primary.
type NodeState struct {
	Host       string
	Reachable  bool
	Role       Role
	Timeline   Timeline // primary only (from the current WAL insert position)
	TimelineOK bool
	// WriteLSN is the position used for survivor ranking (invariant 8): the
	// primary's current WAL LSN, or a standby's last *received* LSN.
	WriteLSN LSN
	LSNOK    bool
}

// Prober runs SQL probes against PostgreSQL nodes via psql.
type Prober struct {
	Exec    Exec
	Timeout time.Duration
}

// NewProber returns a Prober using the real psql with the shell's 10s timeout.
func NewProber() *Prober { return &Prober{Exec: OSExec{}, Timeout: 10 * time.Second} }

// psql runs one query (tuples-only, unaligned) and returns trimmed stdout. The
// per-call context bounds total time; PGCONNECT_TIMEOUT bounds the connect phase
// (mirrors the shell `timeout 10 psql ... connect_timeout=10`).
func (p *Prober) psql(ctx context.Context, ci ConnInfo, sql string) (string, error) {
	to := p.Timeout
	if to <= 0 {
		to = 10 * time.Second // a zero Timeout would make the deadline expire immediately
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	env := []string{
		"PGPASSWORD=" + ci.Password,
		fmt.Sprintf("PGCONNECT_TIMEOUT=%d", int(to.Seconds())),
	}
	args := []string{"-h", ci.Host, "-p", strconv.Itoa(ci.Port), "-U", ci.User, "-d", ci.DB, "-tAc", sql}
	return p.Exec.Run(ctx, env, "psql", args...)
}

// InRecovery reports pg_is_in_recovery(). reachable is false when the node could
// not be queried or returned anything other than the expected 't'/'f'.
func (p *Prober) InRecovery(ctx context.Context, ci ConnInfo) (inRecovery, reachable bool, err error) {
	out, err := p.psql(ctx, ci, "SELECT pg_is_in_recovery();")
	if err != nil {
		return false, false, err
	}
	switch out {
	case "t":
		return true, true, nil
	case "f":
		return false, true, nil
	default:
		return false, false, nil
	}
}

// PrimaryWALPosition reads a primary's timeline + current WAL LSN. The timeline is
// taken from the WAL insert position (pg_walfile_name(pg_current_wal_lsn())), which
// reflects a fast promotion immediately, NOT pg_control_checkpoint() which lags.
// pg_current_wal_lsn() is primary-only; ok is false on a standby or unreadable node.
func (p *Prober) PrimaryWALPosition(ctx context.Context, ci ConnInfo) (tl Timeline, lsn LSN, ok bool, err error) {
	out, err := p.psql(ctx, ci, "SELECT substring(pg_walfile_name(pg_current_wal_lsn()) from 1 for 8), pg_current_wal_lsn();")
	if err != nil {
		return 0, LSN{}, false, err
	}
	hi, lo, found := strings.Cut(out, "|")
	if !found {
		return 0, LSN{}, false, nil
	}
	tl, tlok := ParseTimeline(strings.TrimSpace(hi))
	lsn, lok := ParseLSN(strings.TrimSpace(lo))
	if !tlok || !lok {
		return 0, LSN{}, false, nil
	}
	return tl, lsn, true, nil
}

// StandbyReceiveLSN reads a standby's last received WAL LSN (the position used to
// rank standbys for most-advanced promotion, invariant 8).
func (p *Prober) StandbyReceiveLSN(ctx context.Context, ci ConnInfo) (recv LSN, ok bool, err error) {
	out, err := p.psql(ctx, ci, "SELECT pg_last_wal_receive_lsn();")
	if err != nil {
		return LSN{}, false, err
	}
	recv, lok := ParseLSN(strings.TrimSpace(out))
	if !lok {
		return LSN{}, false, nil
	}
	return recv, true, nil
}

// Probe classifies a node by its actual role and reads the WAL position relevant
// to that role. An unreachable node returns NodeState{Host, Reachable:false}.
func (p *Prober) Probe(ctx context.Context, ci ConnInfo) NodeState {
	ns := NodeState{Host: ci.Host}
	inRec, reachable, err := p.InRecovery(ctx, ci)
	if err != nil || !reachable {
		return ns
	}
	ns.Reachable = true
	if inRec {
		ns.Role = RoleStandby
		if recv, ok, _ := p.StandbyReceiveLSN(ctx, ci); ok {
			ns.WriteLSN, ns.LSNOK = recv, true
		}
		return ns
	}
	ns.Role = RolePrimary
	if tl, lsn, ok, _ := p.PrimaryWALPosition(ctx, ci); ok {
		ns.Timeline, ns.TimelineOK = tl, true
		ns.WriteLSN, ns.LSNOK = lsn, true
	}
	return ns
}
