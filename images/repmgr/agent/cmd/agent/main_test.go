package main

import (
	"testing"
	"time"

	"github.com/cagriekin/pg-ha-agent/internal/pg"
	"github.com/cagriekin/pg-ha-agent/internal/reconcile"
)

func TestDesiredRoleLabels(t *testing.T) {
	peers := []reconcile.PeerState{
		{Name: "pg-1", Reachable: true, Role: pg.RoleStandby},  // -> standby (joins read-only Service)
		{Name: "pg-2", Reachable: true, Role: pg.RolePrimary},  // a second primary -> orphan (out of reads)
		{Name: "pg-3", Reachable: false, Role: pg.RoleUnknown}, // unreachable -> omitted (label untouched)
	}
	got := desiredRoleLabels("pg-0", peers)

	want := map[string]string{"pg-0": "primary", "pg-1": "standby", "pg-2": "orphan"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("label[%q] = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["pg-3"]; ok {
		t.Error("unreachable peer must be omitted so its label is left untouched (#140)")
	}
}

func TestSelfHealthTracker(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	h := &selfHealthTracker{grace: 15 * time.Second}

	// A slow startup (should serve, never been running) must NOT arm the timer,
	// even well past the grace -- otherwise crash-recovery WAL replay looks "stuck".
	if h.stuck(true, false, t0) {
		t.Fatal("startup (never running) must not be stuck")
	}
	if h.stuck(true, false, t0.Add(60*time.Second)) {
		t.Fatal("never-running data must never trip self-health")
	}

	// Once it comes up healthy the tracker is primed.
	if h.stuck(true, true, t0.Add(61*time.Second)) {
		t.Fatal("a running primary is not stuck")
	}

	// It then goes unreachable (frozen): not stuck until the grace elapses, then stuck.
	base := t0.Add(70 * time.Second)
	if h.stuck(true, false, base) {
		t.Fatal("just-unreachable primary should not be stuck before the grace")
	}
	if h.stuck(true, false, base.Add(14*time.Second)) {
		t.Fatal("within grace must not be stuck")
	}
	if !h.stuck(true, false, base.Add(15*time.Second)) {
		t.Fatal("past the grace the wedged primary must be stuck")
	}

	// Recovery (running again) clears the timer; a later blip re-arms from scratch.
	if h.stuck(true, true, base.Add(20*time.Second)) {
		t.Fatal("recovered primary is not stuck")
	}
	if h.stuck(true, false, base.Add(21*time.Second)) {
		t.Fatal("a fresh unreachable period must re-arm, not carry the old timer")
	}

	// Losing the holder role (or becoming a standby) resets everything.
	if h.stuck(false, false, base.Add(100*time.Second)) {
		t.Fatal("a non-serving node is never stuck")
	}
}

func TestShouldAdvanceMarker(t *testing.T) {
	tl := func(n uint32) pg.Timeline { return pg.Timeline(n) }
	cases := []struct {
		name string
		tl   pg.Timeline
		tlOK bool
		m    reconcile.MarkerState
		want bool
	}{
		{"unreadable local timeline never advances", tl(5), false, reconcile.MarkerState{}, false},
		{"no marker -> establish it", tl(5), true, reconcile.MarkerState{}, true},
		{"above highwater -> advance", tl(6), true, reconcile.MarkerState{Present: true, Timeline: tl(5)}, true},
		{"equal highwater -> no write", tl(5), true, reconcile.MarkerState{Present: true, Timeline: tl(5)}, false},
		{"below highwater -> never lower", tl(4), true, reconcile.MarkerState{Present: true, Timeline: tl(5)}, false},
		{"malformed marker -> re-establish", tl(5), true, reconcile.MarkerState{Present: true, Malformed: true, Timeline: tl(9)}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldAdvanceMarker(c.tl, c.tlOK, c.m); got != c.want {
				t.Errorf("shouldAdvanceMarker = %v, want %v", got, c.want)
			}
		})
	}
}

func TestNodeIDAndBaseName(t *testing.T) {
	if got := baseName("my-pg-0"); got != "my-pg" {
		t.Errorf("baseName = %q, want my-pg", got)
	}
	if got := nodeID("my-pg-3"); got != 1003 {
		t.Errorf("nodeID = %d, want 1003", got)
	}
}
