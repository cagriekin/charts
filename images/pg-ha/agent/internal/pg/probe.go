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

	"github.com/cagriekin/pg-ha-agent/internal/childenv"
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
	// Strip the agent's own credential env from what psql inherits (#298 security
	// review): it authenticates via the PGPASSWORD in env, never the raw agent secrets.
	cmd.Env = childenv.Filtered(os.Environ(), env)
	// WaitDelay bounds the gap between killing the process and Wait returning. Without it a
	// cancelled context kills only the direct child, while Wait blocks forever on the stdout
	// copy: any GRANDCHILD still holding the pipe keeps it from reaching EOF. That is not
	// hypothetical here -- `entrypoint.sh initdb` runs `pg_ctl -w start`, which daemonizes a
	// postmaster that inherits the pipe, so a deadline expiring mid-bootstrap would hang the
	// caller indefinitely, holding opMu, stopping the reconcile heartbeat and preventing
	// dcs.OnLost from ever fencing (#288 review).
	cmd.WaitDelay = 10 * time.Second
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
	// --no-psqlrc, as RehashMd5User already passes (#298 review). Every caller here parses
	// psql's OUTPUT SHAPE -- `strings.Cut(out, "|")`, an exact "t"/"f", a bare LSN for
	// ParseLSN -- and a startup file can change all of it (`\pset fieldsep`, `\pset null`,
	// a banner) while psql still exits 0. PSQLRC reaches this process through
	// postgresql.extraEnv and childenv.Filtered strips only *PASSWORD*, and the agent itself
	// writes into the postgres home (writePgpass), so ~/.psqlrc is reachable too. The failure
	// is silent and cluster-wide: LSNOK false on every node, so survivor ranking has no
	// positions to compare and the failover simply never happens.
	args := []string{"--no-psqlrc", "-h", ci.Host, "-p", strconv.Itoa(ci.Port), "-U", ci.User, "-d", ci.DB, "-tAc", sql}
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
	// ParseInt, not ParseUint (#298 review). pg_control_system() exposes
	// system_identifier as int8 (PostgreSQL has no unsigned types), so once the high
	// bit is set the column renders as a NEGATIVE decimal -- while pg_controldata,
	// which ReadControlData parses for the LOCAL id, prints the same 64 bits with
	// UINT64_FORMAT. initdb builds the identifier as ((uint64) tv.tv_sec) << 32 | ...,
	// so every cluster initdb'd from 2038-01-19 on has that bit set. ParseUint rejected
	// the negative rendering, ok came back false for EVERY peer, and assertSameCluster
	// -- which is fail-open on !ok -- stopped enforcing invariant 9 entirely: a
	// misrouted pod from a different cluster would no longer be refused as a
	// clone/rewind source. Reinterpreting the signed value as uint64 reproduces
	// pg_controldata's rendering for all inputs, so the two sides compare equal.
	n, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if perr != nil {
		return 0, false, nil
	}
	return uint64(n), true, nil
}

// ReplicaRow is one row of the primary's view of its streaming standbys (#288).
//
// Both identity fields are returned RAW and unresolved: mapping either to a pod name needs
// the StatefulSet's base name and the slot-naming convention, which are the agent's business,
// not this package's. See the resolution in cmd/agent.
type ReplicaRow struct {
	// AppName is pg_stat_replication.application_name. Under the native mechanism the agent
	// writes the pod name here via primary_conninfo (#288). It is "walreceiver" -- libpq's
	// default -- for any standby whose primary_conninfo predates that, which is the case the
	// SlotName fallback exists for.
	AppName string
	// SlotName is the slot the standby is streaming through, recovered by joining
	// pg_replication_slots on active_pid, or "" for a slotless stream. Because native names
	// slots by pod ordinal (#289), this identifies the pod even when AppName does not.
	SlotName string
	// State is pg_stat_replication.state: "streaming", "catchup", "startup", "backup".
	State string
}

// Streaming reports whether this replica is actually caught up and streaming, as opposed to
// still catching up or backing up. Callers deciding topology should care about the
// distinction: a "catchup" standby exists but cannot yet serve or be promoted safely.
func (r ReplicaRow) Streaming() bool { return r.State == "streaming" }

// ReplicationTopology reads the primary's own view of who is streaming from it (#288).
//
// This replaces repmgr.nodes as the topology source. That table was a CACHE of self-reported
// metadata: nodes registered themselves into it, rows outlived the pods that wrote them (#139),
// and it could disagree with both the lease and the observed positions. pg_stat_replication
// cannot go stale in that way -- it is the primary's live connection list, so a departed pod is
// simply absent and there is no row to strand.
//
// The join is what makes a row identifiable. application_name carries the pod name under
// native mode, but a standby cloned before #288 still dials with libpq's default
// ("walreceiver"), so pg_replication_slots.active_pid recovers the pod from the ordinal-named
// slot instead. Verified against real PostgreSQL 18 with both shapes streaming to one primary
// at once: `pg-1|pg_ha_slot_1|streaming` alongside `walreceiver|pg_ha_slot_2|streaming`.
//
// PHYSICAL SENDERS ONLY. pg_stat_replication has one row per WAL sender, logical decoding
// senders included, so a cluster with a subscription, Debezium or pg_recvlogical would otherwise
// contribute rows whose application_name is the subscription name and whose slot is logical --
// unresolvable to any pod, inflating the streaming count and pinning "unidentified" above zero
// forever. That would defeat the whole point of that gauge, which is to say the topology view is
// incomplete. The join is restricted to physical slots and any pid holding a logical slot is
// excluded outright.
//
// PRIMARY-ONLY, and it must not become a promotion gate on its own. pg_stat_replication is the
// mirror of the pg_stat_wal_receiver caveat above: a standby's row VANISHES the instant it
// disconnects, which is exactly the failover moment when a promotion decision is being made.
// Absence here means "not streaming right now", never "this node does not exist".
func (p *Prober) ReplicationTopology(ctx context.Context, ci ConnInfo) ([]ReplicaRow, error) {
	out, err := p.psql(ctx, ci,
		"SELECT r.application_name, COALESCE(s.slot_name, ''), r.state "+
			"FROM pg_stat_replication r "+
			"LEFT JOIN pg_replication_slots s ON s.active_pid = r.pid AND s.slot_type = 'physical' "+
			"WHERE NOT EXISTS (SELECT 1 FROM pg_replication_slots l "+
			"WHERE l.active_pid = r.pid AND l.slot_type = 'logical') "+
			// ORDER BY is not load-bearing for any caller -- every consumer indexes the rows by
			// application_name (#298 review asked whether it meant something). It is here so the
			// row order is stable: pg_stat_replication's natural order follows walsender
			// start-up, so an unordered read made this probe's log lines and the peer list it
			// feeds reshuffle on every reconnect, and a topology diff between two ticks showed
			// churn that had not happened.
			"ORDER BY 1, 2;")
	if err != nil {
		return nil, err
	}
	var rows []ReplicaRow
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 3 {
			return nil, fmt.Errorf("parse pg_stat_replication row %q: want at least 3 fields, got %d", line, len(parts))
		}
		// EXTRA separators belong to application_name, which is the FIRST column and the only
		// operator-settable one (#288 review). pg_stat_replication is a cluster-wide view this
		// chart does not control: one unrelated client whose application_name contains a '|' --
		// third-party tooling sets it freely -- would otherwise fail the whole read and blind all
		// three gauges on every tick. slot_name is restricted to [a-z0-9_] and state is a fixed
		// enum, so the LAST two fields are unambiguous and everything before them is the name.
		rows = append(rows, ReplicaRow{
			AppName:  strings.TrimSpace(strings.Join(parts[:len(parts)-2], "|")),
			SlotName: strings.TrimSpace(parts[len(parts)-2]),
			State:    strings.TrimSpace(parts[len(parts)-1]),
		})
	}
	return rows, nil
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
//
// The reference LSN uses GREATEST(receive, replay) in recovery, not receive alone (#294
// review). pg_last_wal_receive_lsn() is NULL until streaming starts in THIS postmaster
// lifetime, so on a demoted ex-primary that never reattached -- precisely the node
// standbySlotsTick exists for, holding leftover pg_ha_slot_* with no walreceiver -- the whole
// expression collapsed to 0 and every slot reported no retained WAL. That made
// PGHAReplicationSlotRetainingWAL unable to fire on the one case it was written for. The outer
// GREATEST(..., 0) additionally clamps the negative a post-rewind restart_lsn ahead of the
// local end-of-WAL produces, which slotMetrics' max would otherwise carry silently.
func (p *Prober) PhysicalSlots(ctx context.Context, ci ConnInfo) ([]SlotState, error) {
	out, err := p.psql(ctx, ci,
		"SELECT slot_name, active, GREATEST(COALESCE(pg_wal_lsn_diff("+
			"CASE WHEN pg_is_in_recovery() THEN GREATEST(COALESCE(pg_last_wal_receive_lsn(), '0/0'), COALESCE(pg_last_wal_replay_lsn(), '0/0')) ELSE pg_current_wal_lsn() END, "+
			"restart_lsn), 0), 0)::bigint, "+
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
	// slot_type = 'physical' in the guard, matching PhysicalSlots and
	// DropPhysicalSlotIfInactive (#298 review). Without it a LOGICAL slot squatting on the
	// name suppressed the create silently: the statement returned zero rows with a nil error,
	// so reconcileSlots logged "created replication slot for an expected standby" and re-ran
	// the same no-op every tick forever, because PhysicalSlots -- which IS filtered -- never
	// reported the name back into its `have` map. Meanwhile the standby whose
	// primary_slot_name is that ordinal could not stream at all ("cannot use a logical
	// replication slot for physical replication") and no layer of the agent said why.
	//
	// The collision is RAISEd with a message of our OWN, not left to PostgreSQL's (#298
	// review, round 2). Scoping the guard alone did not surface anything: the create then
	// runs and pg_create_physical_replication_slot fails with the very phrase
	// `replication slot "..." already exists`, which IsDuplicateSlot matches -- so the
	// error was swallowed and this returned nil exactly as before, leaving the silent
	// per-tick no-op the scoping was meant to end. A distinct message (deliberately
	// WITHOUT the words "already exists") makes the squatter fatal while a genuine
	// physical/physical race still raises PostgreSQL's own phrase and is still treated as
	// success below -- which is the one case that IS success.
	// An INVALIDATED slot is recycled, not accepted (#298 review). wal_status = 'lost'
	// means PostgreSQL destroyed the reservation because the slot passed
	// max_slot_wal_keep_size (4GB in this image), and such a slot can never be acquired
	// again -- SlotState.Invalidated() and PGHAReplicationSlotInvalidated already model
	// exactly this state, and probe.go documents the remedy as a full re-clone. But the
	// existence guard below is satisfied by it, so the create was skipped with a nil
	// error and every recovery path wedged: Follow wrote primary_slot_name at a slot the
	// walreceiver cannot acquire, and the escalation to ReclonePreserving passed the same
	// name to `pg_basebackup --slot`, which fails at its WAL-stream connect -- after
	// PGDATA had already been renamed aside to an unreaped .diverged.<ts>. Nothing else
	// reclaimed it either: orphanSlot deliberately keeps any slot whose ordinal has a
	// live pod. Dropping the dead reservation here lets the create below mint a usable
	// one, which is the whole point of an idempotent ensure.
	//
	// Guarded on NOT active, so this can only ever remove a reservation nothing holds.
	out, err := p.psql(ctx, ci, fmt.Sprintf(
		"DO $do$ BEGIN IF EXISTS (SELECT 1 FROM pg_replication_slots "+
			"WHERE slot_name = '%[1]s' AND slot_type <> 'physical') THEN "+
			"RAISE EXCEPTION 'replication slot %[1]s is not a physical slot, so it cannot be "+
			"used for streaming replication: drop it or free the name'; END IF; "+
			"IF EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name = '%[1]s' "+
			"AND slot_type = 'physical' AND wal_status = 'lost' AND NOT active) THEN "+
			"PERFORM pg_drop_replication_slot('%[1]s'); END IF; END $do$; "+
			"SELECT pg_create_physical_replication_slot('%[1]s') "+
			"WHERE NOT EXISTS (SELECT 1 FROM pg_replication_slots "+
			"WHERE slot_name = '%[1]s' AND slot_type = 'physical');", name))
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

// SetSynchronizedStandbySlots sets synchronized_standby_slots on the LOCAL primary via
// ALTER SYSTEM and reloads (#308), so a logical failover slot's decode position is held
// back until the named physical standby slot(s) have synced. slots is joined with ",",
// PostgreSQL's own list format for this GUC; an empty slots means "no slots required",
// itself a meaningful value (no active standbys), not an error.
//
// Two separate psql invocations, not one "ALTER SYSTEM ...; SELECT pg_reload_conf();" --
// confirmed live that PostgreSQL sends multiple semicolon-separated statements in one
// simple-query message as an implicit transaction block, and ALTER SYSTEM refuses to run
// inside one ("ALTER SYSTEM cannot run inside a transaction block"). The reload is
// skipped if ALTER SYSTEM fails, so a broken value is never silently left unreloaded
// alongside a stale (but valid) running config.
//
// Each slot name is validated against repmgr's own [A-Za-z0-9_]+ convention HERE, not
// left to the caller: slots is interpolated directly into the ALTER SYSTEM statement
// text, not passed as a bind parameter, because ALTER SYSTEM SET does not accept one
// for its value -- enforcing it in the one function that actually builds that string
// means a future second call site cannot bypass it by omission.
func (p *Prober) SetSynchronizedStandbySlots(ctx context.Context, ci ConnInfo, slots []string) error {
	for _, s := range slots {
		// #289's validSlotName (a func returning error) replaces #308's regexp var of the same
		// name: it is the stricter and more accurate rule -- PostgreSQL restricts slot names to
		// lower-case letters, digits and underscore -- and it carries its own messages.
		if err := validSlotName(s); err != nil {
			return fmt.Errorf("refusing to set synchronized_standby_slots: %w", err)
		}
	}
	alter := fmt.Sprintf("ALTER SYSTEM SET synchronized_standby_slots = '%s';", strings.Join(slots, ","))
	if _, err := p.psql(ctx, ci, alter); err != nil {
		return fmt.Errorf("alter system: %w", err)
	}
	if _, err := p.psql(ctx, ci, "SELECT pg_reload_conf();"); err != nil {
		return fmt.Errorf("reload after alter system: %w", err)
	}
	return nil
}

// SSLActive reports what the RUNNING postmaster answers for `SHOW ssl`, which is the only
// ground truth for "is this server actually speaking TLS" (#335).
//
// Every other signal an operator has -- the rendered ConfigMap, the mounted Secret, the values
// file -- describes INTENT, and #335 is precisely the case where intent and reality diverge in
// silence: the conf.d include never reached postgresql.conf, so `ssl = on` was written, mounted,
// and never read. `SHOW` reports the value the postmaster is running with, after every include,
// ALTER SYSTEM and command-line override, so it cannot be fooled by any of them.
//
// A non-boolean answer is reported as an error rather than as false: this drives a fail-closed
// readiness/alerting path, and "the query did not work" must not be indistinguishable from
// "TLS is off" -- the caller retries the former and escalates the latter.
func (p *Prober) SSLActive(ctx context.Context, ci ConnInfo) (bool, error) {
	out, err := p.psql(ctx, ci, "SHOW ssl;")
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(out) {
	case "on":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("unexpected `SHOW ssl` result %q", out)
	}
}
