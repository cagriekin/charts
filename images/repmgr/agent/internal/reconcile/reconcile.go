// Package reconcile is the agent's brain: a pure function from an Observation of
// the world to a single Decision. It holds no state and performs no I/O, so it is
// exhaustively unit-testable and an agent crash mid-action recovers by simply
// re-observing and re-deciding (the crash-consistency property, Part H3).
//
// The Lease is the sole authority for who MAY be primary; these rules add the
// data-safety gates the Lease cannot express (most-advanced election, forward-only
// rewind, the stale-winner/highwater guard) so a node never serves or discards
// committed data incorrectly.
package reconcile

import "github.com/cagriekin/pg-ha-agent/internal/pg"

// Action is what the agent should do this tick.
type Action int

const (
	NoOp              Action = iota
	Wait                     // observe again; take no destructive action
	BootstrapInitdb          // empty data + lease + no live primary: initdb as primary
	BootstrapClone           // empty data + not the chosen primary: clone from Target
	Promote                  // standby holds lease, caught up & most-advanced: promote
	StayPrimary              // holds lease, already a current primary: assert routing only
	Follow                   // not holder, standby: follow Target (the leader)
	DemoteFence              // read-write without the lease: demote now (soft fence)
	RejoinForward            // local data behind Target's newer timeline: rewind forward
	ReleaseLease             // holds lease but must not serve (stale/behind/unhealthy): release + step down
	StartLocal               // initialized but stopped, and safe to start in its on-disk role
	RestartLocal             // stuck single-node primary: force-restart postgres in place (no peer to fail over to)
	StartRecovery            // non-holder primary-state data: start READ-ONLY (standby.signal) so its true position is observable
	Switchover               // operator-requested handoff: a caught-up target standby exists; clear the request + step down so it promotes
)

func (a Action) String() string {
	return [...]string{"NoOp", "Wait", "BootstrapInitdb", "BootstrapClone", "Promote",
		"StayPrimary", "Follow", "DemoteFence", "RejoinForward", "ReleaseLease", "StartLocal", "RestartLocal", "StartRecovery", "Switchover"}[a]
}

// Decision is the chosen action plus the peer it targets and a human reason for
// the audit trail (Part H6).
type Decision struct {
	Action Action
	Target string
	Reason string
}

func d(a Action, target, reason string) Decision { return Decision{Action: a, Target: target, Reason: reason} }

// LocalState is the local node's state. Timeline comes from pg_controldata when the
// node is not a running primary (so it is meaningful even for a stopped/standby node).
type LocalState struct {
	HasData    bool
	Running    bool
	InRecovery bool
	Timeline   pg.Timeline
	TimelineOK bool
	LSN        pg.LSN
	LSNOK      bool
}

// PeerState is a sibling's observed state. Position (Timeline/LSN) comes from a
// live SQL probe when Reachable, or from the peer's gossiped pod annotation when
// not (Gossip=true) -- the latter lets the most-advanced election rank a
// stopped/unreachable peer at cold boot. A gossip-only peer is never a rewind or
// follow target (it is unreachable); it only informs the release/handoff decision.
type PeerState struct {
	Name       string
	Reachable  bool
	Gossip     bool // position is from gossip (peer not SQL-reachable)
	Role       pg.Role
	Timeline   pg.Timeline
	TimelineOK bool
	LSN        pg.LSN
	LSNOK      bool
}

// MarkerState is the durable highwater marker (<fullname>-primary).
type MarkerState struct {
	Present   bool
	Malformed bool // #174: present but unparseable -> fail closed
	Timeline  pg.Timeline
}

// Observation is the full input to a decision.
type Observation struct {
	HoldLease      bool
	Paused         bool   // maintenance mode (Part H1): suspend automatic promote/demote/fence
	LeaderIdentity string // current lease holder (for followers); "" if unknown
	Local          LocalState
	Peers          []PeerState
	Marker         MarkerState
	// LocalStuck is set by the agent (a stateful, time-based signal) when a
	// previously-running local primary has been unreachable past the self-health
	// grace -- a frozen/wedged postmaster that Start cannot recover and the
	// reconcile-loop liveness probe won't catch. It drives self-health failover.
	LocalStuck bool
	// PeersPending is set by the agent only during the cold-boot window (shortly
	// after agent start) while some peer is not yet SQL-reachable. The holder waits
	// rather than promoting/resuming, so the most-advanced election runs against
	// peers' TRUE positions (recovery-mode brings stopped primary-state peers up
	// read-only and observable) instead of a checkpoint estimate or an empty set.
	// It is false in steady state, so a real failover is never delayed.
	PeersPending bool
	// SwitchoverTarget is the pod an operator requested a controlled handoff to
	// (the pg-ha/switchover-target marker annotation, Part H2); "" when none. The
	// serving primary steps down for it only once the target is a caught-up,
	// same-timeline standby (switchoverTargetReady) -- otherwise it keeps serving.
	SwitchoverTarget string
	// LocalProcessAlive is process liveness of the agent's supervised postmaster
	// (alive, distinct from Local.Running which is SQL reachability). A postmaster
	// replaying WAL toward consistency is alive but not yet SQL-ready. The agent
	// must not act on a starting node's stale on-disk role (#181).
	LocalProcessAlive bool
}

// Decide maps an Observation to the single action to take.
func Decide(o Observation) Decision {
	if o.Paused {
		return d(NoOp, "", "paused: automatic failover suspended (maintenance mode)")
	}

	// The agent's own postmaster is running but not yet accepting SQL connections:
	// the node is STARTING (e.g. a freshly-cloned standby replaying WAL to a
	// consistent point). Do not act on its on-disk role -- right after pg_basebackup
	// the control file still carries the source primary's "in production" state,
	// which would misread a starting standby as a stopped primary and trigger a
	// RejoinForward that kills its walreceiver mid-stream (#181). Wait for SQL to
	// confirm the role. A previously-ready node now frozen is caught by LocalStuck
	// below (self-health failover), so exclude that case.
	if o.LocalProcessAlive && !o.Local.Running && !o.LocalStuck {
		return d(Wait, "", "postgres is running but not yet accepting connections; waiting for it to reach a ready state")
	}

	newer := newestPrimaryAbove(o) // reachable live primary on a strictly newer timeline than local

	if o.HoldLease {
		switch {
		case !o.Local.HasData && !o.Local.Running:
			if p := anyReachablePrimary(o.Peers); p != nil {
				return d(BootstrapClone, p.Name, "hold lease but a live primary exists; clone, never initdb (avoids a divergent cluster)")
			}
			if o.Marker.Present || o.Marker.Malformed {
				return d(Wait, "", "empty data with a marker present; settle before initdb (PVC-loss recreate, #170)")
			}
			return d(BootstrapInitdb, "", "empty data, lease holder, fresh install: initdb as primary")

		case o.Local.Running && o.Local.InRecovery:
			if newer != nil {
				return d(RejoinForward, newer.Name, "standby holds lease but a peer is on a newer timeline; rejoin forward before promoting")
			}
			if bad, why := unsafeToServe(o); bad {
				return d(ReleaseLease, "", "refuse to promote: "+why)
			}
			if t, reachable := moreAdvancedPeer(o); t != "" {
				if reachable {
					// A live peer is ahead and can take over: hand off (invariant 8).
					return d(ReleaseLease, t, "a reachable peer has more WAL on the same timeline; release so the most-advanced node promotes (invariant 8)")
				}
				// Gossip-only (unreachable) peer is "ahead": at cold boot it may be a
				// stopped primary-state peer that will become observable via recovery
				// mode, so wait for it. In steady state it is almost always the
				// just-dead primary whose annotation is still fresh -- a corpse that can
				// never reacquire, so handing off to it would only flap the lease and
				// delay failover. Do not hand off; fall through to promote.
				if o.PeersPending {
					return d(Wait, "", "cold boot: a gossip-only peer reports more WAL; wait for it to become observable before promoting (it may promote to a newer timeline)")
				}
			}
			if o.PeersPending {
				return d(Wait, "", "cold boot: waiting for peers to report their position before promoting (recovery-mode makes stopped primary-state peers observable)")
			}
			return d(Promote, "", "standby holds lease, caught up and most-advanced: promote")

		case o.Local.Running && !o.Local.InRecovery:
			if newer != nil {
				return d(RejoinForward, newer.Name, "primary holds lease but a peer is on a newer timeline (anomaly); demote and rejoin")
			}
			if bad, why := unsafeToServe(o); bad {
				return d(ReleaseLease, "", "primary must not serve: "+why)
			}
			if t := switchoverTargetReady(o); t != "" {
				return d(Switchover, t, "controlled switchover: target "+t+" is a caught-up same-timeline standby; clear the request and step down so it promotes")
			}
			if o.SwitchoverTarget != "" {
				// Requested but not yet actionable: keep serving and surface why in the
				// audit trail (the target is lagging, unreachable, divergent, not a
				// standby, or names self/an unknown pod) so the stuck request is
				// diagnosable rather than a silent no-op.
				return d(StayPrimary, "", "primary holds lease; switchover to "+o.SwitchoverTarget+" requested but the target is not yet a caught-up, reachable, same-timeline standby")
			}
			return d(StayPrimary, "", "primary holds lease and is current")

		default: // has data, not running
			if newer != nil {
				return d(RejoinForward, newer.Name, "lease holder has data on an older timeline; rejoin forward before starting (never start stale data read-write)")
			}
			if !o.Local.InRecovery {
				// On-disk primary state: starting comes up read-write, so apply the
				// highwater guard first (#125/#171/#173/#174) -- never start stale data
				// read-write even as the lease holder.
				if bad, why := unsafeToServe(o); bad {
					return d(ReleaseLease, "", "stopped primary-state data must not start read-write: "+why)
				}
				// Self-health: a previously-running primary stuck unreachable past the
				// grace (frozen/wedged) cannot be recovered by Start. Fail over to a
				// standby if one exists; on a single node force-restart in place (no
				// peer to take over -- releasing would strand the only node).
				if o.LocalStuck {
					if len(o.Peers) == 0 {
						return d(RestartLocal, "", "single-node primary stuck unhealthy; force-restart postgres in place")
					}
					return d(ReleaseLease, "", "primary stuck unhealthy and standbys exist; release the lease for failover (self-health)")
				}
				// At cold boot, wait until peers are observable before resuming as
				// primary: a peer that promoted to a NEWER timeline may still be coming
				// up (recovery-mode), and `newer` only sees reachable peers. A primary
				// source is never behind its own same-timeline peers, so no LSN compare
				// is needed here once no newer-timeline peer exists.
				if o.PeersPending {
					return d(Wait, "", "cold boot: waiting for peers before resuming as primary (a newer-timeline peer may still be coming up)")
				}
			}
			return d(StartLocal, "", "lease holder, initialized but stopped: start (role per on-disk data)")
		}
	}

	// Non-holder.
	switch {
	case o.Local.Running && !o.Local.InRecovery:
		return d(DemoteFence, "", "read-write without the lease; demote now (soft fence)")

	case o.Local.Running && o.Local.InRecovery:
		if o.LeaderIdentity == "" {
			return d(Wait, "", "standby but no known leader; keep the current upstream")
		}
		return d(Follow, o.LeaderIdentity, "standby; follow the lease holder")

	case !o.Local.HasData && !o.Local.Running:
		if p := primaryNamed(o, o.LeaderIdentity); p != nil {
			return d(BootstrapClone, p.Name, "empty data; clone from the lease holder")
		}
		if p := anyReachablePrimary(o.Peers); p != nil {
			return d(BootstrapClone, p.Name, "empty data; clone from the live primary")
		}
		return d(Wait, "", "empty data, no reachable primary yet; wait (never initdb as a non-holder)")

	default: // has data, not running
		if newer != nil {
			return d(RejoinForward, newer.Name, "has data on an older timeline; rejoin forward, then follow")
		}
		if !o.Local.InRecovery {
			// On-disk primary state without the lease. Never start read-write -- it
			// would come up primary, then be fenced next tick (the flap). Rejoin a
			// same-timeline live primary as a standby if one exists (split-brain
			// recovery; pg_rewind/reclone handles the divergence).
			if p := sameTimelinePrimary(o); p != nil {
				return d(RejoinForward, p.Name, "stopped primary-state data without the lease; a same-timeline primary exists, rejoin it as a standby")
			}
			// Recovery mode is only for a genuine cold-boot election, signalled by
			// another node already holding the lease: come up READ-ONLY (standby.signal)
			// so the holder can rank our true position. With NO leader yet, the lease is
			// merely settling (acquisition is async) -- a fresh or sole primary that will
			// win it. Do NOT enter recovery mode then: a fresh master has no repmgr
			// record, so it could never promote back out, deadlocking the bootstrap
			// (the standby's clone waits for this node to become a registered primary).
			if o.LeaderIdentity != "" {
				return d(StartRecovery, "", "stopped primary-state data, another node holds the lease; start read-only (recovery mode) so its true position is observable for the election")
			}
			return d(Wait, "", "stopped primary-state data, no leader yet; wait to acquire the lease (a fresh/sole primary must not enter recovery mode -- it has no repmgr record to promote out of)")
		}
		return d(StartLocal, "", "standby-state data, stopped: start as a standby")
	}
}

// sameTimelinePrimary returns a reachable live primary on exactly the local
// timeline, or nil. It is the rejoin target for a stopped primary-state non-holder
// (a strictly-newer primary is handled earlier by newestPrimaryAbove; a primary on
// a lower timeline is stale and must never be a rejoin source -- forward-only,
// invariant 5).
func sameTimelinePrimary(o Observation) *PeerState {
	if !o.Local.TimelineOK {
		return nil
	}
	for i := range o.Peers {
		p := &o.Peers[i]
		if p.Reachable && p.Role == pg.RolePrimary && p.TimelineOK && p.Timeline == o.Local.Timeline {
			return p
		}
	}
	return nil
}

// Cold-boot election (full-cluster restart, podManagementPolicy: Parallel).
// Leadership is granted arbitrarily by the Lease, so a less-advanced node can win.
// Two gates converge the cluster on the right primary WITHOUT two writers:
//   - cross-timeline: a winner below the marker highwater is unsafeToServe ->
//     ReleaseLease, which now actually releases the Lease (DCS.Release), so the
//     highest-timeline node (its timeline == the marker it wrote) wins the freed
//     Lease and serves. This is the C2 "lower-timeline pod boots first" case.
//   - same-timeline peers: a non-holder primary-state node is started READ-ONLY in
//     recovery mode (StartRecovery / standby.signal), so it replays its own WAL to
//     the TRUE end and reports an exact LSN over SQL -- no two-writer, no checkpoint
//     estimate. moreAdvancedPeer (holder-standby branch) then ranks it precisely and
//     a less-advanced standby winner releases for it. PeersPending makes the holder
//     wait through the cold-boot window until peers are SQL-reachable, so it does not
//     promote before those true positions are in. LSN gossip remains a secondary
//     signal for a transiently-unreachable peer (a checkpoint lower bound).
//   - a primary source is never behind its own same-timeline standbys, so a
//     primary-state holder resumes directly (StartLocal, crash recovery, no timeline
//     bump) once no NEWER-timeline peer exists -- no LSN compare needed.
//
// RESIDUAL (documented, cold-boot RPO only -- never a two-writer risk):
//   - Divergent SAME-timeline split-brain (two nodes were both primaries on one
//     timeline with forked WAL): recovery-mode brings the loser up as a read-only
//     standby (safe), but its divergent tail is discarded on rejoin. This is the
//     marker/fence's domain, not the LSN election's.
//   - A peer whose agent is also dead (never SQL-reachable, no fresh gossip) through
//     the whole cold-boot window: once PeersPending's grace elapses the holder
//     proceeds -- a double-fault, not the common cold boot.
//
// unsafeToServe ports the evaluate_lone_primary guard (#171/#173/#174): refuse to
// promote/serve when the marker is malformed, the local timeline is unreadable
// (even with no marker), the local timeline is below the highwater, or a different
// node shares the marker timeline (equal-timeline split-brain with no LSN to compare).
func unsafeToServe(o Observation) (bool, string) {
	if o.Marker.Malformed {
		return true, "marker malformed (#174)"
	}
	if !o.Local.TimelineOK {
		return true, "local timeline unreadable (#173)"
	}
	if o.Marker.Present {
		if o.Local.Timeline < o.Marker.Timeline {
			return true, "timeline below recorded highwater (#125)"
		}
		if o.Local.Timeline == o.Marker.Timeline {
			for _, p := range o.Peers {
				if p.Reachable && p.Role == pg.RolePrimary && p.TimelineOK && p.Timeline == o.Marker.Timeline {
					return true, "another node shares the marker timeline (#171 equal-timeline split-brain)"
				}
			}
		}
	}
	return false, ""
}

// newestPrimaryAbove returns the reachable live primary on the highest timeline
// strictly above the local timeline, or nil. Used for forward-only rejoin.
func newestPrimaryAbove(o Observation) *PeerState {
	if !o.Local.TimelineOK {
		return nil
	}
	var best *PeerState
	for i := range o.Peers {
		p := &o.Peers[i]
		if !p.Reachable || p.Role != pg.RolePrimary || !p.TimelineOK {
			continue
		}
		if p.Timeline <= o.Local.Timeline {
			continue
		}
		if best == nil || p.Timeline > best.Timeline {
			best = p
		}
	}
	return best
}

func anyReachablePrimary(peers []PeerState) *PeerState {
	for i := range peers {
		if peers[i].Reachable && peers[i].Role == pg.RolePrimary {
			return &peers[i]
		}
	}
	return nil
}

func primaryNamed(o Observation, name string) *PeerState {
	if name == "" {
		return nil
	}
	for i := range o.Peers {
		if o.Peers[i].Name == name && o.Peers[i].Reachable && o.Peers[i].Role == pg.RolePrimary {
			return &o.Peers[i]
		}
	}
	return nil
}

// moreAdvancedPeer returns the name of the peer strictly ahead of the local node
// (higher timeline, or same timeline + higher LSN), or "", and whether that peer is
// currently SQL-reachable. A reachable peer can actually take over (a real handoff
// target); an unreachable, gossip-only peer cannot -- it may be the just-dead
// primary whose annotation is still fresh, so handing the lease to it would never
// complete. The caller distinguishes the two (see the holder-standby branch).
func moreAdvancedPeer(o Observation) (string, bool) {
	best := ""
	bestReachable := false
	var bestTL pg.Timeline
	var bestLSN pg.LSN
	for i := range o.Peers {
		p := &o.Peers[i]
		if !p.Reachable && !p.Gossip {
			continue // no known position for this peer
		}
		if !peerAhead(*p, o.Local) {
			continue
		}
		if best == "" || ahead(p.Timeline, p.TimelineOK, p.LSN, p.LSNOK, bestTL, true, bestLSN, true) {
			best, bestReachable, bestTL, bestLSN = p.Name, p.Reachable, p.Timeline, p.LSN
		}
	}
	return best, bestReachable
}

func peerAhead(p PeerState, l LocalState) bool {
	return ahead(p.Timeline, p.TimelineOK, p.LSN, p.LSNOK, l.Timeline, l.TimelineOK, l.LSN, l.LSNOK)
}

// ahead reports whether (aTL,aLSN) ranks strictly above (bTL,bLSN): timeline
// dominates, then LSN. Unknown timelines fall through to an LSN comparison.
func ahead(aTL pg.Timeline, aTLok bool, aLSN pg.LSN, aLSNok bool, bTL pg.Timeline, bTLok bool, bLSN pg.LSN, bLSNok bool) bool {
	if aTLok && bTLok {
		if aTL != bTL {
			return aTL > bTL
		}
	}
	if aLSNok && bLSNok {
		return aLSN.Greater(bLSN)
	}
	return false
}

// switchoverTargetReady returns o.SwitchoverTarget when an operator has requested a
// controlled handoff (Part H2) AND that target is safe to promote: a DIFFERENT pod
// that is a reachable (SQL-confirmed, not gossip), same-timeline standby caught up
// to the serving primary's WAL position (its replay/receive LSN >= the local
// primary's LSN, invariant 8). Until then it returns "" and the primary keeps
// serving -- so the handoff never discards already-committed data. An empty/self/
// unreachable/divergent/lagging target all defer. (Self-target naturally defers:
// peers exclude self, so it is never found.)
func switchoverTargetReady(o Observation) string {
	t := o.SwitchoverTarget
	if t == "" || !o.Local.TimelineOK || !o.Local.LSNOK {
		return ""
	}
	for i := range o.Peers {
		p := &o.Peers[i]
		if p.Name != t {
			continue
		}
		if !p.Reachable || p.Role != pg.RoleStandby {
			return "" // must be a live, in-recovery standby (a confirmed position)
		}
		if !p.TimelineOK || p.Timeline != o.Local.Timeline {
			return "" // different timeline -> not a clean same-line handoff
		}
		if !p.LSNOK || o.Local.LSN.Greater(p.LSN) {
			return "" // target has not caught up to the primary's WAL position yet
		}
		return t
	}
	return "" // requested target is not among the observed peers
}
