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
	slots     string // PhysicalSlots slot_name output (newline-separated)
	err       error  // if set, every call fails (node unreachable)
}

func (f fakeExec) Run(_ context.Context, _ []string, _ string, args ...string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	sql := args[len(args)-1]
	switch {
	case strings.Contains(sql, "pg_replication_slots"):
		return f.slots, nil
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

// #308: synchronized_standby_slots reconciliation reads this.
// Pins the specific regression an earlier revision had: filtering on `active` meant a
// standby restart, a rolling upgrade, or a brief network blip (walsender momentarily
// detached) emptied synchronized_standby_slots. This must query existence only.
func TestPhysicalSlotsQueryHasNoActiveFilter(t *testing.T) {
	ex := &spySlotExec{}
	p := &Prober{Exec: ex}
	if _, err := p.PhysicalSlots(context.Background(), ConnInfo{Host: "x"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(ex.calls) != 1 {
		t.Fatalf("calls = %v, want exactly 1", ex.calls)
	}
	if strings.Contains(ex.calls[0], "active") {
		t.Errorf("query must not filter on active (that reintroduces the blip regression): %q", ex.calls[0])
	}
	if !strings.Contains(ex.calls[0], "slot_type = 'physical'") {
		t.Errorf("query must still scope to physical slots: %q", ex.calls[0])
	}
}

func TestPhysicalSlots(t *testing.T) {
	p := proberWith(fakeExec{slots: "repmgr_slot_1001\nrepmgr_slot_1002\n"})
	names, err := p.PhysicalSlots(context.Background(), ConnInfo{Host: "x"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := []string{"repmgr_slot_1001", "repmgr_slot_1002"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %v, want %v", names, want)
		}
	}
}

func TestPhysicalSlotsEmpty(t *testing.T) {
	p := proberWith(fakeExec{slots: ""})
	if names, err := p.PhysicalSlots(context.Background(), ConnInfo{Host: "x"}); err != nil || names != nil {
		t.Fatalf("empty result must return (nil, nil), got (%v, %v)", names, err)
	}
}

func TestPhysicalSlotsUnreachable(t *testing.T) {
	p := proberWith(fakeExec{err: errors.New("connection refused")})
	if _, err := p.PhysicalSlots(context.Background(), ConnInfo{Host: "x"}); err == nil {
		t.Fatal("an unreachable node must return an error")
	}
}

// spySlotExec records every SQL statement it is asked to run, and can be told to fail
// on ALTER SYSTEM specifically -- SetSynchronizedStandbySlots's tests need to assert on
// call COUNT and ORDER, which fakeExec (shared, value-typed, output-only) does not track.
type spySlotExec struct {
	calls    []string
	alterErr error
}

func (s *spySlotExec) Run(_ context.Context, _ []string, _ string, args ...string) (string, error) {
	sql := args[len(args)-1]
	s.calls = append(s.calls, sql)
	if s.alterErr != nil && strings.Contains(sql, "ALTER SYSTEM") {
		return "", s.alterErr
	}
	return "", nil
}

// #308: confirmed live that PostgreSQL treats multiple semicolon-separated statements
// in one simple-query message as an implicit transaction block, and ALTER SYSTEM
// refuses to run inside one -- so this must be two separate psql invocations, not one
// combined "ALTER SYSTEM ...; SELECT pg_reload_conf();" string.
func TestSetSynchronizedStandbySlotsIssuesTwoSeparateStatements(t *testing.T) {
	ex := &spySlotExec{}
	p := &Prober{Exec: ex}
	if err := p.SetSynchronizedStandbySlots(context.Background(), ConnInfo{Host: "x"}, []string{"repmgr_slot_1001", "repmgr_slot_1002"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(ex.calls) != 2 {
		t.Fatalf("calls = %v, want exactly 2 separate statements", ex.calls)
	}
	if !strings.Contains(ex.calls[0], "ALTER SYSTEM SET synchronized_standby_slots = 'repmgr_slot_1001,repmgr_slot_1002'") {
		t.Errorf("first call = %q", ex.calls[0])
	}
	if !strings.Contains(ex.calls[1], "pg_reload_conf") {
		t.Errorf("second call = %q, want the reload", ex.calls[1])
	}
}

func TestSetSynchronizedStandbySlotsEmptyListIsAValidValue(t *testing.T) {
	// No active standbys is a legitimate steady state (e.g. a lone primary), not an
	// error -- synchronized_standby_slots = '' means "no slots required".
	ex := &spySlotExec{}
	p := &Prober{Exec: ex}
	if err := p.SetSynchronizedStandbySlots(context.Background(), ConnInfo{Host: "x"}, nil); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(ex.calls[0], "synchronized_standby_slots = ''") {
		t.Errorf("first call = %q, want an empty value, not omitted or erroring", ex.calls[0])
	}
}

func TestSetSynchronizedStandbySlotsSkipsReloadOnAlterFailure(t *testing.T) {
	ex := &spySlotExec{alterErr: errors.New("permission denied")}
	p := &Prober{Exec: ex}
	err := p.SetSynchronizedStandbySlots(context.Background(), ConnInfo{Host: "x"}, []string{"repmgr_slot_1001"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(ex.calls) != 1 {
		t.Fatalf("calls = %v, want the reload skipped after ALTER SYSTEM fails", ex.calls)
	}
}

// #308: validation is enforced HERE, not left to the caller -- slots is interpolated
// directly into the ALTER SYSTEM statement text, so a future second call site cannot
// bypass the check by omission.
func TestSetSynchronizedStandbySlotsRefusesUnexpectedSlotName(t *testing.T) {
	ex := &spySlotExec{}
	p := &Prober{Exec: ex}
	err := p.SetSynchronizedStandbySlots(context.Background(), ConnInfo{Host: "x"}, []string{"repmgr_slot_1001'; DROP TABLE x; --"})
	if err == nil {
		t.Fatal("expected an error for an unexpected slot name")
	}
	if len(ex.calls) != 0 {
		t.Fatalf("expected no SQL at all, got %v", ex.calls)
	}
}
