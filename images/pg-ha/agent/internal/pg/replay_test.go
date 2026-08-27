package pg

import (
	"context"
	"errors"
	"testing"
)

// fakeReplayExec returns a canned psql result (or error) for the single query
// ReplayProgress issues.
type fakeReplayExec struct {
	out string
	err error
}

func (f fakeReplayExec) Run(context.Context, []string, string, ...string) (string, error) {
	return f.out, f.err
}

func replayProber(out string, err error) *Prober {
	return &Prober{Exec: fakeReplayExec{out: out, err: err}}
}

func TestReplayProgressRecovering(t *testing.T) {
	p := replayProber("t|16/B3000000|16/B2000000|2026-08-01 11:58:03.123456+00", nil)
	r, err := p.ReplayProgress(context.Background(), ConnInfo{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.Reachable || !r.InRecovery {
		t.Fatalf("want reachable node in recovery, got %+v", r)
	}
	if !r.ReceiveOK || !r.ReplayOK {
		t.Fatalf("both positions should parse: %+v", r)
	}
	lag, ok := r.ReplayLagBytes()
	if !ok {
		t.Fatal("lag should be known when both positions parse")
	}
	if want := uint64(0x1000000); lag != want {
		t.Errorf("lag = %d, want %d", lag, want)
	}
	if r.LastReplayTime != "2026-08-01 11:58:03.123456+00" {
		t.Errorf("LastReplayTime = %q", r.LastReplayTime)
	}
}

// A promoted/normal primary is a valid answer, not an error: no recovery, and the
// replay positions are NULL (empty), so the lag must read as UNKNOWN rather than 0.
func TestReplayProgressNotInRecovery(t *testing.T) {
	p := replayProber("f|||", nil)
	r, err := p.ReplayProgress(context.Background(), ConnInfo{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.Reachable || r.InRecovery {
		t.Fatalf("want reachable, not in recovery: %+v", r)
	}
	if _, ok := r.ReplayLagBytes(); ok {
		t.Error("lag must be unknown when the positions are NULL, never reported as caught up")
	}
}

// Replay can momentarily read ahead of receive on a node replaying its own WAL from
// the archive; clamp to zero rather than underflowing a uint64 into a huge lag.
func TestReplayProgressClampsNegativeLag(t *testing.T) {
	p := replayProber("t|16/B2000000|16/B3000000|", nil)
	r, _ := p.ReplayProgress(context.Background(), ConnInfo{})
	lag, ok := r.ReplayLagBytes()
	if !ok || lag != 0 {
		t.Errorf("lag = (%d, %v), want (0, true)", lag, ok)
	}
}

func TestReplayProgressUnreachable(t *testing.T) {
	p := replayProber("", errors.New("connection refused"))
	r, err := p.ReplayProgress(context.Background(), ConnInfo{})
	if err == nil {
		t.Fatal("a failed probe must surface the error")
	}
	if r.Reachable {
		t.Error("an unreachable node must not report Reachable")
	}
}

// A malformed result must report unknown, never a partially-parsed position that a
// caller could read as progress.
func TestReplayProgressMalformed(t *testing.T) {
	p := replayProber("t|16/B3000000", nil)
	r, err := p.ReplayProgress(context.Background(), ConnInfo{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Reachable || r.ReceiveOK || r.ReplayOK {
		t.Errorf("malformed output must yield an all-unknown result: %+v", r)
	}
}

func TestLSNUint64(t *testing.T) {
	l, ok := ParseLSN("16/B374D848")
	if !ok {
		t.Fatal("parse failed")
	}
	if want := uint64(0x16)<<32 | 0xB374D848; l.Uint64() != want {
		t.Errorf("Uint64() = %x, want %x", l.Uint64(), want)
	}
}
