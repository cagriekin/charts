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
	base   string // StatefulSet name (pod name without the ordinal)

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
		base:   baseName(cfg.PodName),
	}, nil
}

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
			sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = a.sup.Demote(sctx, false) // graceful (fast) on planned shutdown
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
		}
	}
	o := reconcile.Observation{
		HoldLease:      a.dcs.IsLeader(),
		LeaderIdentity: a.dcs.Leader(),
		Local:          ls,
	}
	for i := 0; i < a.cfg.NodeCount; i++ {
		name := a.base + "-" + strconv.Itoa(i)
		if name == a.cfg.PodName {
			continue
		}
		ns := a.prober.Probe(ctx, a.peerConn(name))
		o.Peers = append(o.Peers, reconcile.PeerState{
			Name: name, Reachable: ns.Reachable, Role: ns.Role,
			Timeline: ns.Timeline, TimelineOK: ns.TimelineOK, LSN: ns.WriteLSN, LSNOK: ns.LSNOK,
		})
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
	return o
}

func (a *agent) act(ctx context.Context, dec reconcile.Decision, obs reconcile.Observation) error {
	switch dec.Action {
	case reconcile.Promote:
		// The node is already running as a standby (the reconcile guard); promote
		// acts on the running postmaster — do NOT Start it (that would error).
		if err := a.mech.Promote(ctx); err != nil {
			return err
		}
		a.metr.IncPromotion()
		_ = a.mech.RegisterPrimary(ctx)
		return a.assertPrimaryRouting(ctx, obs)

	case reconcile.StayPrimary:
		return a.assertPrimaryRouting(ctx, obs)

	case reconcile.Follow:
		return a.mech.Follow(ctx, nodeID(dec.Target))

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
		// Step down: demote and let the next leadership cycle re-evaluate (with
		// Parallel pod management + the marker guard, the legitimate node wins).
		a.metr.IncDemote()
		return a.sup.Demote(ctx, false)

	case reconcile.StartLocal:
		// Bring an initialized-but-stopped node up in its on-disk role. The decision
		// table only chooses StartLocal when this is safe (holder + highwater-ok for
		// primary-state data, or any standby-state data); a non-holder's primary-state
		// data is routed to RejoinForward/Wait instead, never started read-write here.
		if !obs.Local.Running && obs.Local.HasData {
			return a.sup.Start(ctx)
		}
		return nil

	case reconcile.Wait, reconcile.NoOp, reconcile.BootstrapInitdb:
		// Inert: never auto-start here. Starting a stopped node is an explicit
		// StartLocal decision so primary-state data is never brought up read-write
		// without passing the holdership/highwater guard (fresh-node initdb is done by
		// the entrypoint before the agent starts).
		return nil
	}
	return nil
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
