package pg

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeExec dispatches on a distinctive token in the SQL (the last arg) so each
// probe query can be stubbed independently.
type fakeExec struct {
	role      string // pg_is_in_recovery output ("t"/"f"/"")
	primary   string // pg_walfile_name(...)|pg_current_wal_lsn() output
	standby   string // GREATEST(receive, replay) output
	standbyTL string // StandbyTimeline GREATEST(checkpoint TL, min_recovery_end_timeline) output (decimal)
	sysID     string // pg_control_system() system_identifier output (decimal)
	walRcv    string // StreamingUpstream "sender_host|status" output
	nodes     string // SELECT node_id FROM repmgr.nodes output (newline-separated)
	err       error  // if set, every call fails (node unreachable)
}

func (f fakeExec) Run(_ context.Context, _ []string, _ string, args ...string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	sql := args[len(args)-1]
	switch {
	case strings.Contains(sql, "repmgr.nodes"):
		return f.nodes, nil
	// StandbyTimeline reads GREATEST(checkpoint TL, min_recovery_end_timeline); match
	// it on the distinctive recovery field.
	case strings.Contains(sql, "min_recovery_end_timeline"):
		return f.standbyTL, nil
	case strings.Contains(sql, "sender_host"):
		return f.walRcv, nil
	case strings.Contains(sql, "pg_is_in_recovery"):
		return f.role, nil
	case strings.Contains(sql, "pg_control_system"):
		return f.sysID, nil
	case strings.Contains(sql, "pg_control_checkpoint"):
		return f.standbyTL, nil
	case strings.Contains(sql, "pg_last_wal_receive_lsn"):
		return f.standby, nil
	case strings.Contains(sql, "pg_walfile_name"):
		return f.primary, nil
	}
	return "", nil
}

func proberWith(f fakeExec) *Prober { return &Prober{Exec: f} }

func TestProbePrimary(t *testing.T) {
	p := proberWith(fakeExec{role: "f", primary: "0000000A|16/B374D848"})
	ns := p.Probe(context.Background(), ConnInfo{Host: "pod-0"})
	if !ns.Reachable || ns.Role != RolePrimary {
		t.Fatalf("got reachable=%v role=%v", ns.Reachable, ns.Role)
	}
	if !ns.TimelineOK || ns.Timeline != 10 {
		t.Errorf("timeline = (%d, ok=%v), want 10", ns.Timeline, ns.TimelineOK)
	}
	if !ns.LSNOK || ns.WriteLSN.Hi != 0x16 || ns.WriteLSN.Lo != 0xB374D848 {
		t.Errorf("writeLSN = (%+v, ok=%v)", ns.WriteLSN, ns.LSNOK)
	}
}

func TestProbeStandby(t *testing.T) {
	p := proberWith(fakeExec{role: "t", standby: "16/B374D840", standbyTL: "7"})
	ns := p.Probe(context.Background(), ConnInfo{Host: "pod-1"})
	if !ns.Reachable || ns.Role != RoleStandby {
		t.Fatalf("got reachable=%v role=%v", ns.Reachable, ns.Role)
	}
	// A running standby MUST report its timeline (control-file, decimal), else
	// unsafeToServe would refuse to ever promote it and failover would livelock.
	if !ns.TimelineOK || ns.Timeline != 7 {
		t.Errorf("standby timeline = (%d, ok=%v), want 7", ns.Timeline, ns.TimelineOK)
	}
	if !ns.LSNOK || ns.WriteLSN.Hi != 0x16 || ns.WriteLSN.Lo != 0xB374D840 {
		t.Errorf("receive LSN = (%+v, ok=%v)", ns.WriteLSN, ns.LSNOK)
	}
}

// A standby whose control-file timeline is unreadable must report TimelineOK=false
// (so the highwater guard fails closed), not a bogus 0.
func TestProbeStandbyTimelineUnreadable(t *testing.T) {
	p := proberWith(fakeExec{role: "t", standby: "16/B374D840", standbyTL: ""})
	ns := p.Probe(context.Background(), ConnInfo{Host: "pod-1"})
	if ns.TimelineOK {
		t.Errorf("unreadable standby timeline must be TimelineOK=false, got %d", ns.Timeline)
	}
}

// sqlCaptureExec records the last SQL it was asked to run and returns a fixed result.
type sqlCaptureExec struct {
	lastSQL string
	ret     string
}

func (e *sqlCaptureExec) Run(_ context.Context, _ []string, _ string, args ...string) (string, error) {
	e.lastSQL = args[len(args)-1]
	return e.ret, nil
}

// StandbyTimeline must read the recovery-end timeline
// (pg_control_recovery.min_recovery_end_timeline) GREATEST'd with the checkpoint
// timeline -- a standby that has followed a new timeline by streaming but not yet
// checkpointed reports the new timeline, and that signal PERSISTS in the control file
// after the upstream dies (unlike pg_stat_wal_receiver, which vanishes at the failover
// moment), so the #125 highwater guard does not reject a caught-up standby after a
// pg_rewind rejoin (#178). Guards against reverting to the bare control-file read or
// to the transient walreceiver field.
func TestStandbyTimelineUsesRecoveryTimeline(t *testing.T) {
	e := &sqlCaptureExec{ret: "2"}
	p := &Prober{Exec: e}
	tl, ok, err := p.StandbyTimeline(context.Background(), ConnInfo{Host: "x"})
	if err != nil || !ok || tl != 2 {
		t.Fatalf("got tl=%d ok=%v err=%v, want 2", tl, ok, err)
	}
	for _, needle := range []string{"min_recovery_end_timeline", "pg_control_checkpoint", "GREATEST"} {
		if !strings.Contains(e.lastSQL, needle) {
			t.Errorf("StandbyTimeline query missing %q: %s", needle, e.lastSQL)
		}
	}
	if strings.Contains(e.lastSQL, "pg_stat_wal_receiver") {
		t.Errorf("StandbyTimeline must not depend on the transient pg_stat_wal_receiver: %s", e.lastSQL)
	}
}

func TestProbeUnreachable(t *testing.T) {
	p := proberWith(fakeExec{err: errors.New("connection refused")})
	ns := p.Probe(context.Background(), ConnInfo{Host: "pod-2"})
	if ns.Reachable || ns.Role != RoleUnknown || ns.LSNOK || ns.TimelineOK {
		t.Errorf("unreachable node must be zero-valued: %+v", ns)
	}
}

func TestProbeUnexpectedRoleOutput(t *testing.T) {
	// A reachable node returning garbage for the role is not classifiable and must
	// be treated as unreachable, never as a primary.
	p := proberWith(fakeExec{role: "ERROR: something"})
	ns := p.Probe(context.Background(), ConnInfo{Host: "pod-3"})
	if ns.Reachable || ns.Role == RolePrimary {
		t.Errorf("unparseable role must not classify as reachable/primary: %+v", ns)
	}
}

func TestStreamingUpstream(t *testing.T) {
	t.Run("streaming reports host + true", func(t *testing.T) {
		p := proberWith(fakeExec{walRcv: "pg-0.h|streaming"})
		host, streaming, err := p.StreamingUpstream(context.Background(), ConnInfo{Host: "x"})
		if err != nil || !streaming || host != "pg-0.h" {
			t.Fatalf("got (host=%q streaming=%v err=%v), want pg-0.h/true/nil", host, streaming, err)
		}
	})
	t.Run("no walreceiver row -> not streaming", func(t *testing.T) {
		p := proberWith(fakeExec{walRcv: ""})
		host, streaming, err := p.StreamingUpstream(context.Background(), ConnInfo{Host: "x"})
		if err != nil || streaming || host != "" {
			t.Fatalf("got (host=%q streaming=%v err=%v), want empty/false/nil", host, streaming, err)
		}
	})
	t.Run("connecting (not yet streaming) -> false", func(t *testing.T) {
		p := proberWith(fakeExec{walRcv: "pg-0.h|connecting"})
		_, streaming, err := p.StreamingUpstream(context.Background(), ConnInfo{Host: "x"})
		if err != nil || streaming {
			t.Fatalf("a non-streaming status must report false, got streaming=%v err=%v", streaming, err)
		}
	})
	t.Run("unreachable -> err", func(t *testing.T) {
		p := proberWith(fakeExec{err: errors.New("connection refused")})
		if _, _, err := p.StreamingUpstream(context.Background(), ConnInfo{Host: "x"}); err == nil {
			t.Fatal("an unreachable node must return an error")
		}
	})
}

func TestSystemIdentifier(t *testing.T) {
	p := proberWith(fakeExec{sysID: "7395000000000000001"})
	id, ok, err := p.SystemIdentifier(context.Background(), ConnInfo{Host: "pod-1"})
	if err != nil || !ok || id != 7395000000000000001 {
		t.Fatalf("got (id=%d ok=%v err=%v), want 7395000000000000001", id, ok, err)
	}
}

func TestSystemIdentifierUnreachable(t *testing.T) {
	p := proberWith(fakeExec{err: errors.New("connection refused")})
	if _, ok, err := p.SystemIdentifier(context.Background(), ConnInfo{Host: "x"}); ok || err == nil {
		t.Errorf("unreachable peer must return ok=false + err, got ok=%v err=%v", ok, err)
	}
}

func TestSystemIdentifierUnparseable(t *testing.T) {
	p := proberWith(fakeExec{sysID: "not-a-number"})
	if _, ok, err := p.SystemIdentifier(context.Background(), ConnInfo{Host: "x"}); ok || err != nil {
		t.Errorf("unparseable id must return ok=false, nil err, got ok=%v err=%v", ok, err)
	}
}

func TestStandbyNodeIDs(t *testing.T) {
	p := proberWith(fakeExec{nodes: "1000\n1001\n1002\n"})
	ids, err := p.StandbyNodeIDs(context.Background(), ConnInfo{Host: "x", DB: "repmgr"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := []int{1000, 1001, 1002}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
}

func TestStandbyNodeIDsEmpty(t *testing.T) {
	p := proberWith(fakeExec{nodes: ""})
	if ids, err := p.StandbyNodeIDs(context.Background(), ConnInfo{Host: "x"}); err != nil || ids != nil {
		t.Fatalf("empty result must return (nil, nil), got (%v, %v)", ids, err)
	}
}

func TestStandbyNodeIDsUnparseable(t *testing.T) {
	// A malformed node_id must surface as an error, never be silently dropped: a
	// dropped row could hide a real ghost (or, inversely, make a live node look absent).
	p := proberWith(fakeExec{nodes: "1000\nbogus\n"})
	if _, err := p.StandbyNodeIDs(context.Background(), ConnInfo{Host: "x"}); err == nil {
		t.Fatal("a malformed node_id must surface as an error")
	}
}

func TestStandbyNodeIDsUnreachable(t *testing.T) {
	p := proberWith(fakeExec{err: errors.New("connection refused")})
	if _, err := p.StandbyNodeIDs(context.Background(), ConnInfo{Host: "x"}); err == nil {
		t.Fatal("an unreachable node must return an error")
	}
}

// slotExec records the SQL it is asked to run and replies with a canned result, so the
// slot helpers can be asserted on BOTH the statement they build and the value they parse.
type slotExec struct {
	out  string
	err  error
	sqls []string
}

func (s *slotExec) Run(_ context.Context, _ []string, _ string, args ...string) (string, error) {
	s.sqls = append(s.sqls, args[len(args)-1])
	return s.out, s.err
}

// The three states verified against real PostgreSQL 18, in the order the query returns
// them: a slot created ahead of its standby (wal_status NULL -> "", restart_lsn NULL),
// a healthy reserving one, and one PostgreSQL invalidated for exceeding
// max_slot_wal_keep_size (wal_status "lost", restart_lsn back to NULL -- which is why
// its retained-bytes figure reads 0 and cannot be alerted on).
func TestPhysicalSlotsParsesNameActiveRetainedWALAndWALStatus(t *testing.T) {
	ex := &slotExec{out: "pg_ha_slot_1|f|0||f\npg_ha_slot_2|t|16777216|reserved|t\npg_ha_slot_3|f|0|lost|f"}
	p := &Prober{Exec: ex}
	got, err := p.PhysicalSlots(context.Background(), ConnInfo{})
	if err != nil {
		t.Fatalf("PhysicalSlots: %v", err)
	}
	want := []SlotState{
		{Name: "pg_ha_slot_1", Active: false, RetainedWALBytes: 0, WALStatus: "", Reserving: false},
		{Name: "pg_ha_slot_2", Active: true, RetainedWALBytes: 16777216, WALStatus: "reserved", Reserving: true},
		{Name: "pg_ha_slot_3", Active: false, RetainedWALBytes: 0, WALStatus: "lost", Reserving: false},
	}
	if got[2].Invalidated() != true || got[1].Invalidated() != false {
		t.Errorf("Invalidated() misreads wal_status: %+v", got)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d slots, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("slot %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// The listing has to work on a STANDBY too, since a demoted primary reclaims the slots it
// minted while it was primary. pg_current_wal_lsn() is primary-only and raises "recovery is
// in progress" on a standby (verified against PostgreSQL 18), which returned an EMPTY
// listing there and made those leftovers invisible.
func TestPhysicalSlotsQueryWorksOnAStandby(t *testing.T) {
	ex := &slotExec{}
	p := &Prober{Exec: ex}
	if _, err := p.PhysicalSlots(context.Background(), ConnInfo{}); err != nil {
		t.Fatalf("PhysicalSlots: %v", err)
	}
	sql := ex.sqls[0]
	if !strings.Contains(sql, "pg_is_in_recovery()") {
		t.Errorf("query does not branch on recovery, so it errors out on a standby: %s", sql)
	}
	if !strings.Contains(sql, "pg_last_wal_receive_lsn()") {
		t.Errorf("query has no standby reference LSN: %s", sql)
	}
}

// Logical slots belong to the operator's subscriptions; the agent must never enumerate
// them as candidates for its own reconcile.
func TestPhysicalSlotsQueryExcludesLogicalSlots(t *testing.T) {
	ex := &slotExec{}
	p := &Prober{Exec: ex}
	if _, err := p.PhysicalSlots(context.Background(), ConnInfo{}); err != nil {
		t.Fatalf("PhysicalSlots: %v", err)
	}
	if !strings.Contains(ex.sqls[0], "slot_type = 'physical'") {
		t.Errorf("query does not filter to physical slots: %s", ex.sqls[0])
	}
}

// A NULL restart_lsn (freshly created slot, no WAL reserved yet) must read as 0 rather
// than erroring the whole listing -- otherwise one new slot hides every other slot's
// retained WAL.
func TestPhysicalSlotsEmptyOutputIsNoSlotsNotAnError(t *testing.T) {
	p := &Prober{Exec: &slotExec{out: ""}}
	got, err := p.PhysicalSlots(context.Background(), ConnInfo{})
	if err != nil {
		t.Fatalf("PhysicalSlots: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want no slots", got)
	}
}

// A malformed row is an error, not a silent skip: silently dropping a row would hide an
// orphaned slot that is filling the disk.
func TestPhysicalSlotsMalformedRowIsAnError(t *testing.T) {
	p := &Prober{Exec: &slotExec{out: "pg_ha_slot_1|t"}}
	if _, err := p.PhysicalSlots(context.Background(), ConnInfo{}); err == nil {
		t.Fatal("want an error for a 2-field row, got nil")
	}
}

func TestPhysicalSlotsUnparseableRetainedWALIsAnError(t *testing.T) {
	p := &Prober{Exec: &slotExec{out: "pg_ha_slot_1|t|notanumber"}}
	if _, err := p.PhysicalSlots(context.Background(), ConnInfo{}); err == nil {
		t.Fatal("want an error for a non-numeric retained-WAL value, got nil")
	}
}

// The create must be idempotent in SQL so a per-tick reconcile does not log a duplicate
// -name failure forever once the slot exists.
func TestCreatePhysicalSlotIsIdempotentInSQL(t *testing.T) {
	ex := &slotExec{}
	p := &Prober{Exec: ex}
	if err := p.CreatePhysicalSlot(context.Background(), ConnInfo{}, "pg_ha_slot_1"); err != nil {
		t.Fatalf("CreatePhysicalSlot: %v", err)
	}
	sql := ex.sqls[0]
	if !strings.Contains(sql, "pg_create_physical_replication_slot('pg_ha_slot_1')") {
		t.Errorf("does not create the named slot: %s", sql)
	}
	if !strings.Contains(sql, "WHERE NOT EXISTS") {
		t.Errorf("create is not guarded against an existing slot: %s", sql)
	}
}

// "never drop an active slot" must be enforced by the statement itself: a read-then-drop
// in Go would let a standby reattach in the window between the two.
func TestDropPhysicalSlotRefusesActiveSlotsInSQL(t *testing.T) {
	ex := &slotExec{out: ""}
	p := &Prober{Exec: ex}
	if _, err := p.DropPhysicalSlotIfInactive(context.Background(), ConnInfo{}, "pg_ha_slot_9"); err != nil {
		t.Fatalf("DropPhysicalSlotIfInactive: %v", err)
	}
	sql := ex.sqls[0]
	if !strings.Contains(sql, "NOT active") {
		t.Errorf("drop is not guarded on inactivity: %s", sql)
	}
	if !strings.Contains(sql, "slot_type = 'physical'") {
		t.Errorf("drop is not restricted to physical slots: %s", sql)
	}
}

// The statement must PROJECT the slot name, not the drop function's own result.
// pg_drop_replication_slot returns void, so `SELECT pg_drop_replication_slot(slot_name)
// FROM ...` prints an empty line for an affected row under psql -tA -- byte-identical to
// the zero-row case (verified against PostgreSQL 18). Under that shape the drop still
// happens but is reported as dropped=false, so the reclaim is never logged and the only
// observable record of it disappears. This pins the shape that actually reports it.
func TestDropPhysicalSlotProjectsTheSlotNameNotTheVoidResult(t *testing.T) {
	ex := &slotExec{out: ""}
	p := &Prober{Exec: ex}
	if _, err := p.DropPhysicalSlotIfInactive(context.Background(), ConnInfo{}, "pg_ha_slot_9"); err != nil {
		t.Fatalf("DropPhysicalSlotIfInactive: %v", err)
	}
	sql := ex.sqls[0]
	if strings.Contains(sql, "SELECT pg_drop_replication_slot(") {
		t.Errorf("drop projects the void function result, so an affected row is indistinguishable from none: %s", sql)
	}
	if !strings.Contains(sql, "SELECT v.slot_name") {
		t.Errorf("drop does not project the slot name, so it cannot report what it reclaimed: %s", sql)
	}
}

// An empty result means the slot was already gone or is active -- both are "nothing was
// dropped", not an error the caller should warn about every tick.
func TestDropPhysicalSlotReportsWhetherARowWasAffected(t *testing.T) {
	p := &Prober{Exec: &slotExec{out: ""}}
	dropped, err := p.DropPhysicalSlotIfInactive(context.Background(), ConnInfo{}, "pg_ha_slot_9")
	if err != nil || dropped {
		t.Errorf("empty result: got dropped=%v err=%v, want false/nil", dropped, err)
	}
	p = &Prober{Exec: &slotExec{out: "pg_ha_slot_9"}}
	dropped, err = p.DropPhysicalSlotIfInactive(context.Background(), ConnInfo{}, "pg_ha_slot_9")
	if err != nil || !dropped {
		t.Errorf("non-empty result: got dropped=%v err=%v, want true/nil", dropped, err)
	}
}

func TestDropPhysicalSlotPropagatesQueryErrors(t *testing.T) {
	p := &Prober{Exec: &slotExec{err: errors.New("boom")}}
	if _, err := p.DropPhysicalSlotIfInactive(context.Background(), ConnInfo{}, "pg_ha_slot_9"); err == nil {
		t.Fatal("want the query error propagated, got nil")
	}
}

// Slot names are interpolated into SQL, so anything outside PostgreSQL's own accepted
// character class must be refused before it reaches the statement.
func TestSlotNameValidationRejectsInjectionAndAcceptsRealNames(t *testing.T) {
	bad := []string{
		"",
		"pg_ha_slot_1'; DROP TABLE x; --",
		"pg_ha_slot_1;",
		"PG_HA_SLOT_1", // upper case: PostgreSQL would reject it too
		"pg-ha-slot-1",
		"pg ha slot 1",
		strings.Repeat("a", 64),
	}
	for _, n := range bad {
		if err := validSlotName(n); err == nil {
			t.Errorf("validSlotName(%q) = nil, want an error", n)
		}
	}
	for _, n := range []string{"pg_ha_slot_0", "pg_ha_slot_12", "repmgr_slot_1001", strings.Repeat("a", 63)} {
		if err := validSlotName(n); err != nil {
			t.Errorf("validSlotName(%q) = %v, want nil", n, err)
		}
	}
}

// The guard must run BEFORE the SQL is built, so a rejected name never reaches psql.
func TestSlotHelpersRejectBadNamesWithoutQuerying(t *testing.T) {
	ex := &slotExec{}
	p := &Prober{Exec: ex}
	if err := p.CreatePhysicalSlot(context.Background(), ConnInfo{}, "bad; name"); err == nil {
		t.Error("CreatePhysicalSlot accepted an invalid name")
	}
	if _, err := p.DropPhysicalSlotIfInactive(context.Background(), ConnInfo{}, "bad; name"); err == nil {
		t.Error("DropPhysicalSlotIfInactive accepted an invalid name")
	}
	if len(ex.sqls) != 0 {
		t.Errorf("an invalid name reached psql: %v", ex.sqls)
	}
}

// A create that loses a create/create race means the slot now EXISTS, which is the
// caller's goal -- so it must be reported as success, not failure. The WHERE NOT EXISTS
// guard is NOT atomic (verified against PostgreSQL 18: 40 of 40 concurrent pairs raced),
// and there are two legitimate independent creators (a cloning standby and the primary's
// own reconcile), so this path is genuinely reachable at bootstrap.
func TestCreatePhysicalSlotToleratesLosingACreateRace(t *testing.T) {
	dup := &slotExec{out: `ERROR:  replication slot "pg_ha_slot_1" already exists`, err: errors.New("exit status 1")}
	p := &Prober{Exec: dup}
	if err := p.CreatePhysicalSlot(context.Background(), ConnInfo{}, "pg_ha_slot_1"); err != nil {
		t.Errorf("a lost create race must be success (the slot exists), got %v", err)
	}
}

// It must NOT swallow an unrelated failure -- a connection error or a permission denial
// has to surface, or a primary that cannot create slots at all would look healthy.
func TestCreatePhysicalSlotPropagatesUnrelatedErrors(t *testing.T) {
	for _, out := range []string{
		"psql: error: connection to server failed: Connection refused",
		"ERROR:  permission denied for function pg_create_physical_replication_slot",
		"ERROR:  all replication slots are in use",
		"",
	} {
		p := &Prober{Exec: &slotExec{out: out, err: errors.New("exit status 1")}}
		if err := p.CreatePhysicalSlot(context.Background(), ConnInfo{}, "pg_ha_slot_1"); err == nil {
			t.Errorf("error %q was swallowed; only a duplicate-slot race may be treated as success", out)
		}
	}
}

func TestIsDuplicateSlotIsNarrow(t *testing.T) {
	if IsDuplicateSlot("anything", nil) {
		t.Error("a nil error is not a duplicate-slot outcome")
	}
	if !IsDuplicateSlot(`ERROR:  replication slot "x" already exists`, errors.New("exit 1")) {
		t.Error("the real PostgreSQL duplicate-slot message was not recognised")
	}
	// Both halves are required, so an unrelated "already exists" (a table, a role, a
	// database) cannot be mistaken for a slot race.
	for _, out := range []string{
		`ERROR:  relation "foo" already exists`,
		`ERROR:  role "repmgr" already exists`,
		`ERROR:  database "x" already exists`,
		"replication slot is active",
	} {
		if IsDuplicateSlot(out, errors.New("exit 1")) {
			t.Errorf("IsDuplicateSlot(%q) = true, want false", out)
		}
	}
}

// The production Exec must surface the command's stderr in its error, because psql puts
// every diagnostic there and exec.ExitError.Error() renders only "exit status N". Without
// this, IsDuplicateSlot had nothing to match on and a lost create/create race -- the
// documented, reproducible one -- surfaced as a per-tick warning instead of a no-op. Runs
// against the real OSExec, since a fake that returns the text as stdout is exactly what
// hid the gap.
func TestOSExecSurfacesStderrSoDiagnosticsAreInspectable(t *testing.T) {
	const msg = `ERROR:  replication slot "pg_ha_slot_1" already exists`
	out, err := OSExec{}.Run(context.Background(), nil, "sh", "-c", "echo '"+msg+"' >&2; exit 1")
	if err == nil {
		t.Fatal("want a non-nil error for a non-zero exit")
	}
	if out != "" {
		t.Errorf("stderr leaked into stdout, which callers parse as query output: %q", out)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error message drops the stderr diagnostic: %q", err.Error())
	}
	if !IsDuplicateSlot(out, err) {
		t.Errorf("IsDuplicateSlot cannot recognise a real duplicate-slot failure: out=%q err=%q", out, err.Error())
	}
}

// #288: the row shapes are the two verified against real PostgreSQL 18 with both kinds of
// standby streaming to one primary at once -- a native standby publishing its pod name, and a
// pre-#288 standby still dialling with libpq's default application_name whose identity has to
// come from the ordinal-named slot instead.
func TestReplicationTopologyParsesBothIdentitySources(t *testing.T) {
	ex := &slotExec{out: "pg-1|pg_ha_slot_1|streaming\nwalreceiver|pg_ha_slot_2|streaming"}
	rows, err := (&Prober{Exec: ex}).ReplicationTopology(context.Background(), ConnInfo{})
	if err != nil {
		t.Fatalf("ReplicationTopology: %v", err)
	}
	want := []ReplicaRow{
		{AppName: "pg-1", SlotName: "pg_ha_slot_1", State: "streaming"},
		{AppName: "walreceiver", SlotName: "pg_ha_slot_2", State: "streaming"},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d: got %+v, want %+v", i, rows[i], want[i])
		}
	}
	if !rows[0].Streaming() {
		t.Error("a streaming row does not report Streaming()")
	}
}

// A standby that exists but is still catching up must be distinguishable from one that is
// caught up: it has a row, so it is present, but it cannot serve or be safely promoted yet.
func TestReplicationTopologyDistinguishesCatchupFromStreaming(t *testing.T) {
	ex := &slotExec{out: "pg-1|pg_ha_slot_1|catchup"}
	rows, err := (&Prober{Exec: ex}).ReplicationTopology(context.Background(), ConnInfo{})
	if err != nil {
		t.Fatalf("ReplicationTopology: %v", err)
	}
	if len(rows) != 1 || rows[0].Streaming() {
		t.Errorf("a catchup replica reported as streaming: %+v", rows)
	}
}

// A slotless stream is still a real replica -- it just cannot be identified by slot, so the
// empty SlotName must survive rather than dropping the row.
func TestReplicationTopologyKeepsSlotlessRows(t *testing.T) {
	ex := &slotExec{out: "pg-1||streaming"}
	rows, err := (&Prober{Exec: ex}).ReplicationTopology(context.Background(), ConnInfo{})
	if err != nil {
		t.Fatalf("ReplicationTopology: %v", err)
	}
	if len(rows) != 1 || rows[0].SlotName != "" || rows[0].AppName != "pg-1" {
		t.Errorf("slotless row mishandled: %+v", rows)
	}
}

// No standbys is a legitimate answer (a lone primary), not an error -- unlike the repmgr.nodes
// reads this replaces, where an empty result meant "the table is missing" and had to be an
// error to keep the #297 gate from firing.
func TestReplicationTopologyEmptyIsNoReplicasNotAnError(t *testing.T) {
	rows, err := (&Prober{Exec: &slotExec{out: ""}}).ReplicationTopology(context.Background(), ConnInfo{})
	if err != nil || len(rows) != 0 {
		t.Errorf("empty output: got rows=%+v err=%v, want none/nil", rows, err)
	}
}

// A row that does not split into exactly three fields is an error, not a silently mis-split
// topology: application_name is operator-settable in principle, so a value containing the
// separator must be reported rather than guessed at.
func TestReplicationTopologyRejectsMalformedRows(t *testing.T) {
	if _, err := (&Prober{Exec: &slotExec{out: "pg-1|pg_ha_slot_1"}}).ReplicationTopology(context.Background(), ConnInfo{}); err == nil {
		t.Fatal("want an error for a 2-field row")
	}
}

// The query must read the primary's live connection list and recover identity from the slot,
// and must NOT fall back to repmgr.nodes -- retiring that table is the point of #288.
func TestReplicationTopologyQueryShape(t *testing.T) {
	ex := &slotExec{}
	if _, err := (&Prober{Exec: ex}).ReplicationTopology(context.Background(), ConnInfo{}); err != nil {
		t.Fatalf("ReplicationTopology: %v", err)
	}
	sql := ex.sqls[0]
	for _, needle := range []string{"pg_stat_replication", "pg_replication_slots", "active_pid", "application_name"} {
		if !strings.Contains(sql, needle) {
			t.Errorf("query is missing %q: %s", needle, sql)
		}
	}
	if strings.Contains(sql, "repmgr.nodes") {
		t.Errorf("topology still reads repmgr.nodes, which #288 exists to retire: %s", sql)
	}
}
