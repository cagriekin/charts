package pg

import (
	"context"
	"errors"
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

// Run executes name with args, appending env to the current environment, and returns
// trimmed stdout.
//
// psql writes its diagnostics to STDERR, and cmd.Output() puts them in
// exec.ExitError.Stderr -- but ExitError.Error() renders only "exit status N", so an
// unwrapped error carries none of them. Every caller that inspects the message therefore
// saw nothing to inspect: IsDuplicateSlot could never match a real create/create race
// (verified: stdout empty, err.Error() == "exit status 1", the ERROR text only in
// ExitError.Stderr), and every probe failure logged an opaque "exit status 1" instead of
// the reason PostgreSQL gave. Fold stderr into the error so both work.
//
// Stdout is returned separately and unchanged: callers parse it as query output, so
// diagnostics must not be mixed into it the way CombinedOutput would.
func (OSExec) Run(ctx context.Context, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), withStderr(err)
}

// withStderr rewraps an *exec.ExitError so its message carries the command's stderr,
// which is where psql puts every diagnostic worth reading. Non-ExitError failures
// (context deadline, binary not found) already describe themselves and pass through.
func withStderr(err error) error {
	var ee *exec.ExitError
	if !errors.As(err, &ee) || len(ee.Stderr) == 0 {
		return err
	}
	return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
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

// StandbyReceiveLSN reads a standby's furthest WAL position -- the greater of the
// last RECEIVED (streaming) and last REPLAYED LSN -- used to rank standbys for
// most-advanced promotion (invariant 8). A node replaying its OWN WAL in recovery
// mode (no streaming upstream, e.g. a primary-state node brought up read-only for
// the cold-boot election) has a NULL receive LSN but a real replay LSN, so the
// GREATEST (with NULLs coalesced) reports its true end-of-WAL.
func (p *Prober) StandbyReceiveLSN(ctx context.Context, ci ConnInfo) (recv LSN, ok bool, err error) {
	out, err := p.psql(ctx, ci, "SELECT GREATEST(COALESCE(pg_last_wal_receive_lsn(), '0/0'), COALESCE(pg_last_wal_replay_lsn(), '0/0'));")
	if err != nil {
		return LSN{}, false, err
	}
	recv, lok := ParseLSN(strings.TrimSpace(out))
	if !lok {
		return LSN{}, false, nil
	}
	return recv, true, nil
}

// StandbyTimeline reads a standby's current timeline as the GREATER of its
// control-file checkpoint timeline and its minimum-recovery-end timeline
// (pg_control_recovery.min_recovery_end_timeline). pg_walfile_name(pg_current_wal_lsn())
// is primary-only, so without a timeline a RUNNING standby would trip unsafeToServe's
// "timeline unreadable" guard and never promote (failover livelock).
//
// The checkpoint timeline alone is NOT enough: it only advances at a restartpoint, so
// a standby that has FOLLOWED a newly-promoted primary onto a higher timeline by
// streaming, but not yet checkpointed, still reports the OLD timeline. That made the
// #125 highwater guard reject a genuinely caught-up standby on failover -- masked
// while a rejoin always fell back to a full re-clone (which copied the primary's
// control file at the new timeline), but exposed once pg_rewind rejoin works (#178):
// the rewound node streams the new timeline while its checkpoint timeline lags.
//
// min_recovery_end_timeline is the timeline of the furthest WAL the standby has
// durably received/flushed during recovery; it advances as the standby replays the
// timeline switch -- ahead of the checkpoint -- and, crucially, PERSISTS in the
// control file after the upstream dies (unlike pg_stat_wal_receiver.received_tli, which
// vanishes the instant the walreceiver disconnects -- i.e. exactly at the failover
// moment when promotion is decided). So GREATEST gives the node's true current
// timeline even mid-failover; whether it has all committed WAL is the separate
// LSN/most-advanced-replica check, not this timeline-staleness gate. The recovery
// timeline is NULL/0 outside recovery (COALESCE -> 0, GREATEST falls back to the
// checkpoint timeline). Both are decimal (like pg_controldata, NOT the hex WAL-file
// timeline), parsed base 10.
func (p *Prober) StandbyTimeline(ctx context.Context, ci ConnInfo) (tl Timeline, ok bool, err error) {
	out, err := p.psql(ctx, ci, "SELECT GREATEST((SELECT timeline_id FROM pg_control_checkpoint()), COALESCE((SELECT min_recovery_end_timeline FROM pg_control_recovery()), 0));")
	if err != nil {
		return 0, false, err
	}
	n, perr := strconv.ParseUint(strings.TrimSpace(out), 10, 32)
	if perr != nil {
		return 0, false, nil
	}
	return Timeline(n), true, nil
}

// StreamingUpstream reports the host of the upstream this standby's walreceiver is
// connected to and whether it is actively streaming, read from
// pg_stat_wal_receiver. A standby with no walreceiver row (not yet attached, or a
// primary) returns streaming=false. Used to make the Follow reconcile idempotent
// (#182): a standby already streaming from the lease holder needs no repmgr standby
// follow (which would error "slot already active" and re-run every tick). The host
// is sender_host, derived from primary_conninfo -- the upstream's registered FQDN.
func (p *Prober) StreamingUpstream(ctx context.Context, ci ConnInfo) (host string, streaming bool, err error) {
	out, err := p.psql(ctx, ci, "SELECT sender_host, status FROM pg_stat_wal_receiver;")
	if err != nil {
		return "", false, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", false, nil // no walreceiver row: not streaming
	}
	h, status, found := strings.Cut(out, "|")
	if !found {
		return "", false, nil
	}
	return strings.TrimSpace(h), strings.TrimSpace(status) == "streaming", nil
}

// SystemIdentifier reads a running peer's cluster identity from pg_control_system().
// It is compared to the local pg_controldata identifier to refuse a clone/follow/
// rewind from a peer of a different cluster (invariant 9 -- a stale/misrouted pod on
// a shared headless Service, a leftover, or a DR-restored cluster). ok is false when
// the peer is unreachable or returns an unparseable value.
func (p *Prober) SystemIdentifier(ctx context.Context, ci ConnInfo) (id uint64, ok bool, err error) {
	out, err := p.psql(ctx, ci, "SELECT system_identifier FROM pg_control_system();")
	if err != nil {
		return 0, false, err
	}
	n, perr := strconv.ParseUint(strings.TrimSpace(out), 10, 64)
	if perr != nil {
		return 0, false, nil
	}
	return n, true, nil
}

// RegisteredNodeIDs returns every node_id present in repmgr.nodes, read from whichever
// copy ci points at. Unlike StandbyNodeIDs this includes the primary row and is meant to
// be run against the LOCAL node: repmgr.nodes replicates from the primary, so a standby's
// own copy tells it which nodes the cluster has a record for.
//
// That is the signal behind the #297 promote gate. A node whose own row is absent has
// never registered, so no survivor can `repmgr standby follow` it -- promoting it would
// leave the cluster with no serving primary. A malformed row is an error rather than a
// silent skip, so a broken table cannot read as "nobody is registered" and quietly
// disable the gate.
//
// Under the #287 native mechanism there is no repmgr.nodes at all, so this always errors
// and reconcile.Observation.RegistryRead stays false -- by design (see that field's doc):
// the gate the caller drives from this is fail-open, so native mode promotes normally
// instead of being silently blocked. Do not make this return an empty, error-free result
// for a missing table; that would flip RegistryRead to true and make the gate fire on every
// native-mode promote.
func (p *Prober) RegisteredNodeIDs(ctx context.Context, ci ConnInfo) ([]int, error) {
	out, err := p.psql(ctx, ci, "SELECT node_id FROM repmgr.nodes;")
	if err != nil {
		return nil, err
	}
	var ids []int
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n, perr := strconv.Atoi(line)
		if perr != nil {
			return nil, fmt.Errorf("parse repmgr.nodes node_id %q: %w", line, perr)
		}
		ids = append(ids, n)
	}
	return ids, nil
}

// StandbyNodeIDs returns the node_ids of the STANDBY rows in repmgr.nodes, queried on
// the repmgr database (ci must point at it). The primary uses this to find ghost records
// left by a replicaCount scale-down (#139). It excludes the primary row deliberately: a
// scaled-down ex-primary's row is type='primary', which `repmgr standby unregister`
// cannot remove, so listing it would make the caller re-attempt (and re-warn) a
// never-succeeding unregister every tick -- such a row is left for an operator instead.
// An unparseable row is an error rather than a silent skip, so a malformed table never
// masks a ghost.
func (p *Prober) StandbyNodeIDs(ctx context.Context, ci ConnInfo) ([]int, error) {
	out, err := p.psql(ctx, ci, "SELECT node_id FROM repmgr.nodes WHERE type = 'standby';")
	if err != nil {
		return nil, err
	}
	var ids []int
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n, perr := strconv.Atoi(line)
		if perr != nil {
			return nil, fmt.Errorf("parse repmgr.nodes node_id %q: %w", line, perr)
		}
		ids = append(ids, n)
	}
	return ids, nil
}

// SlotState is one physical replication slot as the primary sees it (#289).
//
// RetainedWALBytes is how much WAL the slot is holding back:
// pg_current_wal_lsn() - restart_lsn. A NULL restart_lsn reads as 0 rather than being
// skipped, so a slot is always enumerated -- but 0 is ambiguous on its own, which is
// exactly what Reserving and WALStatus below disambiguate.
type SlotState struct {
	Name             string
	Active           bool
	RetainedWALBytes int64
	// WALStatus is pg_replication_slots.wal_status verbatim, "" when the slot has reserved
	// no WAL yet. "lost" is the one that matters most: PostgreSQL has INVALIDATED the slot
	// because it exceeded max_slot_wal_keep_size, so the WAL its consumer needs is gone and
	// that standby can only recover by a full re-clone.
	WALStatus string
	// Reserving is restart_lsn IS NOT NULL -- whether the slot is holding any WAL back at
	// all. It separates the two states that both read as RetainedWALBytes == 0: a slot
	// freshly created ahead of its standby (nothing reserved yet, harmless) and an
	// invalidated one (reservation destroyed, harmful). Verified against PostgreSQL 18:
	// wal_status is NULL with a NULL restart_lsn before first reservation, and "lost" with
	// a NULL restart_lsn after invalidation.
	Reserving bool
}

// Invalidated reports that PostgreSQL dropped this slot's WAL reservation because the slot
// exceeded max_slot_wal_keep_size (#289).
//
// This is the failure the chart actually ships into, not an unbounded disk-fill: the image
// sets max_slot_wal_keep_size = 4GB at initdb, so PostgreSQL caps the damage by killing the
// slot rather than filling the volume. The consequence is quieter and worse to miss -- the
// standby behind it cannot resume and needs a full re-clone -- and retained-WAL alerting
// cannot see it, because invalidation sets restart_lsn to NULL and the retained-bytes gauge
// COLLAPSES TO ZERO at exactly that moment (verified against PostgreSQL 18).
func (s SlotState) Invalidated() bool { return s.WALStatus == "lost" }

// PhysicalSlots lists the physical replication slots on the node ci points at, with
// their active flag and retained WAL (#289).
//
// Works on a STANDBY as well as a primary, which the obvious form of this query does not:
// pg_current_wal_lsn() is primary-only and raises `ERROR: recovery is in progress` on a
// standby (verified against PostgreSQL 18), so the whole listing came back empty there.
// That mattered once a demoted primary had to be able to reclaim the slots it minted while
// it was the primary -- slots that keep reserving WAL on the ex-primary's own volume, with
// nothing consuming them (verified: a leftover slot on a live streaming standby reports
// 16MB reserved and wal_status = reserved). The CASE picks the standby's last RECEIVED LSN
// instead, which is that node's own end-of-WAL and therefore the right reference for how
// much a local slot is holding back. pg_current_wal_lsn() is volatile, so it is not
// evaluated on the branch not taken.
//
// Deliberately unfiltered by name: the caller decides which slots it owns. The agent
// must SEE every physical slot to report retained WAL for all of them (an orphan it
// does not own still fills the disk), even where it will refuse to drop them.
//
// Logical slots are excluded (slot_type='physical'): those belong to the operator's
// subscriptions, the agent never creates or drops them, and enumerating them here
// would invite a caller to treat one as an orphan.
func (p *Prober) PhysicalSlots(ctx context.Context, ci ConnInfo) ([]SlotState, error) {
	out, err := p.psql(ctx, ci,
		"SELECT slot_name, active, COALESCE(pg_wal_lsn_diff("+
			"CASE WHEN pg_is_in_recovery() THEN pg_last_wal_receive_lsn() ELSE pg_current_wal_lsn() END, "+
			"restart_lsn), 0)::bigint, "+
			"COALESCE(wal_status, ''), (restart_lsn IS NOT NULL) "+
			"FROM pg_replication_slots WHERE slot_type = 'physical' ORDER BY slot_name;")
	if err != nil {
		return nil, err
	}
	var slots []SlotState
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 5 {
			return nil, fmt.Errorf("parse pg_replication_slots row %q: want 5 fields, got %d", line, len(parts))
		}
		n, perr := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
		if perr != nil {
			return nil, fmt.Errorf("parse retained WAL bytes %q for slot %q: %w", parts[2], parts[0], perr)
		}
		slots = append(slots, SlotState{
			Name:             strings.TrimSpace(parts[0]),
			Active:           strings.TrimSpace(parts[1]) == "t",
			RetainedWALBytes: n,
			WALStatus:        strings.TrimSpace(parts[3]),
			Reserving:        strings.TrimSpace(parts[4]) == "t",
		})
	}
	return slots, nil
}

// CreatePhysicalSlot creates a physical replication slot if it does not already exist
// (#289).
//
// Two layers, because one is not enough:
//
//   - The WHERE NOT EXISTS guard makes the ordinary repeat call a silent no-op rather than
//     the duplicate-name error pg_create_physical_replication_slot raises. This runs on
//     every primary tick, so without it the log would carry a spurious failure forever
//     once the slot exists.
//   - IsDuplicateSlot on the returned error, because that guard is NOT atomic. Verified
//     against PostgreSQL 18: two callers racing the same not-yet-existing name both pass
//     the NOT EXISTS check and one loses with "already exists" -- reproducibly, in 40 of
//     40 concurrent pairs. Two legitimate, independent creators exist (a cloning standby
//     creating its own slot, and the primary's own periodic reconcile), so this race is
//     reachable at bootstrap and must not surface as a failure.
//
// "The slot exists" is the caller's goal, and both paths achieve it, so both are success.
//
// name is validated before interpolation; it is interpolated into SQL, so an unvalidated
// name would be an injection vector.
func (p *Prober) CreatePhysicalSlot(ctx context.Context, ci ConnInfo, name string) error {
	if err := validSlotName(name); err != nil {
		return err
	}
	out, err := p.psql(ctx, ci, fmt.Sprintf(
		"SELECT pg_create_physical_replication_slot('%s') "+
			"WHERE NOT EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name = '%s');", name, name))
	if err != nil && IsDuplicateSlot(out, err) {
		return nil
	}
	return err
}

// IsDuplicateSlot reports whether a failed slot create lost a create/create race, i.e. the
// slot now exists -- which is the outcome the caller wanted (#289).
//
// Matched on message text because psql surfaces the server error only as combined output
// plus a non-zero exit status; there is no SQLSTATE to read through this transport. Kept
// narrow (the literal phrase PostgreSQL uses for duplicate_object on a slot) so it cannot
// swallow an unrelated failure, and exported so the mechanism layer can apply the same
// judgement to its own clone-time create without duplicating the string.
func IsDuplicateSlot(out string, err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(out + " " + err.Error())
	return strings.Contains(s, "already exists") && strings.Contains(s, "replication slot")
}

// DropPhysicalSlotIfInactive drops a physical slot only when it is not active (#289).
//
// The `AND NOT active` lives in the SQL, not in Go, on purpose: it makes "never drop a
// slot someone is streaming through" atomic with the drop itself. A read-then-decide in
// Go would leave a window where a standby reattaches between the list and the drop, and
// dropping an in-use slot breaks that standby's replication. The same predicate makes
// the statement a no-op (not an error) when the slot is already gone, so a concurrent
// removal or a repeated call is silent rather than a per-tick warning.
//
// Returns whether a row was affected, so the caller can log only real drops. Detecting
// that through psql means the statement has to PROJECT something: pg_drop_replication_slot
// returns void, so selecting it directly prints an empty line for an affected row --
// indistinguishable from the zero-row case under -tA (verified against PostgreSQL 18), and
// the reclaim would then never be logged even though it happened. Hence the CTE plus a
// lateral call: the predicate still lives in SQL, the function is still evaluated exactly
// once per matching slot, and the slot NAME is what comes back.
//
// Verified against PostgreSQL 18: an inactive slot prints its name and is gone; a slot held
// active by a real walsender prints nothing, exits 0, and survives; an absent slot prints
// nothing and exits 0.
func (p *Prober) DropPhysicalSlotIfInactive(ctx context.Context, ci ConnInfo, name string) (dropped bool, err error) {
	if err := validSlotName(name); err != nil {
		return false, err
	}
	out, err := p.psql(ctx, ci, fmt.Sprintf(
		"WITH victim AS (SELECT slot_name FROM pg_replication_slots "+
			"WHERE slot_name = '%s' AND slot_type = 'physical' AND NOT active) "+
			"SELECT v.slot_name FROM victim v, pg_drop_replication_slot(v.slot_name);", name))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// validSlotName rejects anything PostgreSQL would not accept as a slot name, which is
// also exactly the character class that is safe to interpolate into the SQL above.
// PostgreSQL restricts slot names to lower-case letters, digits and underscore; nothing
// in that set can terminate a quoted literal, so a name that passes here cannot inject.
func validSlotName(name string) error {
	if name == "" {
		return fmt.Errorf("replication slot name is empty")
	}
	if len(name) > 63 {
		return fmt.Errorf("replication slot name %q is longer than 63 characters", name)
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return fmt.Errorf("invalid replication slot name %q: only lower-case letters, digits and underscore are allowed", name)
	}
	return nil
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
		if tl, ok, _ := p.StandbyTimeline(ctx, ci); ok {
			ns.Timeline, ns.TimelineOK = tl, true
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
