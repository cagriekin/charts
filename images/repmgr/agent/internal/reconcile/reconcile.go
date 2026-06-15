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
)

func (a Action) String() string {
	return [...]string{"NoOp", "Wait", "BootstrapInitdb", "BootstrapClone", "Promote",
		"StayPrimary", "Follow", "DemoteFence", "RejoinForward", "ReleaseLease", "StartLocal", "RestartLocal"}[a]
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
}

// Decide maps an Observation to the single action to take.
func Decide(o Observation) Decision {
	if o.Paused {
		return d(NoOp, "", "paused: automatic failover suspended (maintenance mode)")
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
			if t := moreAdvancedPeer(o); t != "" {
				return d(ReleaseLease, t, "a peer has more WAL on the same timeline; release so the most-advanced node promotes (invariant 8)")
			}
			return d(Promote, "", "standby holds lease, caught up and most-advanced: promote")

		case o.Local.Running && !o.Local.InRecovery:
			if newer != nil {
				return d(RejoinForward, newer.Name, "primary holds lease but a peer is on a newer timeline (anomaly); demote and rejoin")
			}
			if bad, why := unsafeToServe(o); bad {
				return d(ReleaseLease, "", "primary must not serve: "+why)
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
				// Most-advanced election (invariant 8): if a peer has more WAL on this
				// timeline -- reachable, or stopped-but-gossiping its position -- release
				// so it wins the lease and serves, rather than starting here and
				// discarding its tail (the cold-boot RPO gap LSN gossip closes).
				if t := moreAdvancedPeer(o); t != "" {
					return d(ReleaseLease, t, "a peer has more WAL on this timeline (incl. gossip); release so the most-advanced node serves (invariant 8)")
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
			// recovery; pg_rewind/reclone handles the divergence), else hold.
			if p := sameTimelinePrimary(o); p != nil {
				return d(RejoinForward, p.Name, "stopped primary-state data without the lease; a same-timeline primary exists, rejoin it as a standby")
			}
			return d(Wait, "", "stopped primary-state data without the lease and no rejoin target; hold (never start read-write)")
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
//   - same-timeline peers: moreAdvancedPeer hands off to a peer with more WAL on
//     the local timeline -- whether reachable (live LSN) or stopped-but-gossiping
//     its pg_controldata LSN to its pod annotation (LSN gossip). A stopped
//     primary-state ex-primary the flap-fix holds is therefore no longer invisible
//     at election time: its position is ranked and the less-advanced winner
//     releases for it.
//
// RESIDUAL (documented, cold-boot RPO only -- never a two-writer risk):
//   - A stopped node gossips its pg_controldata CHECKPOINT LSN. After a graceful
//     shutdown that equals the true end-of-WAL (shutdown checkpoint), so a planned
//     full-cluster restart ranks exactly. After an UNGRACEFUL crash (power loss,
//     OOM-kill) the checkpoint lags the true end by up to a checkpoint interval of
//     WAL. A running standby reports its LIVE replay LSN, which can sit ahead of a
//     crashed primary's last checkpoint -- so the crashed ex-primary can be
//     under-ranked and a less-advanced standby promoted, discarding the ex-primary's
//     acknowledged tail when it rejoins. This is no worse than before gossip, but it
//     is NOT fully closed for crash cold-boots. The exact fix (deferred): start a
//     held primary-state node with standby.signal so it replays its own WAL to the
//     true end and reports an exact LSN read-only (then it is SQL-reachable and
//     ranked precisely, no gossip estimate). Within async replication's inherent
//     RPO>0 this lost tail was also un-replicated, but at cold boot the ex-primary
//     IS present, so a sound election could have preserved it.
//   - A peer whose agent is also dead (no fresh gossip, not SQL-reachable) is ranked
//     unknown and the holder proceeds -- a double-fault, not the common cold boot.
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

// moreAdvancedPeer returns the name of a reachable peer strictly ahead of the
// local node (higher timeline, or same timeline + higher LSN), or "". This is the
// handoff target when the lease holder is not the most-advanced replica.
func moreAdvancedPeer(o Observation) string {
	best := ""
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
			best, bestTL, bestLSN = p.Name, p.Timeline, p.LSN
		}
	}
	return best
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
