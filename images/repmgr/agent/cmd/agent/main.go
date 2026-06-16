// Command agent is the PID-1 PostgreSQL HA agent: it holds a Kubernetes Lease as
// the sole authority for who is primary and drives repmgr (the Mechanism) to act.
//
// This file is the integration/wiring: config -> DCS + Mechanism + Supervisor +
// k8s routing + reconcile + observe, run as a tick loop with a synchronous OnLost
// fence. Package-level logic is unit-tested in internal/*; the end-to-end behavior
// (start/standby transitions, promotion, fencing) is validated by the chart's KinD
// agent-failover suite.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cagriekin/pg-ha-agent/internal/config"
	"github.com/cagriekin/pg-ha-agent/internal/dcs"
	"github.com/cagriekin/pg-ha-agent/internal/k8s"
	"github.com/cagriekin/pg-ha-agent/internal/mechanism"
	"github.com/cagriekin/pg-ha-agent/internal/observe"
	"github.com/cagriekin/pg-ha-agent/internal/pg"
	"github.com/cagriekin/pg-ha-agent/internal/process"
	"github.com/cagriekin/pg-ha-agent/internal/reconcile"
)

const (
	metricsAddr      = ":9200"
	pgBindir         = "/usr/lib/postgresql/18/bin"
	pgControlDataBin = pgBindir + "/pg_controldata"
	repmgrConf       = "/etc/repmgr/repmgr.conf"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.FromEnv()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	log.Info("starting pg-ha-agent", "config", cfg.String())

	if cfg.DCSBackend != "kubernetes" {
		// etcd backend is a planned addition (plan Part G); only kubernetes ships now.
		log.Error("unsupported DCS backend", "backend", cfg.DCSBackend)
		os.Exit(1)
	}

	a, err := newAgent(cfg, log)
	if err != nil {
		log.Error("init", "err", err)
		os.Exit(1)
	}
	a.run()
}

type agent struct {
	cfg    *config.Config
	log    *slog.Logger
	dcs    *dcs.K8sDCS
	kube   *k8s.Client
	mech   *mechanism.Repmgr
	sup    *process.Supervisor
	prober *pg.Prober
	metr   *observe.Metrics
	health *selfHealthTracker
	base   string    // StatefulSet name (pod name without the ordinal)
	bootAt time.Time // agent start; the cold-boot grace fallback for PeersPending is measured from here
	// peersSeen latches which peers have been SQL-reachable at least once this
	// lifetime. Once all have, the cold-boot wait never applies again -- so a
	// steady-state failover is not delayed by a recent agent/pod restart.
	peersSeen map[string]bool
	// followUpstream is the leader this standby is currently registered/configured
	// to follow, so repmgr standby follow (which reconfigures and can restart the
	// server) runs only when the upstream actually changes, not every tick. Reset
	// on any non-Follow action.
	followUpstream string

	// gossip publish state: skip re-patching the pod annotation when the position
	// is unchanged, refreshing only on change or a heartbeat (to keep it fresh).
	lastPubPos k8s.NodeStatus // position fields only (UpdatedAtUnix zeroed)
	lastPubAt  time.Time

	// opMu serializes all postmaster/mechanism mutations so the reconcile tick and
	// the OnLost fence callback never drive the supervisor concurrently (single
	// transition path; also avoids a concurrent-Stop deadlock).
	opMu sync.Mutex
}

func newAgent(cfg *config.Config, log *slog.Logger) (*agent, error) {
	d, err := dcs.NewK8sDCS(dcs.K8sConfig{
		Namespace:     cfg.Namespace,
		LeaseName:     cfg.LeaseName,
		LeaseDuration: cfg.LeaseDuration,
		RenewDeadline: cfg.RenewDeadline,
		RetryPeriod:   cfg.RetryPeriod,
	})
	if err != nil {
		return nil, err
	}
	kube, err := k8s.New(cfg.Namespace)
	if err != nil {
		return nil, err
	}
	return &agent{
		cfg:    cfg,
		log:    log,
		dcs:    d,
		kube:   kube,
		mech:   mechanism.NewRepmgr(repmgrConf, cfg.PGDATA, cfg.RepmgrPassword),
		sup:    process.NewSupervisor(process.NewChildPostmaster(pgBindir+"/postgres", cfg.PGDATA)),
		prober: pg.NewProber(),
		metr:   observe.New(),
		// Self-health grace scales with the lease timing (the cloud preset widens
		// both), tolerating a transient stall before declaring the primary wedged.
		health: &selfHealthTracker{grace: cfg.LeaseDuration},
		base:      baseName(cfg.PodName),
		bootAt:    time.Now(),
		peersSeen: map[string]bool{},
	}, nil
}

// bootstrapGrace is the cold-boot window during which the holder waits for peers to
// become SQL-reachable before promoting/resuming (so the most-advanced election
// sees true positions). Measured from agent start, so a steady-state failover --
// where the agent has long been up -- never waits. Scales with the lease timing.
func (a *agent) bootstrapGrace() time.Duration { return 2 * a.cfg.LeaseDuration }

func (a *agent) run() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go a.startMetrics(ctx)

	// Leadership: OnLost demotes synchronously (the fence-ordering guarantee)
	// before the lock can be re-acquired by anyone.
	go a.dcs.Run(ctx, a.cfg.PodName, dcs.Callbacks{
		OnAcquired: func(context.Context) {
			a.metr.SetLeader(true)
			a.log.Info("acquired leadership")
		},
		OnLost: func() {
			a.metr.SetLeader(false)
			a.metr.IncFence()
			a.log.Warn("lost leadership; demoting (fence)")
			a.opMu.Lock()
			defer a.opMu.Unlock()
			dctx, cancel := context.WithTimeout(context.Background(), a.cfg.RenewDeadline)
			defer cancel()
			if err := a.sup.Demote(dctx, true); err != nil {
				a.log.Error("fence demote failed", "err", err)
			}
		},
	})

	if err := a.boot(ctx); err != nil {
		a.log.Error("boot", "err", err)
	}

	ticker := time.NewTicker(a.cfg.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			a.log.Info("shutting down; releasing lease (ctx cancel) and stopping postgres")
			// Serialize with the leaderelection OnLost demote (which also fires on
			// this ctx cancel): two concurrent Stops share one single-delivery exit
			// channel, so the second would block forever waiting on an exit the first
			// already consumed. opMu makes them sequential -- the second sees cmd
			// already cleared and no-ops.
			sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			a.opMu.Lock()
			_ = a.sup.Demote(sctx, false) // graceful (fast) on planned shutdown
			a.opMu.Unlock()
			cancel()
			return
		case <-ticker.C:
			a.tick(ctx)
		}
	}
}

// boot generates repmgr.conf (failover=manual) and starts Postgres if the data dir
// is already initialized (initdb/clone of a fresh node is handled by the entrypoint
// before the agent starts, or by the reconcile loop's clone path).
func (a *agent) boot(ctx context.Context) error {
	nid := mechanism.NodeIdentity{
		NodeID:   nodeID(a.cfg.PodName),
		NodeName: a.cfg.PodName,
		FQDN:     a.fqdn(a.cfg.PodName),
		DataDir:  a.cfg.PGDATA,
		PGBindir: pgBindir,
		ReplUser: a.cfg.RepmgrUser,
		ReplDB:   a.cfg.RepmgrDB,
	}
	if err := a.mech.GenerateConfig(ctx, nid, mechanism.ConfigOpts{Failover: "manual", UseReplicationSlots: true}); err != nil {
		return err
	}
	// Streaming replication authenticates as the repmgr user via primary_conninfo,
	// which is deliberately passwordless (the password is not stored in repmgr.conf
	// -- the PR1 hardening). Without a credential the standby's walreceiver fails
	// with "no password supplied", so write a 0600 ~/.pgpass libpq picks up.
	if err := a.writePgpass(); err != nil {
		return err
	}
	if !process.HasData(a.cfg.PGDATA) {
		return nil
	}
	// Only bring up data that is safe to start regardless of holdership: a
	// standby-state node comes up in recovery and follows its upstream. Primary-state
	// data is deferred to the reconcile loop, which starts it (StartLocal) only when
	// this node holds the lease and passes the highwater guard -- otherwise a fenced
	// ex-primary would come up read-write before the lease state is known and flap.
	cd, err := pg.ReadControlData(ctx, pg.OSExec{}, pgControlDataBin, a.cfg.PGDATA)
	if err != nil {
		a.log.Warn("boot: read pg_controldata; deferring start to reconcile", "err", err)
		return nil
	}
	if cd.InRecovery {
		return a.sup.Start(ctx)
	}
	a.log.Info("boot: on-disk primary state; deferring start until reconcile confirms holdership + highwater", "state", cd.State)
	return nil
}

func (a *agent) tick(ctx context.Context) {
	a.metr.Beat()
	obs := a.observe(ctx)
	a.publishStatus(ctx, obs.Local)
	dec := reconcile.Decide(obs)
	observe.Audit(a.log, obs.HoldLease, dec.Action.String(), dec.Target, dec.Reason)
	a.opMu.Lock()
	err := a.act(ctx, dec, obs)
	a.opMu.Unlock()
	if err != nil {
		a.metr.IncReconcileError()
		a.log.Error("act", "action", dec.Action.String(), "err", err)
	}
}

func (a *agent) observe(ctx context.Context) reconcile.Observation {
	local := a.prober.Probe(ctx, a.selfConn())
	ls := reconcile.LocalState{
		HasData:    process.HasData(a.cfg.PGDATA),
		Running:    local.Reachable,
		InRecovery: local.Role == pg.RoleStandby,
		Timeline:   local.Timeline,
		TimelineOK: local.TimelineOK,
		LSN:        local.WriteLSN,
		LSNOK:      local.LSNOK,
	}
	// When postgres is not running its timeline/role are unreadable via SQL. Fall
	// back to pg_controldata so the forward-rejoin and highwater guards still apply
	// to a stopped node (without this a fenced primary-state node has no timeline,
	// is started read-write, and immediately fences -- the flap).
	if !ls.Running && ls.HasData {
		if cd, err := pg.ReadControlData(ctx, pg.OSExec{}, pgControlDataBin, a.cfg.PGDATA); err != nil {
			a.log.Warn("read pg_controldata", "err", err)
		} else {
			ls.Timeline, ls.TimelineOK = cd.Timeline, cd.TimelineOK
			ls.InRecovery = cd.InRecovery
			ls.LSN, ls.LSNOK = cd.LSN, cd.LSNOK // checkpoint LSN: position for gossip ranking while stopped
		}
	}
	o := reconcile.Observation{
		HoldLease:      a.dcs.IsLeader(),
		LeaderIdentity: a.dcs.Leader(),
		Local:          ls,
	}
	// Peers' gossiped positions (pod annotations) let the most-advanced election
	// rank a stopped/unreachable peer at cold boot. Only the holder consults gossip
	// (moreAdvancedPeer is holder-only; rewind/follow targets are reachable-only), so
	// non-holders skip the per-tick List. Best-effort: a read failure means no gossip.
	var gossip map[string]k8s.NodeStatus
	if o.HoldLease {
		g, gerr := a.kube.ReadPeerStatuses(ctx, a.cfg.PodSelector, a.cfg.PodName)
		if gerr != nil {
			a.log.Warn("read peer statuses (gossip)", "err", gerr)
		}
		gossip = g
	}
	for i := 0; i < a.cfg.NodeCount; i++ {
		name := a.base + "-" + strconv.Itoa(i)
		if name == a.cfg.PodName {
			continue
		}
		ns := a.prober.Probe(ctx, a.peerConn(name))
		ps := reconcile.PeerState{
			Name: name, Reachable: ns.Reachable, Role: ns.Role,
			Timeline: ns.Timeline, TimelineOK: ns.TimelineOK, LSN: ns.WriteLSN, LSNOK: ns.LSNOK,
		}
		// An unreachable peer with fresh gossip contributes its self-reported
		// position to the election (it is never a rewind/follow target -- only the
		// release/handoff decision uses it).
		if !ps.Reachable {
			if g, ok := gossip[name]; ok && a.gossipFresh(g) {
				ps.Gossip = true
				ps.Timeline, ps.TimelineOK = pg.Timeline(g.Timeline), g.TimelineOK
				ps.LSN, ps.LSNOK = pg.LSN{Hi: g.LSNHi, Lo: g.LSNLo}, g.LSNOK
			}
		}
		o.Peers = append(o.Peers, ps)
	}
	m, err := a.kube.ReadMarker(ctx, a.cfg.MarkerName)
	if err != nil {
		a.log.Warn("read marker", "err", err)
	}
	o.Marker = reconcile.MarkerState{
		Present:   m.Present,
		Malformed: m.Malformed || (m.Present && !m.TimelineOK),
		Timeline:  pg.Timeline(m.Timeline),
	}
	// Self-health (stateful/time-based, so computed here, not in the pure Decide):
	// a holder whose primary-state postgres has been unreachable past the grace is
	// stuck (frozen/wedged), which drives a self-health failover.
	shouldServe := o.HoldLease && o.Local.HasData && !o.Local.InRecovery
	o.LocalStuck = a.health.stuck(shouldServe, o.Local.Running, time.Now())
	// PeersPending: the holder waits (does not promote/resume) only during a true
	// cold boot -- some peer's true position is not yet in. The latch (peersSeen)
	// records peers ever SQL-reachable; once all have been seen, the wait never
	// applies again, so a steady-state failover is NOT delayed by a recent agent
	// restart (the dead primary being unreachable then does not re-arm the wait).
	// The bootstrap grace is a hard fallback so a genuinely-absent peer at cold boot
	// cannot block promotion forever.
	anyUnreachable := false
	for i := range o.Peers {
		if o.Peers[i].Reachable {
			a.peersSeen[o.Peers[i].Name] = true
		} else {
			anyUnreachable = true
		}
	}
	allSeen := len(a.peersSeen) >= len(o.Peers)
	// Only wait when a prior cluster existed (the marker records a past primary's
	// highwater) -- that is the cold-boot most-advanced election PeersPending guards.
	// At a FRESH install there is no marker and no election to wait for, so the sole
	// primary must not stall the ~grace waiting for a standby that is still cloning
	// FROM it (which would deadlock-by-delay the bootstrap).
	o.PeersPending = o.Marker.Present && anyUnreachable && !allSeen && time.Since(a.bootAt) < a.bootstrapGrace()
	return o
}

func (a *agent) act(ctx context.Context, dec reconcile.Decision, obs reconcile.Observation) error {
	// Any action other than Follow changes (or ends) this node's standby identity, so
	// the next Follow must re-register + repoint.
	if dec.Action != reconcile.Follow {
		a.followUpstream = ""
	}
	switch dec.Action {
	case reconcile.Promote:
		// The node is already running as a standby (the reconcile guard); promote
		// acts on the running postmaster — do NOT Start it (that would error).
		if err := a.mech.Promote(ctx); err != nil {
			return err
		}
		a.metr.IncPromotion()
		_ = a.mech.RegisterPrimary(ctx)
		// H3 order: promote PG -> advance the highwater marker -> assert routing.
		// Re-probe the post-promote timeline (promotion bumped it) and advance the
		// marker, so a later stale node is correctly refused by unsafeToServe.
		if tl, _, ok, _ := a.prober.PrimaryWALPosition(ctx, a.selfConn()); ok {
			a.advanceMarker(ctx, tl, true, obs.Marker)
		}
		return a.assertPrimaryRouting(ctx, obs)

	case reconcile.StayPrimary:
		// Register this primary in repmgr.nodes. In agent mode there is no repmgrd
		// sidecar to do it, and a fresh primary comes up via StartLocal (never
		// Promote), so without this repmgr.nodes stays empty and a standby's
		// init-clone (which waits for a registered primary) hangs forever. The
		// command is idempotent (--force) and self-healing; the standby clone needs
		// only that the primary is registered, so a best-effort per-tick reconcile is
		// fine (it succeeds within a tick or two of the primary opening).
		if err := a.mech.RegisterPrimary(ctx); err != nil {
			a.log.Warn("register primary in repmgr.nodes", "err", err)
		}
		// Keep the highwater marker at this primary's timeline (monotonic; written
		// only when it advances, so steady-state ticks make no API write).
		a.advanceMarker(ctx, obs.Local.Timeline, obs.Local.TimelineOK, obs.Marker)
		return a.assertPrimaryRouting(ctx, obs)

	case reconcile.Follow:
		// Ensure this standby has a repmgr.nodes record. In agent mode no repmgrd
		// sidecar registers it, and without the record BOTH repmgr standby follow and
		// a later promote fail ("unable to retrieve node record"). Registration must
		// happen while the upstream primary is reachable (now, before any failover),
		// so register here. Then repoint only when the upstream actually changes --
		// repmgr standby follow reconfigures and can restart the server, so running it
		// every tick on a healthy standby would churn it.
		if a.followUpstream == dec.Target {
			return nil
		}
		up := nodeID(dec.Target)
		if err := a.mech.RegisterStandby(ctx, up); err != nil {
			a.log.Warn("register standby in repmgr.nodes", "err", err)
		}
		if err := a.mech.Follow(ctx, up); err != nil {
			return err
		}
		a.followUpstream = dec.Target
		return nil

	case reconcile.DemoteFence:
		a.metr.IncDemote()
		return a.sup.Demote(ctx, true)

	case reconcile.RejoinForward:
		if err := a.sup.Demote(ctx, true); err != nil {
			return err
		}
		if err := a.mech.RejoinForceRewind(ctx, a.peerMechConn(dec.Target)); err != nil {
			if err := a.mech.ReclonePreserving(ctx, a.peerMechConn(dec.Target)); err != nil {
				return err
			}
		}
		return a.sup.Start(ctx)

	case reconcile.BootstrapClone:
		if err := a.mech.Clone(ctx, a.peerMechConn(dec.Target)); err != nil {
			return err
		}
		return a.sup.Start(ctx)

	case reconcile.ReleaseLease:
		// Step down: release the Lease so a peer can take over (stale-winner handoff
		// at cold boot, or self-health failover). Only stop postgres if we were
		// serving read-write; a standby is already read-only, so releasing the Lease
		// is enough and we avoid churning its postmaster.
		a.dcs.Release()
		if obs.Local.Running && !obs.Local.InRecovery {
			a.metr.IncDemote()
			return a.sup.Demote(ctx, false)
		}
		return nil

	case reconcile.StartLocal:
		// Bring an initialized-but-stopped node up in its on-disk role. The decision
		// table only chooses StartLocal when this is safe (holder + highwater-ok for
		// primary-state data, or any standby-state data); a non-holder's primary-state
		// data is routed to RejoinForward/StartRecovery instead, never read-write here.
		if !obs.Local.Running && obs.Local.HasData {
			if !obs.Local.InRecovery {
				// primary-state: clear any stray standby.signal so it opens read-write
				// (crash recovery on the same timeline -- a resume, not a promotion).
				if err := process.ClearRecoverySignal(a.cfg.PGDATA); err != nil {
					return err
				}
			}
			return a.sup.Start(ctx)
		}
		return nil

	case reconcile.StartRecovery:
		// Non-holder primary-state data: start READ-ONLY (standby.signal) so it
		// replays its WAL to the true end and is observable for the election, without
		// risking a second writer. It promotes only if it later wins the lease.
		if !obs.Local.Running && obs.Local.HasData {
			if err := process.SetRecoverySignal(a.cfg.PGDATA); err != nil {
				return err
			}
			return a.sup.Start(ctx)
		}
		return nil

	case reconcile.RestartLocal:
		// Single-node primary wedged (frozen/hung): no peer to fail over to, so
		// force-stop in place (Stop escalates to SIGKILL on the timeout if a frozen
		// postmaster ignores the signal) and start fresh.
		a.metr.IncDemote()
		rctx, cancel := context.WithTimeout(ctx, a.cfg.RenewDeadline)
		_ = a.sup.Stop(rctx, process.Immediate)
		cancel()
		return a.sup.Start(ctx)

	case reconcile.Wait, reconcile.NoOp, reconcile.BootstrapInitdb:
		// Inert: never auto-start here. Starting a stopped node is an explicit
		// StartLocal decision so primary-state data is never brought up read-write
		// without passing the holdership/highwater guard (fresh-node initdb is done by
		// the entrypoint before the agent starts).
		return nil
	}
	return nil
}

// pgpassPath is the postgres user's home in the repmgr image; written/owned by the
// postgres uid the agent runs as. Fixed (not $HOME) because after gosu the agent
// may inherit root's HOME, which postgres cannot write.
const pgpassPath = "/var/lib/postgresql/.pgpass"

// writePgpass writes a 0600 .pgpass with the repmgr replication credential so a
// passwordless primary_conninfo (the password is kept out of repmgr.conf -- the
// PR1 hardening) can still authenticate streaming replication. It also exports
// PGPASSFILE so the walreceiver child (and the agent's repmgr shells) find it
// regardless of HOME. Rewritten every boot; the home is ephemeral so the secret
// never persists on a volume. The wildcard host/port/db entry matches both
// replication and regular connections.
func (a *agent) writePgpass() error {
	// Escape '\' then ':' per the .pgpass format so a credential containing them
	// round-trips.
	esc := func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		return strings.ReplaceAll(s, `:`, `\:`)
	}
	line := fmt.Sprintf("*:*:*:%s:%s\n", esc(a.cfg.RepmgrUser), esc(a.cfg.RepmgrPassword))
	if err := os.WriteFile(pgpassPath, []byte(line), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", pgpassPath, err)
	}
	if err := os.Setenv("PGPASSFILE", pgpassPath); err != nil {
		return fmt.Errorf("set PGPASSFILE: %w", err)
	}
	return nil
}

// publishStatus gossips this node's WAL position to its own pod annotation so the
// lease holder can rank it at election time even when it is stopped/unreachable.
// It re-patches only when the position changed or a heartbeat (half the freshness
// window) elapsed, to avoid a pod write every tick on an idle node.
func (a *agent) publishStatus(ctx context.Context, ls reconcile.LocalState) {
	if !ls.HasData {
		return // nothing meaningful to report yet
	}
	pos := k8s.NodeStatus{
		Timeline: uint32(ls.Timeline), TimelineOK: ls.TimelineOK,
		LSNHi: ls.LSN.Hi, LSNLo: ls.LSN.Lo, LSNOK: ls.LSNOK,
	}
	now := time.Now()
	heartbeat := 2 * a.cfg.ReconcileInterval // < the 4x freshness window readers use
	if pos == a.lastPubPos && now.Sub(a.lastPubAt) < heartbeat {
		return
	}
	st := pos
	st.UpdatedAtUnix = now.Unix()
	if err := a.kube.PublishStatus(ctx, a.cfg.PodName, st); err != nil {
		a.log.Warn("publish status (gossip)", "err", err)
		return
	}
	a.lastPubPos, a.lastPubAt = pos, now
}

// gossipFresh reports whether a peer's gossiped status is recent enough to trust
// (a wedged/dead agent stops refreshing it). The window is generous relative to
// the reconcile cadence; cross-node clocks are assumed NTP-synced, with a small
// tolerance for a peer whose clock runs slightly ahead (negative age).
func (a *agent) gossipFresh(g k8s.NodeStatus) bool {
	if g.UpdatedAtUnix == 0 {
		return false
	}
	age := time.Now().Unix() - g.UpdatedAtUnix
	tol := int64(a.cfg.RenewDeadline.Seconds())
	return age >= -tol && time.Duration(age)*time.Second <= 4*a.cfg.ReconcileInterval
}

// advanceMarker records tl as the durable highwater (the #125 marker) when it is
// strictly above the current marker -- monotonic, so it never lowers the highwater
// and writes only on a real advance. A node booting below this later refuses to
// serve (unsafeToServe). No-op when the local timeline is unreadable.
func (a *agent) advanceMarker(ctx context.Context, tl pg.Timeline, tlOK bool, m reconcile.MarkerState) {
	if !shouldAdvanceMarker(tl, tlOK, m) {
		return
	}
	if err := a.kube.WriteMarker(ctx, a.cfg.MarkerName, a.cfg.PodName, uint32(tl)); err != nil {
		a.log.Warn("advance marker", "err", err)
	}
}

// shouldAdvanceMarker reports whether tl is strictly above the recorded highwater
// (so the marker only ever moves up, and a no-op tick makes no API write). An
// unreadable local timeline never advances it; a malformed marker is treated as
// "no constraint" so the primary can re-establish the highwater.
func shouldAdvanceMarker(tl pg.Timeline, tlOK bool, m reconcile.MarkerState) bool {
	if !tlOK {
		return false
	}
	if m.Present && !m.Malformed && tl <= m.Timeline {
		return false
	}
	return true
}

// assertPrimaryRouting is run by the holder/primary: it points the write Service
// at this pod and publishes the cluster's pg-role labels for the read-only Service.
func (a *agent) assertPrimaryRouting(ctx context.Context, obs reconcile.Observation) error {
	if _, err := a.kube.PatchWriteSelector(ctx, a.cfg.MasterService, a.cfg.PodName); err != nil {
		return err
	}
	return a.kube.ReconcilePodLabels(ctx, a.cfg.PodSelector, desiredRoleLabels(a.cfg.PodName, obs.Peers))
}

// desiredRoleLabels builds the pg-role map the primary publishes each tick (the
// #140 3-way classification): self is the primary; an in-recovery peer is a
// standby (joins the read-only Service); a reachable non-recovery peer is an
// orphan (a divergent second primary -- kept OUT of read traffic); an unreachable
// peer is omitted so ReconcilePodLabels leaves its label untouched rather than
// churning a node it cannot classify.
func desiredRoleLabels(self string, peers []reconcile.PeerState) map[string]string {
	m := map[string]string{self: "primary"}
	for _, p := range peers {
		switch {
		case !p.Reachable:
			// omit: leave the label untouched
		case p.Role == pg.RoleStandby:
			m[p.Name] = "standby"
		case p.Role == pg.RolePrimary:
			m[p.Name] = "orphan"
		}
	}
	return m
}

func (a *agent) selfConn() pg.ConnInfo {
	return pg.ConnInfo{Host: "127.0.0.1", Port: 5432, User: a.cfg.RepmgrUser, DB: a.cfg.RepmgrDB, Password: a.cfg.RepmgrPassword}
}

func (a *agent) peerConn(name string) pg.ConnInfo {
	return pg.ConnInfo{Host: a.fqdn(name), Port: 5432, User: a.cfg.RepmgrUser, DB: a.cfg.RepmgrDB, Password: a.cfg.RepmgrPassword}
}

func (a *agent) peerMechConn(name string) mechanism.Conn {
	return mechanism.Conn{Host: a.fqdn(name), Port: 5432, User: a.cfg.RepmgrUser, DB: a.cfg.RepmgrDB, ConnectTimeout: 10 * time.Second}
}

func (a *agent) fqdn(name string) string { return name + "." + a.cfg.HeadlessService }

func (a *agent) startMetrics(ctx context.Context) {
	srv := &http.Server{Addr: metricsAddr, Handler: a.metr.Handler(a.cfg.ReconcileInterval * 3)}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		a.log.Warn("metrics server stopped", "err", err)
	}
}

// selfHealthTracker arms a grace timer when a previously-running local primary
// goes unreachable, so a frozen/wedged postmaster -- which Start cannot recover and
// the reconcile-loop liveness probe (it checks the loop, not postgres) will not
// catch -- trips a self-health step-down. Data that has never come up this
// lifecycle is NOT armed, so a slow legitimate startup (e.g. crash-recovery WAL
// replay) is never mistaken for a wedged primary.
type selfHealthTracker struct {
	grace       time.Duration
	wasRunning  bool
	unhealthyAt time.Time
}

// stuck advances the tracker one tick and reports whether the local primary has
// been unreachable past the grace. shouldServe is true only for a holder with
// primary-state data (a node that ought to be a running primary).
func (h *selfHealthTracker) stuck(shouldServe, running bool, now time.Time) bool {
	switch {
	case !shouldServe:
		h.wasRunning, h.unhealthyAt = false, time.Time{}
		return false
	case running:
		h.wasRunning, h.unhealthyAt = true, time.Time{}
		return false
	case !h.wasRunning:
		return false // never came up this lifecycle: a startup, not a regression
	}
	if h.unhealthyAt.IsZero() {
		h.unhealthyAt = now
	}
	return now.Sub(h.unhealthyAt) >= h.grace
}

// baseName strips the trailing -<ordinal> from a StatefulSet pod name.
func baseName(pod string) string {
	if i := strings.LastIndex(pod, "-"); i > 0 {
		return pod[:i]
	}
	return pod
}

// nodeID maps a pod name to its repmgr node_id (ordinal + 1000), matching init-repmgr.sh.
func nodeID(pod string) int {
	if i := strings.LastIndex(pod, "-"); i >= 0 {
		if n, err := strconv.Atoi(pod[i+1:]); err == nil {
			return n + 1000
		}
	}
	return 0
}
