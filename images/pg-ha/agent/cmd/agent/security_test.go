package main

import (
	"testing"
	"time"

	"github.com/cagriekin/pg-ha-agent/internal/pg"
	"github.com/cagriekin/pg-ha-agent/internal/reconcile"
)

func TestValidRestoreProvenance(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name, in, want string
	}{
		{"empty stays empty", "", ""},
		{"valid past kept", "2026-07-30T09:05:00Z", "2026-07-30T09:05:00Z"},
		{"near-now within skew kept", "2026-08-29T12:30:00Z", "2026-08-29T12:30:00Z"},
		{"far future rejected", "9999-01-01T00:00:00Z", ""},
		{"just past skew rejected", "2026-08-29T13:30:00Z", ""},
		{"unparseable rejected", "not-a-timestamp", ""},
		{"lexically-high garbage rejected", "zzzz", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := validRestoreProvenance(c.in, now); got != c.want {
				t.Errorf("validRestoreProvenance(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestValidPeerName(t *testing.T) {
	a := &agent{base: "pg"}
	valid := []string{"pg-0", "pg-1", "pg-12"}
	for _, n := range valid {
		if !a.validPeerName(n) {
			t.Errorf("validPeerName(%q) = false, want true", n)
		}
	}
	invalid := []string{
		"",
		"pg",                                   // no ordinal
		"other-0",                              // wrong base
		"pg-0 port=5432",                       // injected conninfo keyword
		"evil.svc port=5432 sslmode=disable x", // the attack string
		"pg--1",                                // not <base>-<ordinal>
		"pg-x",                                 // non-numeric ordinal
	}
	for _, n := range invalid {
		if a.validPeerName(n) {
			t.Errorf("validPeerName(%q) = true, want false", n)
		}
	}
}

func TestMarkerTamperSuspected(t *testing.T) {
	local := func(tl uint32, ok bool) reconcile.LocalState {
		return reconcile.LocalState{Timeline: pg.Timeline(tl), TimelineOK: ok}
	}
	cases := []struct {
		name    string
		o       reconcile.Observation
		suspect bool
	}{
		{"absent marker is fine", reconcile.Observation{Marker: reconcile.MarkerState{Present: false}}, false},
		{"malformed is suspect", reconcile.Observation{Marker: reconcile.MarkerState{Present: true, Malformed: true}}, true},
		{"plausible marker is fine", reconcile.Observation{
			Local:  local(5, true),
			Marker: reconcile.MarkerState{Present: true, Timeline: pg.Timeline(6)},
		}, false},
		{"implausibly high is suspect", reconcile.Observation{
			Local:  local(5, true),
			Marker: reconcile.MarkerState{Present: true, Timeline: pg.Timeline(9999)},
		}, true},
		{"high marker judged against a reachable peer is fine", reconcile.Observation{
			Local:  local(5, true),
			Peers:  []reconcile.PeerState{{Timeline: pg.Timeline(2000), TimelineOK: true}},
			Marker: reconcile.MarkerState{Present: true, Timeline: pg.Timeline(2001)},
		}, false},
		{"no observable timeline: cannot judge, not suspect", reconcile.Observation{
			Local:  local(0, false),
			Marker: reconcile.MarkerState{Present: true, Timeline: pg.Timeline(9999)},
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := markerTamperSuspected(c.o)
			if got != c.suspect {
				t.Errorf("markerTamperSuspected = %v, want %v", got, c.suspect)
			}
		})
	}
}
