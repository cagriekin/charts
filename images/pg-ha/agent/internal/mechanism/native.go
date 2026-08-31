package mechanism

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cagriekin/pg-ha-agent/internal/atomicfile"
)

// Native drives PostgreSQL's own tools as the HA mechanism instead of the repmgr CLI
// (#287): pg_ctl promote, pg_basebackup, pg_rewind, and primary_conninfo + standby.signal.
//
// Policy is unaffected. The Lease is still the sole authority for who is primary, and the
// timeline/LSN election, fencing and Service routing all live in reconcile, which imports
// only the Mechanism interface. Only the mechanics differ -- that is what the seam is for.
//
// EXPERIMENTAL, but it runs a real multi-node cluster since #288: topology comes from
// pg_stat_replication (identified by the application_name this mechanism writes into
// primary_conninfo, or by the ordinal-named slot), and the bootstrap is the agent's -- the
// lease holder initdbs, everyone else clones. Slot ownership (#289) creates this node's slot
// before every clone/rejoin -- on whichever upstream this node actually points at, which is
// what makes cascadingReplication work here since #294 (the reclaim policy on both sides is
// cascade-aware, and syncReplicationSlots resolves standbys from the owned slot set rather
// than repmgr.nodes). Remaining gap: an existing repmgr cluster cannot be migrated in place
// yet (#292). #294 also flips the default.
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

// runConn invokes a binary that connects to peer c, with PGPASSWORD and -- crucially --
// PGCONNECT_TIMEOUT set.
//
// libpq's default connect_timeout is UNLIMITED: it waits out the kernel's SYN retries, ~127s
// on Linux's default tcp_syn_retries=6. Every command here addressed its peer with -h/-p/-U
// rather than a conninfo string, so none of them carried the connect_timeout that Conn already
// specifies, and a blackholed upstream (a dead node whose pod has not been evicted yet, so the
// address still resolves and nothing answers) blocked the reconcile goroutine for those two
// minutes with opMu held and no heartbeat. /healthz goes stale at reconcileInterval*3 = 15s and
// the liveness probe gives up after 10 failures at 10s spacing (~115s), so the kubelet SIGKILLs
// an agent that is PID 1 over a perfectly healthy postmaster -- taking the standby down, and
// repeating for as long as the partition lasts (#298 review).
//
// Every OTHER remote call on this goroutine is already bounded for exactly this reason: the
// prober sets both PGCONNECT_TIMEOUT and a per-call ctx deadline, and the apiserver calls run
// under fenceBudget. These were the exceptions.
func (n *Native) runConn(ctx context.Context, c Conn, name string, args ...string) (string, error) {
	env := []string{fmt.Sprintf("PGCONNECT_TIMEOUT=%d", c.connectTimeoutSecs())}
	if n.Password != "" {
		env = append(env, "PGPASSWORD="+n.Password)
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
	const header = "# Managed by pg-ha-agent (native mechanism, #287)"
	// REPOSITION rather than skip when the line is already present (#288 review). The agent's
	// fragment must be the LAST include so its replication settings win, and an
	// append-only-if-absent check cannot maintain that: a native cluster created with no conf.d
	// feature has the agent's include at the end, and enabling postgresql.configuration later
	// makes the setup-config init container append include_dir AFTER it on the next pod start.
	// From then on an operator's wal_log_hints or hot_standby would silently override the
	// agent's -- removing the cheap pg_rewind rejoin path -- and the inverted file would be
	// cloned verbatim to every standby.
	if hasActiveDirective(string(b), line) && isLastActiveDirective(string(b), line) {
		return nil
	}
	// ONE ATOMIC WRITE (#288 review). An earlier revision truncated the file and then re-appended
	// in a second O_APPEND write, which is unsafe twice over: between the two writes
	// postgresql.conf carries no include at all, and act() issues pg_reload_conf() after every
	// successful Follow on a RUNNING standby (as does the chart's postStart hook) -- a reload
	// landing in that window drops primary_conninfo, primary_slot_name, hot_standby and
	// wal_log_hints and stops the walreceiver. A crash or ENOSPC mid-truncate also leaves a
	// postgresql.conf the postmaster will not start on at all. writeFileAtomic exists in this
	// file for exactly that reason.
	body := stripActiveDirective(string(b), line)
	// Drop the orphaned header the strip would otherwise leave behind, so repeated repositions
	// do not accumulate them.
	body = stripActiveDirective(body, header)
	body = strings.TrimRight(body, "\n")
	if err := atomicfile.WriteString(confPath, body+"\n\n"+header+"\n"+line+"\n", 0o600); err != nil {
		return fmt.Errorf("native: rewrite %s with the managed include last: %w", confPath, err)
	}
	return nil
}

// isLastActiveDirective reports whether line is the final non-comment, non-blank directive in
// conf. PostgreSQL applies includes in file order, so "last" is what decides precedence (#288).
func isLastActiveDirective(conf, line string) bool {
	var last string
	for _, l := range strings.Split(conf, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		last = t
	}
	return last == line
}

// stripActiveDirective removes every active occurrence of line, so the caller can re-append it
// at the end (#288).
func stripActiveDirective(conf, line string) string {
	out := make([]string, 0, len(strings.Split(conf, "\n")))
	for _, l := range strings.Split(conf, "\n") {
		if strings.TrimSpace(l) == line {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
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
	//
	// Written on a PRIMARY too, where PostgreSQL ignores it (#298 review flagged this as
	// cosmetic; it is deliberate). hot_standby is postmaster-only, so a node that acquires it
	// only when it becomes a standby needs a RESTART to apply it -- and the moment that matters
	// is a demote, where the agent stops, reconfigures and starts a node that must come up
	// queryable immediately or the readonly Service has a hole in it. Keeping the fragment
	// role-independent also means demote/promote never rewrite this file, so a reload can never
	// race a role change over it.
	b.WriteString("hot_standby = on\n")
	if primaryConninfo != "" {
		// application_name identifies THIS node in the upstream's pg_stat_replication (#288),
		// which is what makes that view a usable topology source -- see Native.NodeName. Appended
		// to the conninfo rather than set as a separate GUC because primary_conninfo is what the
		// walreceiver actually dials; a bare application_name GUC would name the postmaster's
		// own sessions, not the replication connection.
		// Idempotent: GenerateConfig feeds currentPrimaryConninfo() -- the value already on
		// disk -- straight back in, so an unconditional append accumulated a copy on every
		// agent boot (#288 review). A standby that is already streaming never re-enters Follow
		// (the streamingFromTarget latch short-circuits it), so nothing rewrote it cleanly:
		// after four boots the GUC read `... application_name=pg-1 application_name=pg-1
		// application_name=pg-1 application_name=pg-1`. libpq takes the last, so replication
		// kept working while the value grew without bound.
		if n.NodeName != "" {
			// Keyed on THIS node's name, not on the presence of any application_name (#288
			// review). pg_rewind copies the SOURCE's pg-ha-agent.conf into the target PGDATA, so
			// if the agent dies after the rewind but before Follow finishes, the next boot feeds
			// the source pod's name straight back through currentPrimaryConninfo(). A
			// presence-only guard would preserve it forever: two senders would resolve to one
			// pod, and the real pod would be logged as "not streaming" indefinitely while
			// unidentified stayed 0. Strip any foreign value and write our own.
			// TOKEN comparison, not a substring test (#288 review). `strings.Contains` for
			// "application_name=pg-1" is satisfied by an inherited "application_name=pg-10", so
			// with >=11 replicas the foreign value was kept -- exactly the case this guard
			// exists to defeat, and invisible to a test using pg-0/pg-1.
			want := "application_name=" + n.NodeName
			if !hasField(primaryConninfo, want) {
				primaryConninfo = stripApplicationName(primaryConninfo) + " " + want
			}
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
	if err := atomicfile.WriteString(n.managedConfPath(), b.String(), 0o600); err != nil {
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
	cur, err := n.currentPrimaryConninfo()
	if err != nil {
		// Only ENOENT means "fresh node, or a primary that never followed". Any OTHER read
		// error (EIO, EACCES, a half-mounted volume) used to collapse to the same "" and be
		// COMMITTED: writeManagedConf then rewrote the fragment with no primary_conninfo and no
		// primary_slot_name, so a working standby lost its upstream. boot() calls this before
		// starting the postmaster, so the node comes up in recovery attached to nobody until
		// the first Follow repoints it. Refusing preserves the documented contract that Follow
		// is the only thing that CHANGES this value (#298 review).
		return fmt.Errorf("native: generate config: could not read the managed fragment, so the current upstream cannot be preserved: %w", err)
	}
	return n.writeManagedConf(cur)
}

// hasField reports whether conninfo contains want as a whole space-separated token.
func hasField(conninfo, want string) bool {
	for _, f := range strings.Fields(conninfo) {
		if f == want {
			return true
		}
	}
	return false
}

// stripApplicationName removes any application_name=<value> token from a conninfo, so the local
// node's own can replace a value inherited from another node (#288 review).
func stripApplicationName(conninfo string) string {
	fields := strings.Fields(conninfo)
	kept := fields[:0]
	for _, f := range fields {
		if strings.HasPrefix(f, "application_name=") {
			continue
		}
		kept = append(kept, f)
	}
	return strings.Join(kept, " ")
}

// currentPrimaryConninfo reads back the primary_conninfo already on disk, or "" if the
// managed fragment does not exist yet (fresh node) or carries none (primary).
//
// A MISSING file is "" with no error -- that is the fresh-node case. Every other read error
// is returned, because the caller commits this value and "" means "clear the upstream".
func (n *Native) currentPrimaryConninfo() (string, error) {
	b, err := os.ReadFile(n.managedConfPath())
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("native: read %s: %w", n.managedConfPath(), err)
	}
	const prefix = "primary_conninfo = '"
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			if v, ok := strings.CutSuffix(rest, "'"); ok {
				return unescapeSingleQuoted(v), nil
			}
		}
	}
	return "", nil
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
		return fmt.Errorf("native: follow needs upstream.Host to write primary_conninfo, and it is empty")
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
	// standby.signal is what makes the postmaster start in recovery, and it is created FIRST
	// -- ahead of the fallible slot ensure and conf write -- because it is the only step whose
	// ABSENCE is dangerous (#298 review).
	//
	// Clone calls Follow on a directory that pg_basebackup just filled from a running primary:
	// its pg_control says "in production" and it has no standby.signal, so until this file
	// exists the data reads as primary-state. A transient failure below would otherwise leave
	// a COMPLETED clone (kept deliberately -- discardTornClone's no-marker branch) in primary
	// shape, and the next tick would take it for a diverged ex-primary: pg_rewind refuses a
	// target that was not shut down cleanly, which escalated to ReclonePreserving and re-ran
	// the entire multi-hour base backup. Same shape after a rewind, where the node genuinely
	// was a primary moments ago.
	//
	// Ordering it first is safe in the other direction too: standby.signal without
	// primary_conninfo starts a standby that simply waits for WAL, whereas conninfo without
	// standby.signal starts a SECOND read-write primary -- the two-writer risk. Creating it is
	// idempotent, so the reload path (already Running && InRecovery) is unaffected.
	sig := filepath.Join(n.DataDir, "standby.signal")
	if err := atomicfile.WriteString(sig, "", 0o600); err != nil {
		return fmt.Errorf("native: create standby.signal: %w", err)
	}
	if err := n.ensureSlotOnUpstream(ctx, upstream); err != nil {
		return err
	}
	if err := n.writeManagedConf(upstream.conninfo()); err != nil {
		return err
	}
	return nil
}

// Clone builds the local PGDATA fresh from source. The caller guarantees PGDATA is empty or
// has been moved aside (ReclonePreserving does the moving).
//
// -X stream copies WAL concurrently, so the backup is self-consistent without depending on WAL
// still being retained on the primary when the copy finishes.
//
// Deliberately NOT pg_basebackup -R: that writes primary_conninfo/standby.signal into
// postgresql.auto.conf, a SECOND place recovery config can live besides the managed fragment
// Follow writes to. auto.conf is read last, so it would silently outrank any later Follow -- a
// standby cloned once and re-pointed to a new upstream would keep streaming from the original
// source. Calling Follow here instead keeps exactly one authoritative place. (The agent now also
// REFUSES to start on a data directory whose auto.conf already sets those GUCs, which is how a
// 1.x repmgr volume is caught -- see assertNoForeignRecoveryConfig, #294.)
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
	// runConn, not run: pg_basebackup is addressed with -h/-p/-U, so without PGCONNECT_TIMEOUT
	// its connect phase is unbounded (#298 review). No ctx deadline, deliberately -- a base
	// backup of a large cluster legitimately runs for hours, and the connect bound is what this
	// path actually needed.
	if out, err := n.runConn(ctx, source, n.bin("pg_basebackup"), args...); err != nil {
		return fmt.Errorf("native: pg_basebackup from %s: %w: %s", source.Host, err, strings.TrimSpace(out))
	}
	return n.Follow(ctx, source)
}

// slotEnsureTimeout is the total budget for the slot-create query, generous enough that a
// merely slow upstream is not failed (the connect bound is the tighter one at 10s) but far
// inside the ~115s in which the liveness probe would kill the agent.
const slotEnsureTimeout = 30 * time.Second

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
	// slot_type = 'physical' in the guard, mirroring Prober.CreatePhysicalSlot (#298 review).
	// Unscoped, a LOGICAL slot squatting on this ordinal's name suppressed the create
	// SILENTLY: the statement returns zero rows with a nil error, so this reports success and
	// Follow then writes primary_slot_name pointing at it. The walreceiver loops forever on
	// "cannot use a logical replication slot for physical replication" and no layer of the
	// agent says why.
	//
	// The collision is RAISEd with a message of our OWN, not left to PostgreSQL's (#298
	// review, round 2). Scoping the guard alone surfaced nothing: the create then runs and
	// pg_create_physical_replication_slot fails with the very phrase `replication slot "..."
	// already exists`, which isDuplicateSlot matches -- so the error was swallowed below and
	// this reported success exactly as before. A distinct message (deliberately WITHOUT the
	// words "already exists") makes the squatter fatal, while a genuine physical/physical
	// race still raises PostgreSQL's own phrase and stays success. Mirrors
	// Prober.CreatePhysicalSlot.
	//
	// An INVALIDATED slot is recycled rather than accepted, mirroring
	// Prober.CreatePhysicalSlot (#298 review). wal_status = 'lost' means PostgreSQL
	// destroyed the reservation at max_slot_wal_keep_size (4GB in this image) and the slot
	// can never be acquired again -- yet the existence guard below is satisfied by it, so
	// the create was skipped with a nil error and BOTH recovery paths wedged: Follow wrote
	// primary_slot_name at a slot the walreceiver cannot acquire, and the stall escalation
	// through ReclonePreserving handed the same name to `pg_basebackup --slot`, which fails
	// at its WAL-stream connect after PGDATA has already been renamed aside to an unreaped
	// .diverged.<ts>. The primary's own reclaim pass cannot help either -- orphanSlot
	// deliberately keeps any slot whose ordinal still has a live pod -- so the standby
	// stayed out of the cluster until an operator dropped the slot by hand. Guarded on NOT
	// active, so only a reservation nothing holds is ever removed.
	sql := fmt.Sprintf(
		"DO $do$ BEGIN IF EXISTS (SELECT 1 FROM pg_replication_slots "+
			"WHERE slot_name = '%[1]s' AND slot_type <> 'physical') THEN "+
			"RAISE EXCEPTION 'replication slot %[1]s is not a physical slot, so it cannot be "+
			"used for streaming replication: drop it or free the name'; END IF; "+
			"IF EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name = '%[1]s' "+
			"AND slot_type = 'physical' AND wal_status = 'lost' AND NOT active) THEN "+
			"PERFORM pg_drop_replication_slot('%[1]s'); END IF; END $do$; "+
			"SELECT pg_create_physical_replication_slot('%[1]s') "+
			"WHERE NOT EXISTS (SELECT 1 FROM pg_replication_slots "+
			"WHERE slot_name = '%[1]s' AND slot_type = 'physical');",
		n.SlotName)
	// --no-psqlrc, for the reason Prober.psql and RehashMd5User both pass it (#298 review):
	// PSQLRC reaches this process through postgresql.extraEnv (childenv.Filtered strips only
	// *PASSWORD*) and the agent writes into the postgres home, so ~/.psqlrc is reachable too.
	// A startup file that errors makes psql exit non-zero on a query that in fact succeeded,
	// and isDuplicateSlot does not match its message -- so an otherwise healthy clone aborts
	// over the slot it was creating.
	args := []string{
		"--no-psqlrc",
		"-h", source.Host,
		"-p", strconv.Itoa(source.port()),
		"-U", source.User,
		"-d", source.database(),
		"-tAc", sql,
	}
	// Two bounds, because they cover different hangs (#298 review). PGCONNECT_TIMEOUT (via
	// runConn) bounds the CONNECT phase, which is what a blackholed peer stalls on; the ctx
	// deadline bounds the total call, for a server that completes the TCP handshake and then
	// never answers -- an upstream wedged on an exclusive lock, or one whose backend start is
	// blocked. One cheap query has no business taking longer than this, and the whole point is
	// that it runs under opMu on the reconcile goroutine, where an unbounded wait is a
	// liveness-probe kill rather than a slow tick.
	ctx, cancel := context.WithTimeout(ctx, slotEnsureTimeout)
	defer cancel()
	if out, err := n.runConn(ctx, source, n.bin("psql"), args...); err != nil {
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
func (n *Native) RejoinForceRewind(ctx context.Context, target Conn) (rerr error) {
	if n.DataDir == "" {
		return fmt.Errorf("native: DataDir not set")
	}
	if target.Host == "" {
		return fmt.Errorf("native: rewind needs target.Host")
	}
	// standby.signal has to come OUT before pg_rewind runs, or the cheap rewind path is dead
	// for every standby-originated rejoin (#298 review).
	//
	// rejoinOnto always demotes with fence=true, i.e. Immediate/SIGQUIT, so the target (this
	// node's own PGDATA) is left in DB_IN_ARCHIVE_RECOVERY -- not cleanly shut down. Since
	// PG13, pg_rewind handles that by running `postgres --single` on the target to finish
	// crash recovery, unless --no-ensure-shutdown is passed (it is not, anywhere). And
	// readRecoverySignalFile refuses single-user mode outright when standby.signal exists:
	// `FATAL: standby mode is not supported by single-user servers`, so pg_rewind aborts with
	// `postgres single-user mode in target cluster failed`.
	//
	// That message is neither a divergence nor a connection failure, so RejoinForward returned
	// a plain error every time, rejoinOnto counted three of them and escalated to
	// ReclonePreserving: a full base backup plus a .diverged.<ts> copy nothing reaps, on every
	// ordinary "standby on an older timeline" rejoin. Deterministic, not racy.
	//
	// Restored on ANY error, including a rewind that worked but whose Follow did not. The
	// asymmetry is Follow's own: standby.signal without primary_conninfo is a standby waiting
	// for WAL, while its absence on a rewound directory is a SECOND read-write primary.
	sig := filepath.Join(n.DataDir, "standby.signal")
	hadSignal := false
	if _, serr := os.Stat(sig); serr == nil {
		if err := os.Remove(sig); err != nil {
			return fmt.Errorf("native: rewind: could not move standby.signal aside for pg_rewind's crash-recovery step: %w", err)
		}
		hadSignal = true
	}
	defer func() {
		if rerr == nil || !hadSignal {
			return // success: Follow below re-created it
		}
		if werr := atomicfile.WriteString(sig, "", 0o600); werr != nil {
			rerr = fmt.Errorf("%w (and could not restore standby.signal, so this node must not be started read-write: %v)", rerr, werr)
		}
	}()
	base := []string{
		"--target-pgdata=" + n.DataDir,
		"--source-server=" + target.conninfo(),
		"--progress",
	}
	// --source-server carries connect_timeout already; PGCONNECT_TIMEOUT (via runConn) is
	// belt-and-braces so every peer-addressed command in this file is bounded the same way.
	out, err := n.runConn(ctx, target, n.bin("pg_rewind"), append([]string{"--restore-target-wal"}, base...)...)
	// --restore-target-wal is a REQUEST that pg_rewind refuses outright when the target has no
	// restore_command, and it refuses before doing any work: `pg_rewind: error:
	// "restore_command" is not set in the target cluster`. The chart only sets restore_command
	// when pgbackrest is enabled, so on a cluster without it EVERY rejoin failed here -- and
	// the old classifier called that failure divergence, so the caller "recovered" by
	// re-cloning the whole node. The rewind path had therefore never actually run in native
	// mode without pgbackrest; a graceful failover paid a full base backup to bring the
	// ex-primary back (#298 review, observed live in the failover suite).
	//
	// Retried without the flag rather than gated on a chart value: pg_rewind's own diagnostic is
	// the authority on whether the target can fetch archived WAL, it stays correct if
	// restore_command appears or disappears later, and the mechanism layer has no business
	// knowing which chart feature configures archiving. Without the flag pg_rewind reads what it
	// needs from the target's pg_wal, which is enough whenever the needed segments have not been
	// recycled -- the overwhelmingly common case for a node that was primary moments ago.
	if err != nil && strings.Contains(out, `"restore_command" is not set in the target cluster`) {
		out, err = n.runConn(ctx, target, n.bin("pg_rewind"), base...)
	}
	if err == nil {
		// pg_rewind leaves the node needing standby.signal + primary_conninfo to come back
		// as a standby; write them now so the supervisor's Start attaches it to the target
		// rather than booting a second read-write primary (the two-writer risk).
		//
		// Follow -- not a bare ensureSlotOnUpstream in front of it (#298 review). Follow
		// already ensures this node's slot on the upstream (#289, same reasoning as Clone:
		// the slot must exist before the stream opens or the primary is free to recycle WAL
		// this node still needs), and it does so AFTER creating standby.signal, which is the
		// whole point of that ordering. Calling the slot ensure here instead put the one
		// fallible remote query AHEAD of standby.signal on a directory pg_rewind has just
		// left in primary shape: a momentary blip against the freshly-promoted target -- the
		// single likeliest moment for one -- returned before any recovery config was written,
		// and the next tick then took the StartLocal "standby-state data, stopped" branch and
		// started a postmaster with neither standby.signal nor primary_conninfo.
		if ferr := n.Follow(ctx, target); ferr != nil {
			return fmt.Errorf("native: rewind ok but could not configure recovery: %w", ferr)
		}
		return nil
	}
	if isRewindDivergence(out) {
		return fmt.Errorf("%w: native: pg_rewind onto %s: %v: %s", ErrRewindDiverged, target.Host, err, strings.TrimSpace(out))
	}
	if isConnectionFailure(out) || isSourceRejection(out) {
		// Transient: the caller retries next tick. Reporting ErrRewindDiverged here would
		// trigger a needless full re-clone of a node whose history is probably fine (#178).
		//
		// Tagged ErrRewindUnreachable rather than left a plain error (#298 review). The
		// classification was computed here and then thrown away -- only the message differed --
		// so rejoinOnto counted it toward rewindFailureLimit like any other non-divergence
		// failure: three ticks (~15s on chart defaults) of a postmaster that was restarting, a
		// pod name that had not propagated, a source at max_connections or a missing pg_hba
		// entry escalated a healthy non-diverged standby to ReclonePreserving. Both classes --
		// the transport never connected, or the source connected and then refused us -- are
		// unreachability for this purpose: see the sentinel and isSourceRejection for why
		// escalating on either converges on nothing.
		return fmt.Errorf("%w: native: pg_rewind onto %s: could not obtain a usable connection to the source (not divergence): %w: %s",
			ErrRewindUnreachable, target.Host, err, strings.TrimSpace(out))
	}
	// Anything else is NOT divergence either, and the DEFAULT matters more than either list.
	// Defaulting to ErrRewindDiverged would send every failure absent from the two lists above
	// -- "target server must be shut down cleanly", a wal_log_hints/data-checksums complaint,
	// "target server needs to exit backup mode", a restore_command error -- through a PGDATA
	// move and a re-clone of a node whose history is fine, inverting the contract #178
	// established (see this function's doc comment). Those examples are all TARGET-side, which
	// is also why they are the ones that keep counting toward rewindFailureLimit: a re-clone
	// replaces exactly the local data directory they describe, so escalating converges.
	return fmt.Errorf("native: pg_rewind onto %s failed for a reason that is not divergence, so the data directory is left alone; retrying: %w: %s",
		target.Host, err, strings.TrimSpace(out))
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
		// Logged, NOT returned (#298 review). The node is fully re-cloned and configured by
		// this point; only the cleanup of the preserved copy failed -- reachable on any store
		// that silly-renames open files (NFS leaves .nfsXXXX entries, so RemoveAll returns
		// ENOTEMPTY). rejoinOnto treats ANY error here as a failed rejoin: it calls
		// discardTornClone and returns without sup.Start, so a healthy fresh clone is left
		// stopped and the next tick re-runs the whole rejoin -- demote, three pg_rewind
		// attempts, another multi-hour base backup, and a SECOND .diverged.<ts> copy on the
		// same PVC. Leaving one stale directory behind for an operator is strictly cheaper.
		slog.Warn("native: reclone: clone succeeded but the preserved copy could not be removed; remove it by hand once you no longer need it",
			"path", backup, "err", err)
	}
	return nil
}

// isRewindDivergence recognises the ONE thing pg_rewind says when the two histories cannot be
// reconciled: it could not find a common ancestor of the source and target timelines. That, and
// only that, is what ErrRewindDiverged means to the caller, and the caller's response is
// destructive -- ReclonePreserving moves PGDATA aside and re-clones from scratch.
//
// Positive detection, rather than "anything not on the transient list" (#298 review). The
// asymmetry is the whole argument, and it is the same one standbyStallTicks is sized by:
//
//   - Calling a NON-divergence failure divergence destroys a healthy standby's data directory,
//     leaves a .diverged.<ts> copy nothing removes (so repeated attempts fill the PVC), and
//     re-runs a full base backup. Cost: hours of degraded HA for a node that needed a retry.
//   - Failing to recognise a GENUINE divergence leaves a standby that keeps retrying the rewind
//     every stall window (~3 minutes) and logging pg_rewind's own message verbatim. Cost: that
//     standby stays behind until an operator reads the log; nothing is destroyed, nothing else
//     in the cluster is affected, and the alerts for a non-streaming standby already exist.
//
// So an unrecognised message must fall on the second side, which means the divergence list --
// not the transient list -- is the one that has to be exact.
//
// Deliberately NOT treated as divergence: "target server must be shut down cleanly" (a state
// error the caller can fix -- pg_rewind's own `postgres --single` step normally handles it,
// which is why RejoinForceRewind takes standby.signal out of the way first; Follow's
// signal-first ordering makes the ABSENCE of that file safe, it does not make the file
// compatible with single-user recovery), "wal_log_hints"/data-checksums complaints (a config problem a reclone would
// reproduce), and "from different systems" (a DIFFERENT cluster -- invariant 9's
// assertSameCluster refuses before the rewind, and cloning from it would be the real disaster).
func isRewindDivergence(out string) bool {
	// pg_rewind: "could not find common ancestor of the source and target cluster's timelines".
	// Matched on the distinctive fragment so a wording change across majors, or a prefix from
	// the surrounding progress output, still classifies.
	return strings.Contains(strings.ToLower(out), "could not find common ancestor")
}

// isConnectionFailure recognises pg_rewind/libpq output that means "could not reach the
// source", as opposed to "histories diverged beyond repair". Keeping these apart is what
// stops a transient blip from escalating into a full re-clone (#178).
//
// This list does not decide whether to reclone IMMEDIATELY -- isRewindDivergence does, and
// everything unrecognised is retried rather than escalated. It DOES, together with
// isSourceRejection, decide the ErrRewindUnreachable tag, and rejoinOnto exempts that tag from
// rewindFailureLimit -- so a match here also keeps the failure out of the re-clone backstop
// entirely (#298 review). See the sentinel's doc comment for why that exemption is right.
func isConnectionFailure(out string) bool {
	s := strings.ToLower(out)
	for _, m := range []string{
		"could not connect",
		"connection refused",
		"could not translate host name",
		"no route to host",
		"connection timed out",
		// libpq's own connect_timeout expiry says "timeout expired", NOT "connection
		// timed out" (that is the kernel ETIMEDOUT strerror, which conninfo()'s
		// connect_timeout preempts) -- so the most common transient outage during a
		// rewind was misclassified as divergence and re-cloned (#298 review).
		"timeout expired",
		"network is unreachable", // ENETUNREACH: routing down, not divergence
		"server closed the connection unexpectedly",
		"terminating connection due to administrator command",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// isSourceRejection reports that the SOURCE accepted the TCP connection and then refused the
// session: the rewind never got to look at either history.
//
// Split from isConnectionFailure but treated identically by the caller (#298 review), because
// the question that decides escalation is not "is this transient" -- it is "would
// ReclonePreserving fix it". A re-clone dials THE SAME source with THE SAME credentials through
// THE SAME pg_hba, so every rejection below refuses the pg_basebackup exactly as it refused the
// rewind. Escalating buys a PGDATA rename, a failed base backup, and an unreaped
// `.diverged.<ts>` copy on the PVC -- and still no rejoin.
//
// That is why permanence is not the criterion. `no pg_hba.conf entry` and a rotated credential
// are permanent until an operator acts, and escalating on them is STILL pointless; whereas the
// conditions the backstop legitimately converges are the ones on the TARGET side -- `target
// server must be shut down cleanly`, `wal_log_hints`, `target server needs to exit backup mode`
// -- because a re-clone replaces the local directory those describe. Those deliberately fall
// through to the default branch and keep counting toward rewindFailureLimit.
//
// Retrying is visible, not silent: rejoinOnto returns the error every tick, so it increments
// pg_ha_agent_reconcile_errors_total and logs the server's own words -- which names the fix
// (raise max_connections, add the pg_hba entry, wait for the promote to finish) far better than
// a multi-hour re-clone that cannot succeed.
func isSourceRejection(out string) bool {
	s := strings.ToLower(out)
	for _, m := range []string{
		"sorry, too many clients already",
		"the database system is starting up",
		"the database system is shutting down",
		"the database system is in recovery mode",
		"no pg_hba.conf entry",
		"password authentication failed",
		"authentication failed for user",
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
