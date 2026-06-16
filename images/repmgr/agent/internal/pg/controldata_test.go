package pg

import (
	"context"
	"errors"
	"testing"
)

// cdExec returns canned pg_controldata output (or an error) regardless of args.
type cdExec struct {
	out string
	err error
}

func (e cdExec) Run(_ context.Context, _ []string, _ string, _ ...string) (string, error) {
	return e.out, e.err
}

func sampleControlData(state, timeline string) string {
	return "pg_control version number:            1700\n" +
		"Catalog version number:               202406281\n" +
		"Database system identifier:           7395000000000000001\n" +
		"Database cluster state:               " + state + "\n" +
		"pg_control last modified:             Mon 01 Jan 2026 00:00:00 UTC\n" +
		"Latest checkpoint location:           0/14D8C18\n" +
		"Latest checkpoint's REDO location:    0/14D8BE0\n" +
		"Latest checkpoint's TimeLineID:       " + timeline + "\n" +
		"Latest checkpoint's full_page_writes: on\n"
}

func TestParseControlDataStates(t *testing.T) {
	cases := []struct {
		state          string
		wantInRecovery bool
	}{
		{"in production", false},
		{"shut down", false},
		{"in crash recovery", false}, // a primary replaying its own WAL, not a standby
		{"in archive recovery", true},
		{"shut down in recovery", true},
		{"starting up", false},
	}
	for _, c := range cases {
		t.Run(c.state, func(t *testing.T) {
			ci := parseControlData(sampleControlData(c.state, "1"))
			if ci.State != c.state {
				t.Errorf("State = %q, want %q", ci.State, c.state)
			}
			if ci.InRecovery != c.wantInRecovery {
				t.Errorf("InRecovery = %v, want %v for state %q", ci.InRecovery, c.wantInRecovery, c.state)
			}
		})
	}
}

func TestParseControlDataTimelineIsDecimal(t *testing.T) {
	// pg_controldata prints the TimeLineID in DECIMAL (unlike the hex timeline in a
	// WAL file name). 13 must parse to Timeline(13), not 0x13.
	ci := parseControlData(sampleControlData("in production", "13"))
	if !ci.TimelineOK || ci.Timeline != 13 {
		t.Fatalf("Timeline = (%d, ok=%v), want 13", ci.Timeline, ci.TimelineOK)
	}
}

func TestParseControlDataUnreadableTimeline(t *testing.T) {
	ci := parseControlData("Database cluster state:               in production\n")
	if ci.TimelineOK {
		t.Errorf("TimelineOK = true with no TimeLineID line; want false")
	}
	if ci.InRecovery {
		t.Errorf("InRecovery = true for 'in production'")
	}
}

func TestReadControlDataError(t *testing.T) {
	_, err := ReadControlData(context.Background(), cdExec{err: errors.New("no data dir")}, "pg_controldata", "/nope")
	if err == nil {
		t.Fatal("want error when pg_controldata fails")
	}
}

func TestReadControlDataOK(t *testing.T) {
	ci, err := ReadControlData(context.Background(), cdExec{out: sampleControlData("shut down in recovery", "7")}, "pg_controldata", "/data")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ci.InRecovery || !ci.TimelineOK || ci.Timeline != 7 {
		t.Errorf("got %+v, want InRecovery=true Timeline=7", ci)
	}
	// "Latest checkpoint location: 0/14D8C18" -> the gossip-ranking LSN.
	if !ci.LSNOK || ci.LSN.Hi != 0 || ci.LSN.Lo != 0x14D8C18 {
		t.Errorf("checkpoint LSN = (%+v, ok=%v), want 0/14D8C18", ci.LSN, ci.LSNOK)
	}
	// "Database system identifier: 7395000000000000001" -> the cluster identity
	// used to refuse a clone/follow/rewind from a foreign cluster (invariant 9).
	if ci.SystemID != 7395000000000000001 {
		t.Errorf("SystemID = %d, want 7395000000000000001", ci.SystemID)
	}
}

func TestParseControlDataUnreadableSystemID(t *testing.T) {
	ci := parseControlData("Database cluster state:               in production\n")
	if ci.SystemID != 0 {
		t.Errorf("SystemID = %d with no identifier line; want 0 (unreadable)", ci.SystemID)
	}
}
