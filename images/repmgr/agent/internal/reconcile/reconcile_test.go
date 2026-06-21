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
		{"holder + empty + marker present (no primary named) -> wait/settle", Observation{HoldLease: true, Local: emptyData, Marker: MarkerState{Present: true, Timeline: tl(5)}}, Wait, ""},
		// #186: empty data holding the lease while the marker names a DIFFERENT primary
		// -> release so the data-bearing primary can acquire (the rolling-restart deadlock).
		{"holder + empty + marker names a different primary -> release (#186)", Observation{HoldLease: true, Local: emptyData, LocalNode: "pg-1", Marker: MarkerState{Present: true, Timeline: tl(5), Primary: "pg-0"}}, ReleaseLease, "pg-0"},
		// ...but if the marker names THIS node (its own PVC was lost), settle -- do not
		// hand off to a node that may be less advanced (#170 self-PVC-loss).
		{"holder + empty + marker names this node -> wait/settle (#170)", Observation{HoldLease: true, Local: emptyData, LocalNode: "pg-0", Marker: MarkerState{Present: true, Timeline: tl(5), Primary: "pg-0"}}, Wait, ""},
		// ...and a MALFORMED marker is never trusted to hand off (#174 fail-closed): settle,
		// even though it names a different primary -- do not release on a corrupt marker.
		{"holder + empty + malformed marker names a different primary -> wait/settle (#174)", Observation{HoldLease: true, Local: emptyData, LocalNode: "pg-1", Marker: MarkerState{Present: true, Malformed: true, Primary: "pg-0"}}, Wait, ""},
		{"holder + empty + a live primary -> clone not initdb", Observation{HoldLease: true, Local: emptyData, Peers: []PeerState{primary("pg-1", 5, 5, 0x100)}}, BootstrapClone, "pg-1"},

		{"holder standby caught up most-advanced -> promote", Observation{HoldLease: true, Local: localStandby, Peers: []PeerState{standby("pg-1", 5, 5, 0x80)}}, Promote, ""},
		{"holder standby + newer primary peer -> rejoin forward", Observation{HoldLease: true, Local: localStandby, Peers: []PeerState{primary("pg-1", 6, 6, 0x10)}}, RejoinForward, "pg-1"},
		{"holder standby below highwater -> release", Observation{HoldLease: true, Local: localStandby, Marker: MarkerState{Present: true, Timeline: tl(6)}}, ReleaseLease, ""},
		{"holder standby but a peer has more WAL -> release/handoff", Observation{HoldLease: true, Local: localStandby, Peers: []PeerState{standby("pg-2", 5, 5, 0x200)}}, ReleaseLease, "pg-2"},

		{"holder primary current -> stay", Observation{HoldLease: true, Local: localPrimary, Peers: []PeerState{standby("pg-1", 5, 5, 0x80)}}, StayPrimary, ""},
		{"holder primary + malformed marker -> release", Observation{HoldLease: true, Local: localPrimary, Marker: MarkerState{Malformed: true}}, ReleaseLease, ""},

		// --- controlled switchover (Part H2): step down only for a caught-up target ---
		// target caught up (same tl, LSN >= local) -> hand off
		{"switchover: caught-up target -> switchover", Observation{HoldLease: true, Local: localPrimary, SwitchoverTarget: "pg-1", Peers: []PeerState{standby("pg-1", 5, 5, 0x100)}}, Switchover, "pg-1"},
		// target lagging (LSN < local) -> keep serving until it catches up (no data loss)
		{"switchover: lagging target -> stay", Observation{HoldLease: true, Local: localPrimary, SwitchoverTarget: "pg-1", Peers: []PeerState{standby("pg-1", 5, 5, 0x80)}}, StayPrimary, ""},
		// target on a different timeline -> not a clean handoff
		{"switchover: divergent-timeline target -> stay", Observation{HoldLease: true, Local: localPrimary, SwitchoverTarget: "pg-1", Peers: []PeerState{standby("pg-1", 4, 5, 0x200)}}, StayPrimary, ""},
		// target not a standby (a reachable non-recovery node) -> refuse
		{"switchover: non-standby target -> stay", Observation{HoldLease: true, Local: localPrimary, SwitchoverTarget: "pg-1", Peers: []PeerState{primary("pg-1", 5, 5, 0x200)}}, StayPrimary, ""},
		// target only known via gossip (unreachable) -> no confirmed position, refuse
		{"switchover: gossip-only target -> stay", Observation{HoldLease: true, Local: localPrimary, SwitchoverTarget: "pg-2", Peers: []PeerState{gossipPeer("pg-2", 5, 5, 0x200)}}, StayPrimary, ""},
		// target not among peers (e.g. self or a typo) -> refuse
		{"switchover: unknown target -> stay", Observation{HoldLease: true, Local: localPrimary, SwitchoverTarget: "pg-9", Peers: []PeerState{standby("pg-1", 5, 5, 0x200)}}, StayPrimary, ""},
		// paused overrides a pending switchover (maintenance owns the cluster)
		{"switchover: paused overrides", Observation{Paused: true, HoldLease: true, Local: localPrimary, SwitchoverTarget: "pg-1", Peers: []PeerState{standby("pg-1", 5, 5, 0x200)}}, NoOp, ""},

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
		// Non-holder, primary-state stopped, ANOTHER node holds the lease (cold-boot
		// election): come up READ-ONLY in recovery mode so its true position is observable.
		{"not holder + primary-state stopped + a leader exists -> recovery", Observation{HoldLease: false, Local: dataStopped, LeaderIdentity: "pg-2"}, StartRecovery, ""},
		// Non-holder, primary-state stopped, NO leader yet (lease settling): wait to acquire,
		// never recovery mode -- a fresh/sole master has no repmgr record to promote back out.
		{"not holder + primary-state stopped + no leader -> wait", Observation{HoldLease: false, Local: dataStopped}, Wait, ""},
		// Non-holder, primary-state data stopped, a same-timeline primary exists: rejoin it as a standby.
		{"not holder + primary-state stopped + same-tl primary -> rejoin", Observation{HoldLease: false, Local: dataStopped, Peers: []PeerState{primary("pg-1", 5, 5, 0x200)}}, RejoinForward, "pg-1"},
		// Non-holder, standby-state data stopped: start as a standby.
		{"not holder + standby-state stopped -> start", Observation{HoldLease: false, Local: dataStoppedStandby}, StartLocal, ""},
		// A stale primary on a LOWER timeline is never a rejoin source (forward-only, invariant 5);
		// with a leader present it comes up read-only in recovery mode for the election.
		{"not holder + primary-state stopped + lower-tl primary + leader -> recovery", Observation{HoldLease: false, Local: dataStopped, LeaderIdentity: "pg-2", Peers: []PeerState{primary("pg-1", 4, 4, 0x10)}}, StartRecovery, ""},

		// --- self-health (LocalStuck): a wedged primary fails over / restarts ---
		// Holder, primary stuck unhealthy, a standby exists: release the lease for failover.
		{"holder + stuck primary + standby -> release for failover", Observation{HoldLease: true, Local: dataStopped, LocalStuck: true, Peers: []PeerState{standby("pg-1", 5, 5, 0x80)}}, ReleaseLease, ""},
		// Holder, primary stuck unhealthy, single node: force-restart in place (no peer to take over).
		{"holder + stuck primary + single node -> restart in place", Observation{HoldLease: true, Local: dataStopped, LocalStuck: true}, RestartLocal, ""},
		// Stuck but below highwater still fails closed (release) before self-health restart is considered.
		{"holder + stuck primary + below highwater -> release (highwater first)", Observation{HoldLease: true, Local: dataStopped, LocalStuck: true, Marker: MarkerState{Present: true, Timeline: tl(6)}}, ReleaseLease, ""},

		// --- LSN gossip: rank an unreachable peer's gossiped position at cold boot ---
		// A gossip-only (unreachable) peer that appears more advanced is almost always
		// the just-dead primary whose annotation is still fresh. In STEADY state
		// (PeersPending false) it can never reacquire, so do NOT hand off (that only
		// flaps the lease + delays failover) -- promote.
		{"holder standby + more-advanced gossip-only peer (steady) -> promote", Observation{HoldLease: true, Local: localStandby, Peers: []PeerState{gossipPeer("pg-2", 5, 5, 0x200)}}, Promote, ""},
		// At COLD BOOT (PeersPending) the same gossip-only peer may be a stopped
		// primary-state node coming up via recovery-mode: wait for it to be observable.
		{"holder standby + more-advanced gossip-only peer (cold boot) -> wait", Observation{HoldLease: true, Local: localStandby, PeersPending: true, Peers: []PeerState{gossipPeer("pg-2", 5, 5, 0x200)}}, Wait, ""},
		// A REACHABLE more-advanced peer can actually take over -> hand off.
		{"holder standby + reachable more-advanced peer -> release/handoff", Observation{HoldLease: true, Local: localStandby, Peers: []PeerState{standby("pg-2", 5, 5, 0x200)}}, ReleaseLease, "pg-2"},
		// A PRIMARY-state holder is a source on its timeline -- a same-timeline peer
		// (standby or gossip estimate) can never be ahead of it, so it resumes directly
		// (no LSN compare here). Only a NEWER-timeline peer (newer) or the highwater
		// guard can stop it; the standby-holder branch handles a less-advanced winner.
		{"holder primary-state stopped + same-tl gossip peer -> start (source not behind)", Observation{HoldLease: true, Local: dataStoppedLSN, Peers: []PeerState{gossipPeer("pg-2", 5, 5, 0x200)}}, StartLocal, ""},

		// --- PeersPending: cold-boot wait until peers' true positions are observable ---
		// A standby holder must not promote while a peer is still coming up (its true
		// position not yet in -- recovery-mode will make it reachable).
		{"holder standby + peers pending -> wait", Observation{HoldLease: true, Local: localStandby, PeersPending: true}, Wait, ""},
		// A primary-state holder waits too (a newer-timeline peer may still be booting).
		{"holder primary-state stopped + peers pending -> wait", Observation{HoldLease: true, Local: dataStopped, PeersPending: true}, Wait, ""},
		// But a known-ahead peer triggers an immediate handoff even while pending.
		{"holder standby + ahead peer wins over pending -> release", Observation{HoldLease: true, Local: localStandby, PeersPending: true, Peers: []PeerState{standby("pg-2", 5, 5, 0x200)}}, ReleaseLease, "pg-2"},

		// --- #181: a STARTING node (process alive, SQL not ready) must not be acted
		// on via its stale on-disk role. Right after a clone, controldata still shows
		// the source's primary state; without the guard this is the RejoinForward that
		// kills the freshly-started standby's walreceiver, and the StartLocal loop.
		// The race itself: non-holder, on-disk primary-state (stale), a same-timeline
		// primary peer, postmaster alive -> WAIT, not RejoinForward.
		{"#181 starting node, stale primary-state + same-tl primary -> wait (not rejoin)", Observation{HoldLease: false, LocalProcessAlive: true, Local: dataStopped, Peers: []PeerState{primary("pg-1", 5, 5, 0x100)}}, Wait, ""},
		// Non-holder starting standby -> wait, not the StartLocal loop.
		{"#181 starting standby (non-holder) -> wait (not StartLocal)", Observation{HoldLease: false, LocalProcessAlive: true, Local: dataStoppedStandby}, Wait, ""},
		// Holder primary-state resume that is still starting -> wait until SQL-ready.
		{"#181 holder primary-state starting -> wait", Observation{HoldLease: true, LocalProcessAlive: true, Local: dataStopped}, Wait, ""},
		// Contrast: with the postmaster NOT alive, the same observation still starts
		// (StartLocal) -- the guard only suppresses action while it is coming up.
		{"#181 contrast: holder primary-state stopped (process dead) -> StartLocal", Observation{HoldLease: true, LocalProcessAlive: false, Local: dataStopped}, StartLocal, ""},
		// Self-health is NOT pre-empted: a previously-ready primary now frozen
		// (LocalStuck) still fails over even though its postmaster is alive.
		{"#181 frozen primary (process alive + stuck) -> release for failover", Observation{HoldLease: true, LocalProcessAlive: true, LocalStuck: true, Local: dataStopped, Peers: []PeerState{standby("pg-1", 5, 5, 0x80)}}, ReleaseLease, ""},
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

// #29: cascading replication. cascadeFollowTarget is a pure chooser; default off and
// every unsafe case falls back to the leader so a standby is never stranded.
func TestCascadeFollowTarget(t *testing.T) {
	localTL5 := LocalState{Timeline: tl(5), TimelineOK: true}
	// unreachable peer (not gossip): position unknown, never a cascade upstream.
	down := func(name string) PeerState { return PeerState{Name: name, Reachable: false} }

	cases := []struct {
		name   string
		o      Observation
		expect string
	}{
		// Off -> always the leader, even with perfectly good intermediate standbys.
		{"cascade off -> leader", Observation{Cascade: false, LeaderIdentity: "pg-0", LocalNode: "pg-3", Local: localTL5,
			Peers: []PeerState{standby("pg-1", 5, 5, 1), standby("pg-2", 5, 5, 1)}}, "pg-0"},
		// On: chain by ordinal toward the leader -> the immediate upstream (closest below self).
		{"on: pg-3 follows pg-2 (immediate upstream)", Observation{Cascade: true, LeaderIdentity: "pg-0", LocalNode: "pg-3", Local: localTL5,
			Peers: []PeerState{standby("pg-1", 5, 5, 1), standby("pg-2", 5, 5, 1)}}, "pg-2"},
		{"on: pg-2 follows pg-1", Observation{Cascade: true, LeaderIdentity: "pg-0", LocalNode: "pg-2", Local: localTL5,
			Peers: []PeerState{standby("pg-1", 5, 5, 1)}}, "pg-1"},
		// Adjacent to the leader -> nothing safe to interpose -> leader.
		{"on: pg-1 adjacent to leader -> leader", Observation{Cascade: true, LeaderIdentity: "pg-0", LocalNode: "pg-1", Local: localTL5,
			Peers: []PeerState{standby("pg-2", 5, 5, 1)}}, "pg-0"},
		// Leader elsewhere in ordinal space (post-failover): chain still works by distance.
		{"on: leader pg-2, pg-0 follows pg-1", Observation{Cascade: true, LeaderIdentity: "pg-2", LocalNode: "pg-0", Local: localTL5,
			Peers: []PeerState{standby("pg-1", 5, 5, 1)}}, "pg-1"},
		// No SAFE upstream -> leader (re-home). Intermediate down/divergent/primary/gossip excluded.
		{"on: only intermediate is down -> leader", Observation{Cascade: true, LeaderIdentity: "pg-0", LocalNode: "pg-3", Local: localTL5,
			Peers: []PeerState{down("pg-1"), down("pg-2")}}, "pg-0"},
		{"on: divergent-timeline intermediate excluded -> leader", Observation{Cascade: true, LeaderIdentity: "pg-0", LocalNode: "pg-3", Local: localTL5,
			Peers: []PeerState{standby("pg-2", 4, 5, 1)}}, "pg-0"},
		{"on: primary-role intermediate excluded -> leader", Observation{Cascade: true, LeaderIdentity: "pg-0", LocalNode: "pg-3", Local: localTL5,
			Peers: []PeerState{primary("pg-2", 5, 5, 1)}}, "pg-0"},
		{"on: gossip-only intermediate excluded -> leader", Observation{Cascade: true, LeaderIdentity: "pg-0", LocalNode: "pg-3", Local: localTL5,
			Peers: []PeerState{gossipPeer("pg-2", 5, 5, 1)}}, "pg-0"},
		// Mixed: pg-2 down, pg-1 healthy -> falls through to pg-1 (still a valid closer upstream).
		{"on: nearest down, next healthy -> next", Observation{Cascade: true, LeaderIdentity: "pg-0", LocalNode: "pg-3", Local: localTL5,
			Peers: []PeerState{standby("pg-1", 5, 5, 1), down("pg-2")}}, "pg-1"},
		// Defensive: unparseable ordinals / empty leader -> leader (never strand).
		{"on: unparseable local ordinal -> leader", Observation{Cascade: true, LeaderIdentity: "pg-0", LocalNode: "weird", Local: localTL5,
			Peers: []PeerState{standby("pg-1", 5, 5, 1)}}, "pg-0"},
		{"on: empty leader -> empty (no upstream yet)", Observation{Cascade: true, LeaderIdentity: "", LocalNode: "pg-3", Local: localTL5,
			Peers: []PeerState{standby("pg-1", 5, 5, 1)}}, ""},
		// local timeline not yet known -> never cascade (cannot verify same line).
		{"on: local timeline unknown -> leader", Observation{Cascade: true, LeaderIdentity: "pg-0", LocalNode: "pg-3", Local: LocalState{TimelineOK: false},
			Peers: []PeerState{standby("pg-2", 5, 5, 1)}}, "pg-0"},
		// Stickiness (#29 thrash fix): when the upstream we already follow still qualifies,
		// keep it -- do NOT flap to a closer peer. This is the tick-B half of the oscillation:
		// we follow pg-1 (re-homed when pg-2 flapped down); pg-2 is back, but pg-1 still
		// qualifies, so STAY on pg-1 rather than switching back to pg-2 (which would re-run
		// `repmgr standby follow` and, if pg-2 keeps flapping, every tick).
		{"sticky: current upstream still qualifies -> keep it (no flap to closer peer)", Observation{Cascade: true, LeaderIdentity: "pg-0", LocalNode: "pg-3", Local: localTL5, CurrentUpstream: "pg-1",
			Peers: []PeerState{standby("pg-1", 5, 5, 1), standby("pg-2", 5, 5, 1)}}, "pg-1"},
		// But re-home when the current upstream stops qualifying -- that is when re-homing
		// is actually needed (never strand on a dead/diverged upstream).
		{"sticky: current upstream down -> re-home to next qualifying", Observation{Cascade: true, LeaderIdentity: "pg-0", LocalNode: "pg-3", Local: localTL5, CurrentUpstream: "pg-2",
			Peers: []PeerState{standby("pg-1", 5, 5, 1), down("pg-2")}}, "pg-1"},
		{"sticky: current upstream diverged -> re-home to next qualifying", Observation{Cascade: true, LeaderIdentity: "pg-0", LocalNode: "pg-3", Local: localTL5, CurrentUpstream: "pg-2",
			Peers: []PeerState{standby("pg-1", 5, 5, 1), standby("pg-2", 4, 5, 1)}}, "pg-1"},
		// A stale current upstream no longer present as a peer -> ignored, normal closest pick.
		{"sticky: current upstream absent -> normal closest pick", Observation{Cascade: true, LeaderIdentity: "pg-0", LocalNode: "pg-3", Local: localTL5, CurrentUpstream: "pg-9",
			Peers: []PeerState{standby("pg-1", 5, 5, 1), standby("pg-2", 5, 5, 1)}}, "pg-2"},
	}
	for _, c := range cases {
		if got := cascadeFollowTarget(c.o); got != c.expect {
			t.Errorf("%s: got %q want %q", c.name, got, c.expect)
		}
	}
}

// #29: the chooser is wired into Decide -- a running standby with cascade on and a safe
// upstream returns Follow targeting that upstream (not the leader); off -> the leader.
func TestDecideCascadeWiring(t *testing.T) {
	local := LocalState{HasData: true, Running: true, InRecovery: true, Timeline: tl(5), TimelineOK: true, LSN: ls(5, 0x100), LSNOK: true}
	peers := []PeerState{standby("pg-1", 5, 5, 1), standby("pg-2", 5, 5, 1)}
	off := Decide(Observation{HoldLease: false, LeaderIdentity: "pg-0", LocalNode: "pg-3", Local: local, Peers: peers})
	if off.Action != Follow || off.Target != "pg-0" {
		t.Errorf("cascade off: want Follow pg-0, got %v %q", off.Action, off.Target)
	}
	on := Decide(Observation{HoldLease: false, Cascade: true, LeaderIdentity: "pg-0", LocalNode: "pg-3", Local: local, Peers: peers})
	if on.Action != Follow || on.Target != "pg-2" {
		t.Errorf("cascade on: want Follow pg-2 (immediate upstream), got %v %q", on.Action, on.Target)
	}
}
