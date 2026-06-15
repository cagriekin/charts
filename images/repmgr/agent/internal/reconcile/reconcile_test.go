package reconcile

import (
	"testing"

	"github.com/cagriekin/pg-ha-agent/internal/pg"
)

func tl(n uint32) pg.Timeline { return pg.Timeline(n) }
func ls(hi, lo uint64) pg.LSN  { return pg.LSN{Hi: hi, Lo: lo} }

func primary(name string, t uint32, hi, lo uint64) PeerState {
	return PeerState{Name: name, Reachable: true, Role: pg.RolePrimary, Timeline: tl(t), TimelineOK: true, LSN: ls(hi, lo), LSNOK: true}
}
func standby(name string, t uint32, hi, lo uint64) PeerState {
	return PeerState{Name: name, Reachable: true, Role: pg.RoleStandby, Timeline: tl(t), TimelineOK: true, LSN: ls(hi, lo), LSNOK: true}
}

// gossipPeer is an unreachable peer whose position is known only from gossip (its
// role is unknown -- gossip carries position, not a trusted role).
func gossipPeer(name string, t uint32, hi, lo uint64) PeerState {
	return PeerState{Name: name, Reachable: false, Gossip: true, Role: pg.RoleUnknown, Timeline: tl(t), TimelineOK: true, LSN: ls(hi, lo), LSNOK: true}
}

func TestDecide(t *testing.T) {
	localStandby := LocalState{HasData: true, Running: true, InRecovery: true, Timeline: tl(5), TimelineOK: true, LSN: ls(5, 0x100), LSNOK: true}
	localPrimary := LocalState{HasData: true, Running: true, InRecovery: false, Timeline: tl(5), TimelineOK: true, LSN: ls(5, 0x100), LSNOK: true}
	emptyData := LocalState{HasData: false, Running: false}
	// stopped nodes carry their timeline/role from pg_controldata (see observe());
	// dataStopped is on-disk primary state, dataStoppedStandby is on-disk standby state.
	dataStopped := LocalState{HasData: true, Running: false, Timeline: tl(5), TimelineOK: true}
	dataStoppedStandby := LocalState{HasData: true, Running: false, InRecovery: true, Timeline: tl(5), TimelineOK: true}
	// a stopped primary whose controldata checkpoint LSN was read (LSNOK), so it can
	// be compared against a gossiped peer position.
	dataStoppedLSN := LocalState{HasData: true, Running: false, Timeline: tl(5), TimelineOK: true, LSN: ls(5, 0x100), LSNOK: true}

	cases := []struct {
		name       string
		obs        Observation
		wantAction Action
		wantTarget string
	}{
		{"paused suspends everything", Observation{Paused: true, HoldLease: true, Local: localPrimary}, NoOp, ""},

		{"holder + empty + no peers -> initdb", Observation{HoldLease: true, Local: emptyData}, BootstrapInitdb, ""},
		{"holder + empty + marker present -> wait/settle", Observation{HoldLease: true, Local: emptyData, Marker: MarkerState{Present: true, Timeline: tl(5)}}, Wait, ""},
		{"holder + empty + a live primary -> clone not initdb", Observation{HoldLease: true, Local: emptyData, Peers: []PeerState{primary("pg-1", 5, 5, 0x100)}}, BootstrapClone, "pg-1"},

		{"holder standby caught up most-advanced -> promote", Observation{HoldLease: true, Local: localStandby, Peers: []PeerState{standby("pg-1", 5, 5, 0x80)}}, Promote, ""},
		{"holder standby + newer primary peer -> rejoin forward", Observation{HoldLease: true, Local: localStandby, Peers: []PeerState{primary("pg-1", 6, 6, 0x10)}}, RejoinForward, "pg-1"},
		{"holder standby below highwater -> release", Observation{HoldLease: true, Local: localStandby, Marker: MarkerState{Present: true, Timeline: tl(6)}}, ReleaseLease, ""},
		{"holder standby but a peer has more WAL -> release/handoff", Observation{HoldLease: true, Local: localStandby, Peers: []PeerState{standby("pg-2", 5, 5, 0x200)}}, ReleaseLease, "pg-2"},

		{"holder primary current -> stay", Observation{HoldLease: true, Local: localPrimary, Peers: []PeerState{standby("pg-1", 5, 5, 0x80)}}, StayPrimary, ""},
		{"holder primary + malformed marker -> release", Observation{HoldLease: true, Local: localPrimary, Marker: MarkerState{Malformed: true}}, ReleaseLease, ""},

		{"not holder + read-write -> soft fence", Observation{HoldLease: false, Local: localPrimary}, DemoteFence, ""},
		{"not holder + standby + known leader -> follow", Observation{HoldLease: false, Local: localStandby, LeaderIdentity: "pg-0"}, Follow, "pg-0"},
		{"not holder + empty + leader primary -> clone from leader", Observation{HoldLease: false, Local: emptyData, LeaderIdentity: "pg-0", Peers: []PeerState{primary("pg-0", 5, 5, 0x100)}}, BootstrapClone, "pg-0"},
		{"not holder + empty + no primary -> wait", Observation{HoldLease: false, Local: emptyData}, Wait, ""},
		{"not holder + data stopped + newer primary -> rejoin forward", Observation{HoldLease: false, Local: dataStopped, Peers: []PeerState{primary("pg-1", 6, 6, 0x10)}}, RejoinForward, "pg-1"},

		// --- stopped-node start gating (controldata-driven; the flap fix) ---
		// Holder, primary-state data stopped, nothing newer, at/above highwater: safe to start.
		{"holder + primary-state stopped + safe -> start", Observation{HoldLease: true, Local: dataStopped}, StartLocal, ""},
		// Holder, primary-state data stopped, below the recorded highwater: must not start RW.
		{"holder + primary-state stopped + below highwater -> release", Observation{HoldLease: true, Local: dataStopped, Marker: MarkerState{Present: true, Timeline: tl(6)}}, ReleaseLease, ""},
		// Cold-boot winner AT the highwater (its own timeline == marker): safe to start as primary.
		{"holder + primary-state stopped + at highwater -> start", Observation{HoldLease: true, Local: dataStopped, Marker: MarkerState{Present: true, Timeline: tl(5)}}, StartLocal, ""},
		// Holder, standby-state data stopped: start as a standby (promotes a later tick).
		{"holder + standby-state stopped -> start", Observation{HoldLease: true, Local: dataStoppedStandby}, StartLocal, ""},
		// Non-holder, primary-state data stopped, no rejoin target: HOLD (never start RW -> the flap).
		{"not holder + primary-state stopped + no primary -> hold", Observation{HoldLease: false, Local: dataStopped}, Wait, ""},
		// Non-holder, primary-state data stopped, a same-timeline primary exists: rejoin it as a standby.
		{"not holder + primary-state stopped + same-tl primary -> rejoin", Observation{HoldLease: false, Local: dataStopped, Peers: []PeerState{primary("pg-1", 5, 5, 0x200)}}, RejoinForward, "pg-1"},
		// Non-holder, standby-state data stopped: start as a standby.
		{"not holder + standby-state stopped -> start", Observation{HoldLease: false, Local: dataStoppedStandby}, StartLocal, ""},
		// A stale primary on a LOWER timeline is never a rejoin source (forward-only, invariant 5): hold.
		{"not holder + primary-state stopped + lower-tl primary -> hold", Observation{HoldLease: false, Local: dataStopped, Peers: []PeerState{primary("pg-1", 4, 4, 0x10)}}, Wait, ""},

		// --- self-health (LocalStuck): a wedged primary fails over / restarts ---
		// Holder, primary stuck unhealthy, a standby exists: release the lease for failover.
		{"holder + stuck primary + standby -> release for failover", Observation{HoldLease: true, Local: dataStopped, LocalStuck: true, Peers: []PeerState{standby("pg-1", 5, 5, 0x80)}}, ReleaseLease, ""},
		// Holder, primary stuck unhealthy, single node: force-restart in place (no peer to take over).
		{"holder + stuck primary + single node -> restart in place", Observation{HoldLease: true, Local: dataStopped, LocalStuck: true}, RestartLocal, ""},
		// Stuck but below highwater still fails closed (release) before self-health restart is considered.
		{"holder + stuck primary + below highwater -> release (highwater first)", Observation{HoldLease: true, Local: dataStopped, LocalStuck: true, Marker: MarkerState{Present: true, Timeline: tl(6)}}, ReleaseLease, ""},

		// --- LSN gossip: rank an unreachable peer's gossiped position at cold boot ---
		// A running standby holder sees an unreachable peer that gossips a higher same-timeline LSN: release.
		{"holder standby + more-advanced gossip peer -> release", Observation{HoldLease: true, Local: localStandby, Peers: []PeerState{gossipPeer("pg-2", 5, 5, 0x200)}}, ReleaseLease, "pg-2"},
		// A stopped primary-state holder likewise steps aside for a more-advanced gossip peer.
		{"holder primary-state stopped + more-advanced gossip peer -> release", Observation{HoldLease: true, Local: dataStoppedLSN, Peers: []PeerState{gossipPeer("pg-2", 5, 5, 0x200)}}, ReleaseLease, "pg-2"},
		// A gossip peer that is BEHIND must not cause a spurious release: the holder starts.
		{"holder primary-state stopped + behind gossip peer -> start", Observation{HoldLease: true, Local: dataStoppedLSN, Peers: []PeerState{gossipPeer("pg-2", 5, 5, 0x10)}}, StartLocal, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Decide(c.obs)
			if got.Action != c.wantAction {
				t.Fatalf("action = %v (%s), want %v", got.Action, got.Reason, c.wantAction)
			}
			if c.wantTarget != "" && got.Target != c.wantTarget {
				t.Errorf("target = %q, want %q (reason: %s)", got.Target, c.wantTarget, got.Reason)
			}
		})
	}
}

// A frozen primary (local read-write) that has lost the lease must always fence,
// regardless of peers — the core two-writer-prevention guarantee.
func TestDecideFenceIsUnconditional(t *testing.T) {
	o := Observation{
		HoldLease: false,
		Local:     LocalState{HasData: true, Running: true, InRecovery: false, Timeline: tl(9), TimelineOK: true, LSN: ls(9, 9), LSNOK: true},
		Peers:     []PeerState{primary("pg-1", 10, 10, 1)}, // a newer primary already exists
	}
	if got := Decide(o); got.Action != DemoteFence {
		t.Errorf("lost-lease read-write node must fence, got %v (%s)", got.Action, got.Reason)
	}
}
