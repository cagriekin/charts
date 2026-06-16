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
	standbyTL string // pg_control_checkpoint timeline_id output (decimal)
	sysID     string // pg_control_system() system_identifier output (decimal)
	err       error  // if set, every call fails (node unreachable)
}

func (f fakeExec) Run(_ context.Context, _ []string, _ string, args ...string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	sql := args[len(args)-1]
	switch {
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
