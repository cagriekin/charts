package mechanism

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Native drives PostgreSQL's own tools as the HA mechanism instead of the repmgr CLI
// (#287): pg_ctl promote, pg_basebackup, pg_rewind, and primary_conninfo + standby.signal.
//
// Policy is unaffected. The Lease is still the sole authority for who is primary, and the
// timeline/LSN election, fencing and Service routing all live in reconcile, which imports
// only the Mechanism interface. Only the mechanics differ -- that is what the seam is for.
//
// EXPERIMENTAL and not usable on its own: topology still comes from repmgr.nodes (#288),
// so a standby has no upstream to choose. Replication slot ownership (#289) HAS landed --
// this mechanism creates its own slot before every clone/rejoin and the primary reconciles
// the set each tick -- so the WAL-recycling gap is closed. #294 promotes it to supported.
//
// Every binary is resolved under PGBindir rather than PATH: the image ships exactly one
// PostgreSQL major and the agent must exec that major's tools, not whatever PATH resolves
// to (#269). pg_rewind and pg_basebackup in particular refuse to work across majors.
type Native struct {
	Runner   Runner
	DataDir  string // PGDATA
	PGBindir string // /usr/lib/postgresql/<major>/bin
	Password string // replication password, supplied to libpq via PGPASSWORD only
	// SlotName is THIS node's physical replication slot on whatever upstream it streams
	// from (#289) -- ordinal-derived and stable across restarts, so a pod that restarts
	// reattaches to the same slot instead of stranding one and reserving a second.
	//
	// It names a slot on the UPSTREAM, not locally: Clone creates it there before the base
	// backup and primary_slot_name makes the walreceiver keep using it. Empty disables slot
	// use entirely (the pre-#289 behaviour), which keeps the mechanism usable in tests and
	// for any caller that has no ordinal to derive a name from.
	SlotName string
	// NodeName is THIS node's pod name, written into primary_conninfo as
	// application_name so the UPSTREAM can tell its standbys apart (#288).
	//
	// Without it every native standby shows up in the primary's pg_stat_replication as
	// application_name = 'walreceiver' -- the libpq default -- so the primary can see HOW
	// MANY standbys are streaming but not WHICH pods they are. That makes
	// pg_stat_replication useless as a topology source, which is exactly what this issue
	// needs it to be. repmgr mode does not have the problem because repmgr injects
	// node_name itself during standby clone.
	//
	// Deliberately not added to Conn.conninfo(): that builder also feeds
	// `repmgr node rejoin -d` and `pg_rewind --source-server`, which are ordinary client
	// connections where an application_name would be noise, and touching it would move the
	// repmgr-mode render. Empty omits the setting.
	NodeName string
	Now      Clock
}

// NewNative builds the native mechanism.
//
// The managed config lives INSIDE PGDATA, not in /etc/postgresql/conf.d: that directory is a
// ConfigMap mounted read-only, so the agent cannot write there. PGDATA is the writable,
// per-node location the chart already appends to at initdb/postStart, it survives restarts
// with the data it describes, and it keeps the agent's fragment clear of both the operator's
// ConfigMap and postgresql.auto.conf (which ALTER SYSTEM and pg_basebackup -R own).
//
// slotName is this node's own slot on its upstream (#289); "" disables slot use.
// nodeName is this node's pod name, published to the upstream as application_name (#288);
// "" omits it.
func NewNative(dataDir, pgBindir, password, slotName, nodeName string) *Native {
	return &Native{
		Runner:   OSRunner{},
		DataDir:  dataDir,
		PGBindir: pgBindir,
		Password: password,
		SlotName: slotName,
		NodeName: nodeName,
		Now:      time.Now,
	}
}

// managedConfName is the agent-owned fragment inside PGDATA, included from
// PGDATA/postgresql.conf. Relative so the include line stays portable if PGDATA moves.
const managedConfName = "pg-ha-agent.conf"

// bin resolves a PostgreSQL binary under the versioned bindir.
func (n *Native) bin(name string) string {
	if n.PGBindir == "" {
		return name // last resort: PATH (tests, or an image without PG_MAJOR)
	}
	return filepath.Join(n.PGBindir, name)
}

// run invokes a binary with PGPASSWORD set. The password is NEVER passed in argv: argv is
// world-readable in the process list, which is exactly the leak #167 was filed for.
func (n *Native) run(ctx context.Context, name string, args ...string) (string, error) {
	var env []string
	if n.Password != "" {
		env = []string{"PGPASSWORD=" + n.Password}
	}
	return n.Runner.Run(ctx, env, name, args...)
}

// managedConfPath is the agent-owned fragment inside PGDATA.
func (n *Native) managedConfPath() string {
	return filepath.Join(n.DataDir, managedConfName)
}

// ensureInclude idempotently appends `include 'pg-ha-agent.conf'` to PGDATA/postgresql.conf.
//
// An include (not include_dir) so it cannot collide with the operator's conf.d include_dir,
// and appended LAST so the agent's replication settings win over anything earlier -- the same
// ordering argument the chart makes for its own authoritative conf.d file. Idempotent: the
// line is added only when absent, so repeated ticks cannot accumulate duplicates the way
// blind appends did.
func (n *Native) ensureInclude() error {
	confPath := filepath.Join(n.DataDir, "postgresql.conf")
	b, err := os.ReadFile(confPath)
	if err != nil {
		return fmt.Errorf("native: read %s: %w", confPath, err)
	}
	line := fmt.Sprintf("include '%s'", managedConfName)
	if hasActiveDirective(string(b), line) {
		return nil
	}
	f, err := os.OpenFile(confPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("native: open %s: %w", confPath, err)
	}
	if _, err := f.WriteString("\n# Managed by pg-ha-agent (native mechanism, #287)\n" + line + "\n"); err != nil {
		f.Close()
		return fmt.Errorf("native: append include to %s: %w", confPath, err)
	}
	// Checked, not deferred-and-dropped: a close failure (e.g. a delayed write-back error
	// surfacing only at close) means the include line's durability is not guaranteed even
	// though the write above returned success, and a missing include silently drops
	// wal_log_hints/hot_standby/primary_conninfo from postgresql.conf.
	if err := f.Close(); err != nil {
		return fmt.Errorf("native: close %s: %w", confPath, err)
	}
	return nil
}

// writeManagedConf writes the agent-owned fragment and makes sure PGDATA/postgresql.conf
// includes it. primaryConninfo is omitted when empty (a primary has no upstream).
func (n *Native) writeManagedConf(primaryConninfo string) error {
	if n.DataDir == "" {
		return fmt.Errorf("native: DataDir not set")
	}
	var b strings.Builder
	b.WriteString("# Managed by pg-ha-agent (native mechanism, #287). Edits are overwritten.\n")
	// wal_log_hints is what makes pg_rewind possible at all; without it a diverged former
	// primary can only be recovered by a full re-clone. The entrypoint sets it at initdb,
	// but assert it here too so a cluster restored from a backup that lacks it still gets
	// the cheap rejoin path rather than silently degrading to re-clone.
	b.WriteString("wal_log_hints = on\n")
	// hot_standby so a standby is queryable -- also how the agent observes its position and
	// what makes the readonly Service useful.
	b.WriteString("hot_standby = on\n")
	if primaryConninfo != "" {
		// application_name identifies THIS node in the upstream's pg_stat_replication (#288),
		// which is what makes that view a usable topology source -- see Native.NodeName. Appended
		// to the conninfo rather than set as a separate GUC because primary_conninfo is what the
		// walreceiver actually dials; a bare application_name GUC would name the postmaster's
		// own sessions, not the replication connection.
		if n.NodeName != "" {
			primaryConninfo = fmt.Sprintf("%s application_name=%s", primaryConninfo, n.NodeName)
		}
		// Single-quoted and escaped: a host or user containing a quote would otherwise break
		// out of the GUC and corrupt the file, failing postmaster start.
		b.WriteString(fmt.Sprintf("primary_conninfo = '%s'\n", escapeSingleQuoted(primaryConninfo)))
		// primary_slot_name is what makes the ONGOING stream hold the slot (#289). Without
		// it the walreceiver connects without a slot, so the upstream is free to recycle WAL
		// this standby has not received yet -- the gap that forces a full re-clone. Written
		// only alongside primary_conninfo: the GUC is meaningless without an upstream, and a
		// primary that carried it would reserve WAL for a stream it never opens.
		if n.SlotName != "" {
			b.WriteString(fmt.Sprintf("primary_slot_name = '%s'\n", escapeSingleQuoted(n.SlotName)))
		}
	}
	if err := writeFileAtomic(n.managedConfPath(), b.String(), 0o600); err != nil {
		return fmt.Errorf("native: write %s: %w", n.managedConfPath(), err)
	}
	return n.ensureInclude()
}

// GenerateConfig writes the managed fragment idempotently.
//
// There is no repmgr.conf analogue: what that file carried (node_id, conninfo, failover
// mode, slot policy) is either repmgr bookkeeping or already known to the agent from its
// environment. What native mode genuinely owns is the replication GUCs that must survive a
// restart, so they live in a rewritten fragment rather than being appended to
// PGDATA/postgresql.conf, where repeated ticks would accumulate duplicates.
//
// Preserves whatever primary_conninfo is already on disk rather than writing "" -- this
// runs once per agent process boot (main.go's boot()), which includes every pod restart on
// an already-cloned standby, not just a fresh node. Forcibly clearing it here would drop a
// working standby's upstream on every restart, self-healing only after the next Follow (a
// needless replication gap). Follow remains the only place that CHANGES it.
func (n *Native) GenerateConfig(ctx context.Context, id NodeIdentity, o ConfigOpts) error {
	return n.writeManagedConf(n.currentPrimaryConninfo())
}

// currentPrimaryConninfo reads back the primary_conninfo already on disk, or "" if the
// managed fragment does not exist yet (fresh node) or carries none (primary).
func (n *Native) currentPrimaryConninfo() string {
	b, err := os.ReadFile(n.managedConfPath())
	if err != nil {
		return ""
	}
	const prefix = "primary_conninfo = '"
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			if v, ok := strings.CutSuffix(rest, "'"); ok {
				return unescapeSingleQuoted(v)
			}
		}
	}
	return ""
}

// Promote turns the local standby into a read-write primary on a new timeline.
//
// -w waits for the promotion to complete, so a caller that immediately re-probes the WAL
// position sees the post-promotion timeline rather than racing it.
func (n *Native) Promote(ctx context.Context) error {
	if n.DataDir == "" {
		return fmt.Errorf("native: DataDir not set")
	}
	if out, err := n.run(ctx, n.bin("pg_ctl"), "-D", n.DataDir, "-w", "promote"); err != nil {
		return fmt.Errorf("native: pg_ctl promote: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// Follow points the local standby at upstream. It only writes files -- applying the
// change is the caller's job, and the two callers need different operations:
//
//   - main.go's act(), on reconcile.Follow: the node is already Running && InRecovery
//     (Decide's precondition for that action), so a reload is correct and sufficient --
//     primary_conninfo is reloadable in modern PostgreSQL, and changing it signals the
//     running WAL receiver to restart against the new target. act() does this reload
//     after every successful Follow.
//   - Clone and RejoinForceRewind, which call Follow themselves on a STOPPED node (one
//     that may have just been a primary, with no standby.signal yet): a reload cannot
//     make a stopped/primary-state postmaster enter recovery at all, only a fresh start
//     picks up a newly-created standby.signal. Both callers follow up with sup.Start.
//
// primary_conninfo is written into the managed fragment (never appended to
// PGDATA/postgresql.conf, which would accumulate a duplicate per repoint) and
// standby.signal is (re)created so a node that was a primary comes back as a standby.
//
// #182: main.go's act() skips calling Follow at all when pg_stat_wal_receiver already
// shows this node streaming from the target, so the reload-path case above is only
// reached when a genuine repoint is needed. That check reads replication state directly
// instead of pattern-matching CLI output the way the repmgr mechanism must -- strictly
// better, and the reason native mode needs no "already following" special case here.
func (n *Native) Follow(ctx context.Context, upstream Conn) error {
	if n.DataDir == "" {
		return fmt.Errorf("native: DataDir not set")
	}
	if upstream.Host == "" {
		// A native follow cannot be addressed by node_id: without a host there is nothing
		// to write into primary_conninfo. Fail loudly rather than write a broken conninfo.
		return fmt.Errorf("native: follow needs upstream.Host (node_id %d is not addressable)", upstream.NodeID)
	}
	// #289: ensure this node's slot exists on the upstream BEFORE pointing at it, the same
	// way Clone and RejoinForceRewind do -- this was the one slot-using path that did not.
	//
	// writeManagedConf below sets primary_slot_name, and a walreceiver whose named slot is
	// missing does NOT fall back to slotless streaming: it errors with `replication slot
	// "..." does not exist` and retries, so the standby streams nothing at all. On a repoint
	// after failover the new primary's own reconcile has usually created it already, but
	// "usually" is the wrong guarantee here -- that create can have failed transiently, or
	// been skipped entirely because the slot list read failed on that tick.
	//
	// Failing the Follow on error is deliberate rather than best-effort: with
	// primary_slot_name pointing at a slot that does not exist the standby cannot stream
	// either way, so a loud error the agent logs and retries beats a "successful" repoint
	// whose only symptom is a walreceiver looping in the postmaster log.
	if err := n.ensureSlotOnUpstream(ctx, upstream); err != nil {
		return err
	}
	if err := n.writeManagedConf(upstream.conninfo()); err != nil {
		return err
	}
	// standby.signal is what makes the postmaster start in recovery. Creating it is
	// idempotent, and it must exist BEFORE the caller restarts the server.
	sig := filepath.Join(n.DataDir, "standby.signal")
	if err := writeFileAtomic(sig, "", 0o600); err != nil {
		return fmt.Errorf("native: create standby.signal: %w", err)
	}
	return nil
}

// Clone builds the local PGDATA fresh from source. The caller guarantees PGDATA is empty or
// has been moved aside (ReclonePreserving does the moving).
//
// -R writes primary_conninfo and standby.signal into the new data directory, so the clone
// streams as soon as it starts without a separate Follow call from the caller -- the #181
// failure mode was a standby that came up after a cutover and never re-established
// streaming. -X stream copies WAL concurrently so the backup is self-consistent without
// depending on WAL still being retained on the primary when the copy finishes.
//
// Deliberately NOT pg_basebackup -R: that writes primary_conninfo/standby.signal into
// postgresql.auto.conf, a SECOND place recovery config can live besides the managed
// fragment Follow writes to. postgresql.auto.conf is included last (initdb appends it to
// the end of postgresql.conf), so it would silently outrank any later Follow -- a standby
// cloned once and later re-pointed to a new upstream would keep streaming from the
// original source. Calling Follow here instead keeps exactly one authoritative place.
// #289: the backup streams through this node's own named slot, created on the source
// FIRST so no WAL gap can open between the base backup starting and the walreceiver
// attaching. Without a slot the source may recycle a segment the new standby still needs
// before it finishes copying, and the clone comes up permanently behind -- recoverable
// only by another full clone. The slot is created idempotently rather than with
// pg_basebackup -C: -C fails outright when the slot already exists (verified), which is
// the normal case for a re-clone of a pod that had one, so -C would break exactly the
// retry path that matters most.
func (n *Native) Clone(ctx context.Context, source Conn) error {
	if n.DataDir == "" {
		return fmt.Errorf("native: DataDir not set")
	}
	if source.Host == "" {
		return fmt.Errorf("native: clone needs source.Host")
	}
	if err := n.ensureSlotOnUpstream(ctx, source); err != nil {
		return err
	}
	args := []string{
		"-h", source.Host,
		"-p", strconv.Itoa(source.port()),
		"-U", source.User,
		"-D", n.DataDir,
		"-X", "stream",
		"--checkpoint=fast",
		"--no-password",
	}
	if n.SlotName != "" {
		args = append(args, "--slot", n.SlotName)
	}
	if out, err := n.run(ctx, n.bin("pg_basebackup"), args...); err != nil {
		return fmt.Errorf("native: pg_basebackup from %s: %w: %s", source.Host, err, strings.TrimSpace(out))
	}
	return n.Follow(ctx, source)
}

// ensureSlotOnUpstream idempotently creates THIS node's slot on the upstream before a
// clone (#289).
//
// Run from the cloning standby rather than left to the primary's own slot reconcile
// because the two race at bootstrap: a fresh standby can reach pg_basebackup before the
// primary's first reconcile tick has created its slot, and pg_basebackup --slot fails on
// a missing slot. Creating it here makes the clone self-sufficient; the primary's
// reconcile then finds it already present and does nothing.
//
// Uses psql (not a Prober) because mechanism must not depend on internal/pg -- the
// dependency runs the other way. The name is validated before interpolation.
func (n *Native) ensureSlotOnUpstream(ctx context.Context, source Conn) error {
	if n.SlotName == "" {
		return nil
	}
	if err := validSlotName(n.SlotName); err != nil {
		return fmt.Errorf("native: clone slot: %w", err)
	}
	sql := fmt.Sprintf(
		"SELECT pg_create_physical_replication_slot('%s') "+
			"WHERE NOT EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name = '%s');",
		n.SlotName, n.SlotName)
	args := []string{
		"-h", source.Host,
		"-p", strconv.Itoa(source.port()),
		"-U", source.User,
		"-d", source.database(),
		"-tAc", sql,
	}
	if out, err := n.run(ctx, n.bin("psql"), args...); err != nil {
		// Losing a create/create race means the slot now EXISTS, which is exactly what this
		// call is for -- so it is success, not failure. The WHERE NOT EXISTS guard above is
		// not atomic (verified: 40 of 40 concurrent pairs race on PostgreSQL 18), and there
		// are two legitimate independent creators of this name -- this cloning standby, and
		// the primary's own periodic reconcile. Propagating the error here would abort an
		// otherwise-fine clone over a slot that is present and usable.
		if isDuplicateSlot(out, err) {
			return nil
		}
		return fmt.Errorf("native: create slot %q on %s: %w: %s", n.SlotName, source.Host, err, strings.TrimSpace(out))
	}
	return nil
}

// isDuplicateSlot mirrors pg.IsDuplicateSlot. Duplicated for the same reason
// validSlotName is: mechanism deliberately does not import internal/pg (that dependency
// runs the other way). Kept narrow so it cannot swallow an unrelated failure.
func isDuplicateSlot(out string, err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(out + " " + err.Error())
	return strings.Contains(s, "already exists") && strings.Contains(s, "replication slot")
}

// validSlotName mirrors internal/pg's guard. Duplicated rather than imported because
// mechanism deliberately does not depend on internal/pg (that import runs the other way,
// and inverting it would couple the mechanics layer to the probe layer). PostgreSQL
// restricts slot names to lower-case letters, digits and underscore -- none of which can
// terminate a quoted SQL literal, so a name that passes cannot inject.
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

// RejoinForceRewind rewinds the diverged local node forward onto target via pg_rewind, then
// leaves it dormant for the supervisor to start as a standby.
//
// Returns ErrRewindDiverged only when pg_rewind genuinely cannot proceed. #178: a transient
// failure to reach the target must NOT be reported as divergence -- that turned a momentary
// connection error into a full re-clone of a healthy node. Connection failures are surfaced
// as a plain error so the caller retries instead of escalating.
//
// pg_rewind needs the source running and this node stopped; the caller has already demoted.
// --restore-target-wal lets it fetch any WAL it needs via restore_command rather than
// failing when the segment has already been recycled locally.
func (n *Native) RejoinForceRewind(ctx context.Context, target Conn) error {
	if n.DataDir == "" {
		return fmt.Errorf("native: DataDir not set")
	}
	if target.Host == "" {
		return fmt.Errorf("native: rewind needs target.Host")
	}
	args := []string{
		"--target-pgdata=" + n.DataDir,
		"--source-server=" + target.conninfo(),
		"--restore-target-wal",
		"--progress",
	}
	out, err := n.run(ctx, n.bin("pg_rewind"), args...)
	if err == nil {
		// Same reasoning as Clone (#289): make sure this node's slot exists on the target
		// BEFORE it starts streaming. The rewind path had been relying on the new primary's
		// own reconcile having already created it -- true almost always (reconcileSlots runs
		// in the same tick as Promote, ahead of the routing switch, while a rejoin is paced
		// by the reconcile interval), but "almost always" is the wrong guarantee for the one
		// thing standing between a standby and a WAL gap. Idempotent, so the overwhelmingly
		// common case where the slot is already there costs one cheap query.
		if err := n.ensureSlotOnUpstream(ctx, target); err != nil {
			return err
		}
		// pg_rewind leaves the node needing standby.signal + primary_conninfo to come back
		// as a standby; write them now so the supervisor's Start attaches it to the target
		// rather than booting a second read-write primary (the two-writer risk).
		if ferr := n.Follow(ctx, target); ferr != nil {
			return fmt.Errorf("native: rewind ok but could not configure recovery: %w", ferr)
		}
		return nil
	}
	if isConnectionFailure(out) {
		// Transient: the caller retries next tick. Reporting ErrRewindDiverged here would
		// trigger a needless full re-clone of a node whose history is probably fine (#178).
		return fmt.Errorf("native: pg_rewind onto %s: could not connect (transient, not divergence): %w: %s",
			target.Host, err, strings.TrimSpace(out))
	}
	return fmt.Errorf("%w: native: pg_rewind onto %s: %v: %s", ErrRewindDiverged, target.Host, err, strings.TrimSpace(out))
}

// ReclonePreserving renames PGDATA aside to .diverged.<ts>, clones from source, and drops
// the backup only on success.
//
// #175: never rm -rf before a clone succeeds. If the clone fails the diverged directory is
// kept and named in the error, so an operator can recover from it -- the alternative lost
// the only copy of the data when the clone then failed.
func (n *Native) ReclonePreserving(ctx context.Context, source Conn) error {
	if n.DataDir == "" {
		return fmt.Errorf("native: DataDir not set")
	}
	backup := fmt.Sprintf("%s.diverged.%s", strings.TrimRight(n.DataDir, "/"), n.Now().UTC().Format("20060102T150405Z"))
	if err := os.Rename(n.DataDir, backup); err != nil {
		return fmt.Errorf("native: reclone: move PGDATA aside to %s: %w", backup, err)
	}
	if err := n.Clone(ctx, source); err != nil {
		return fmt.Errorf("native: reclone: clone failed, diverged data preserved at %s: %w", backup, err)
	}
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("native: reclone: clone ok but could not remove backup %s: %w", backup, err)
	}
	return nil
}

// RegisterPrimary / RegisterStandby / Unregister are no-ops in native mode: repmgr.nodes is
// repmgr's own bookkeeping, and native mode does not maintain it.
//
// They are no-ops rather than errors because reconcile calls them unconditionally as part of
// role reconciliation, and native mode's answer is "nothing to do" rather than "that failed".
// Until #288 moves topology onto pg_stat_replication, native mode therefore has NO topology
// source -- which is precisely why the mechanism is experimental and #288 blocks its use.
func (n *Native) RegisterPrimary(ctx context.Context) error { return nil }

func (n *Native) RegisterStandby(ctx context.Context, upstreamNodeID int) error { return nil }

func (n *Native) Unregister(ctx context.Context, nodeID int) error { return nil }

// isConnectionFailure recognises pg_rewind/libpq output that means "could not reach the
// source", as opposed to "histories diverged beyond repair". Keeping these apart is what
// stops a transient blip from escalating into a full re-clone (#178).
func isConnectionFailure(out string) bool {
	s := strings.ToLower(out)
	for _, m := range []string{
		"could not connect",
		"connection refused",
		"could not translate host name",
		"no route to host",
		"connection timed out",
		"server closed the connection unexpectedly",
		"terminating connection due to administrator command",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// escapeSingleQuoted escapes a value for inclusion in a single-quoted postgresql.conf GUC.
func escapeSingleQuoted(v string) string { return strings.ReplaceAll(v, "'", "''") }

// unescapeSingleQuoted reverses escapeSingleQuoted, for reading a GUC value back out of a
// single-quoted postgresql.conf line.
func unescapeSingleQuoted(v string) string { return strings.ReplaceAll(v, "''", "'") }

// hasActiveDirective reports whether directive appears as its own uncommented line in
// conf, ignoring leading/trailing whitespace. A raw substrings.Contains would also match
// a commented-out directive (e.g. an operator who disabled it with a leading '#', or an
// occurrence inside an unrelated comment) and wrongly conclude it is already active --
// ensureInclude would then skip re-adding it, silently dropping wal_log_hints/
// hot_standby/primary_conninfo from postgresql.conf while still reporting success.
func hasActiveDirective(conf, directive string) bool {
	for _, line := range strings.Split(conf, "\n") {
		if strings.TrimSpace(line) == directive {
			return true
		}
	}
	return false
}

// writeFileAtomic writes via a temp file + rename so a crash mid-write cannot leave the
// postmaster reading a half-written config (which would fail its next start).
func writeFileAtomic(path, content string, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
