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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cagriekin/pg-ha-agent/internal/config"
	"github.com/cagriekin/pg-ha-agent/internal/control"
	"github.com/cagriekin/pg-ha-agent/internal/dcs"
	"github.com/cagriekin/pg-ha-agent/internal/k8s"
	"github.com/cagriekin/pg-ha-agent/internal/mechanism"
	"github.com/cagriekin/pg-ha-agent/internal/observe"
	"github.com/cagriekin/pg-ha-agent/internal/pg"
	"github.com/cagriekin/pg-ha-agent/internal/pgbackrest"
	"github.com/cagriekin/pg-ha-agent/internal/pgconf"
	"github.com/cagriekin/pg-ha-agent/internal/process"
	"github.com/cagriekin/pg-ha-agent/internal/reconcile"
)

const (
	metricsAddr = ":9200"
	repmgrConf  = "/etc/repmgr/repmgr.conf"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// One-shot subcommand: provision etcd RBAC and enable auth. The bundled etcd
	// image is distroless (no shell), so this runs in the repmgr image via the etcd
	// client rather than a shell + etcdctl. Used by the etcd chart's bootstrap Job.
	if len(os.Args) > 1 && os.Args[1] == "rbac-bootstrap" {
		if err := runRBACBootstrap(log); err != nil {
			log.Error("rbac-bootstrap", "err", err)
			os.Exit(1)
		}
		log.Info("rbac-bootstrap complete")
		return
	}

	cfg, err := config.FromEnv()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	logStartupConfig(log, cfg)

	a, err := newAgent(cfg, log)
	if err != nil {
		log.Error("init", "err", err)
		os.Exit(1)
	}
	a.run()
}

func logStartupConfig(log *slog.Logger, cfg *config.Config) {
	log.Info("starting pg-ha-agent",
		"podName", cfg.PodName,
		"namespace", cfg.Namespace,
		"leaseName", cfg.LeaseName,
		"dcsBackend", cfg.DCSBackend,
		"reconcileInterval", cfg.ReconcileInterval,
		"leaseDuration", cfg.LeaseDuration,
		"renewDeadline", cfg.RenewDeadline,
		"retryPeriod", cfg.RetryPeriod,
		"nodeCount", cfg.NodeCount,
		"headlessService", cfg.HeadlessService,
		"masterService", cfg.MasterService,
		"markerName", cfg.MarkerName,
		"cascadeReplication", cfg.CascadeReplication,
		"pgMajor", cfg.PGMajor,
	)
}

// runRBACBootstrap reads the bootstrap inputs from env and drives the etcd Auth API.
func runRBACBootstrap(log *slog.Logger) error {
	endpoints := splitNonEmpty(os.Getenv("ETCD_ENDPOINTS"))
	rootCN := strings.TrimSpace(os.Getenv("ETCD_RBAC_ROOT_CN"))
	healthCheckCN := strings.TrimSpace(os.Getenv("ETCD_RBAC_HEALTHCHECK_CN"))
	var tenants []dcs.RBACTenant
	if raw := strings.TrimSpace(os.Getenv("ETCD_RBAC_TENANTS")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &tenants); err != nil {
			return fmt.Errorf("ETCD_RBAC_TENANTS is not valid JSON: %w", err)
		}
	}
	log.Info("rbac-bootstrap", "endpoints", endpoints, "rootCN", rootCN, "healthCheckCN", healthCheckCN, "tenants", len(tenants))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return dcs.RBACBootstrap(ctx, endpoints,
		os.Getenv("ETCD_TLS_CERT"), os.Getenv("ETCD_TLS_KEY"), os.Getenv("ETCD_TLS_CA"),
		rootCN, healthCheckCN, tenants)
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

type agent struct {
	cfg  *config.Config
	log  *slog.Logger
	dcs  dcs.DCS
	kube *k8s.Client
	// The INTERFACE, not a concrete implementation: this is the seam that lets the mechanics
	// be swapped without touching policy (#287). Typed as *Repmgr it compiled, but it also
	// meant the agent was coupled to repmgr in the one place that is supposed not to be.
	mech   mechanism.Mechanism
	sup    *process.Supervisor
	prober *pg.Prober
	metr   *observe.Metrics
	health *selfHealthTracker
	// Major-versioned locations of the server binaries this image bundles, derived
	// from cfg.PGMajor at boot (#269) rather than hardcoded to one major.
	pgBindir         string
	pgControlDataBin string
	base             string    // StatefulSet name (pod name without the ordinal)
	bootAt           time.Time // agent start; the cold-boot grace fallback for PeersPending is measured from here
	// peersSeen latches which peers have been SQL-reachable at least once this
	// lifetime. Once all have, the cold-boot wait never applies again -- so a
	// steady-state failover is not delayed by a recent agent/pod restart.
	peersSeen map[string]bool
	// followUpstream is the leader this standby is currently registered/configured
	// to follow, so repmgr standby follow (which reconfigures and can restart the
	// server) runs only when the upstream actually changes, not every tick. Reset
	// on any non-Follow action.
	followUpstream string

	// standbyNoReceiverTicks counts consecutive ticks on which this node was a running standby
	// with no walreceiver. Feeds reconcile.Observation.StandbyStalled (#288 review): a diverged
	// standby that can never converge must be rejoined, but a routine reconnect -- or an upstream
	// that is briefly down -- looks identical for a tick or two, so the signal has to persist.
	standbyNoReceiverTicks int

	// lastTopologyGap is the comma-joined set of live peers not streaming, so topologyTick logs
	// one line per CHANGE instead of one per tick (#288). A rolling restart legitimately parks a
	// peer off-stream for a while, and warning every 5s about it would bury everything else.
	lastTopologyGap string

	// gossip publish state: skip re-patching the pod annotation when the position
	// is unchanged, refreshing only on change or a heartbeat (to keep it fresh).
	lastPubPos k8s.NodeStatus // position fields only (UpdatedAtUnix zeroed)
	lastPubAt  time.Time

	// opMu serializes all postmaster/mechanism mutations so the reconcile tick and
	// the OnLost fence callback never drive the supervisor concurrently (single
	// transition path; also avoids a concurrent-Stop deadlock).
	opMu sync.Mutex

	// servingRW is true while local Postgres is a read-write primary. The OnLost
	// fence only demotes when this is set: a read-only standby that loses/releases
	// leadership is not a writer, so shutting it down is needless churn -- and during
	// a repmgrd->agent rolling migration a standby-agent that holds the lease but
	// refuses to promote (equal-timeline with the still-repmgrd primary) would
	// otherwise demote/restart-loop and never go Ready, deadlocking the roll. Set
	// each tick from the observed role, and synchronously on promote (to close the
	// window before the next tick); fail-safe is to demote on uncertainty.
	servingRW atomic.Bool

	// Control API (#276), present only when it is enabled. intents carries node-local
	// operations from HTTP handlers to the reconcile goroutine (which owns the
	// postmaster); snap is the per-tick state the API serves, so a request costs no
	// probes; pgbr reads the pgBackRest repository and the restore outcome file.
	intents chan intentRequest
	snap    atomic.Pointer[control.Snapshot]
	pgbr    pgbackrest.Client

	// rehashedManagedUsers latches once the md5->scram managed-user re-hash has
	// succeeded on this node as primary (#199). It runs once per process on the first
	// RW path (Promote or boot-as-primary StayPrimary) -- not every tick -- and only
	// flips true on success, so a transient failure retries the next primary tick.
	rehashedManagedUsers atomic.Bool
}

// newDCS builds the leadership backend selected by DCS_BACKEND (validated to
// kubernetes|etcd in config.Load). Both satisfy the same dcs.DCS interface, so the
// reconcile loop is backend-agnostic.
func newDCS(cfg *config.Config) (dcs.DCS, error) {
	switch cfg.DCSBackend {
	case "etcd":
		return dcs.NewEtcdDCS(dcs.EtcdConfig{
			Endpoints: cfg.EtcdEndpoints,
			Prefix:    cfg.EtcdPrefix,
			// Whole-second lease TTL, rounded (not truncated) from LeaseDuration; the
			// config validator already enforces LeaseDuration >= 5s for etcd.
			TTLSeconds:  int(cfg.LeaseDuration.Round(time.Second).Seconds()),
			RetryPeriod: cfg.RetryPeriod,
			CertFile:    cfg.EtcdCertFile,
			KeyFile:     cfg.EtcdKeyFile,
			CAFile:      cfg.EtcdCAFile,
		})
	case "kubernetes":
		return dcs.NewK8sDCS(dcs.K8sConfig{
			Namespace:     cfg.Namespace,
			LeaseName:     cfg.LeaseName,
			LeaseDuration: cfg.LeaseDuration,
			RenewDeadline: cfg.RenewDeadline,
			RetryPeriod:   cfg.RetryPeriod,
		})
	default:
		return nil, fmt.Errorf("unsupported DCS backend %q (want kubernetes|etcd)", cfg.DCSBackend)
	}
}

func newAgent(cfg *config.Config, log *slog.Logger) (*agent, error) {
	// Server binaries live in the major-versioned bindir (#269). Verify it before
	// anything else: a PG_MAJOR that disagrees with what the image actually installed
	// would otherwise surface as an exec failure mid-reconcile, after the agent has
	// already taken the lease. Fail fast instead.
	pgBindir := cfg.PGBindir()
	postgresBin := pgBindir + "/postgres"
	if fi, err := os.Stat(postgresBin); err != nil {
		return nil, fmt.Errorf("PG_MAJOR=%s but %s is not usable (%v); this image does not bundle PostgreSQL %s",
			cfg.PGMajor, postgresBin, err, cfg.PGMajor)
	} else if fi.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("PG_MAJOR=%s but %s is not executable (mode %s)", cfg.PGMajor, postgresBin, fi.Mode())
	}
	d, err := newDCS(cfg)
	if err != nil {
		return nil, err
	}
	kube, err := k8s.New(cfg.Namespace)
	if err != nil {
		return nil, err
	}
	mech, err := newMechanism(cfg, repmgrConf, pgBindir, log)
	if err != nil {
		return nil, err
	}
	return &agent{
		cfg:    cfg,
		log:    log,
		dcs:    d,
		kube:   kube,
		mech:   mech,
		sup:    process.NewSupervisor(process.NewChildPostmaster(postgresBin, cfg.PGDATA)),
		prober: pg.NewProber(),
		metr:   observe.New(),
		// Self-health grace scales with the lease timing (the cloud preset widens
		// both), tolerating a transient stall before declaring the primary wedged.
		health:           &selfHealthTracker{grace: cfg.LeaseDuration},
		pgBindir:         pgBindir,
		pgControlDataBin: pgBindir + "/pg_controldata",
		base:             baseName(cfg.PodName),
		bootAt:           time.Now(),
		peersSeen:        map[string]bool{},
		// One slot: a second concurrent node-local operation waits on the channel rather
		// than queueing up behind an unbounded backlog of restarts.
		intents: make(chan intentRequest, 1),
		pgbr:    newPgbackrest(cfg.PGBackrestStanza, cfg.RestoreStatusPath()),
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

	// Control API (#276) on its own port. Enabling it and getting an agent WITHOUT it
	// would leave an operator believing a listener is protected when it is simply
	// absent, so a construction failure (unreadable/unusable TLS material) is fatal.
	if a.cfg.ControlEnabled {
		if err := a.startControl(ctx); err != nil {
			a.log.Error("control API", "err", err)
			os.Exit(1)
		}
	}

	// Leadership: OnLost demotes synchronously (the fence-ordering guarantee)
	// before the lock can be re-acquired by anyone.
	go a.dcs.Run(ctx, a.cfg.PodName, dcs.Callbacks{
		OnAcquired: func(context.Context) {
			a.metr.SetLeader(true)
			a.log.Info("acquired leadership")
		},
		OnLost: func() {
			a.metr.SetLeader(false)
			a.opMu.Lock()
			defer a.opMu.Unlock()
			// Only a read-write primary needs fencing on lease loss (so a peer cannot
			// promote into a second writer). A read-only standby is not a writer:
			// shutting it down is needless churn and, during a repmgrd->agent rolling
			// migration, would deadlock the roll (a standby-agent that holds the lease
			// but refuses to promote against the still-repmgrd primary). Skip it.
			if !a.servingRW.Load() {
				a.log.Info("lost leadership while not read-write; no fence needed")
				return
			}
			a.metr.IncFence()
			a.log.Warn("lost leadership; demoting (fence)")
			dctx, cancel := context.WithTimeout(context.Background(), a.cfg.RenewDeadline)
			defer cancel()
			if err := a.sup.Demote(dctx, true); err != nil {
				a.log.Error("fence demote failed", "err", err)
			}
			a.servingRW.Store(false)
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
			// Best-effort close of the DCS client (etcd holds a gRPC connection; the
			// K8s backend has nothing to close).
			if c, ok := a.dcs.(io.Closer); ok {
				_ = c.Close()
			}
			return
		case <-ticker.C:
			a.tick(ctx)
		case req := <-a.intents:
			// Control-API node-local operations run HERE, in the reconcile goroutine, so
			// they cannot interleave with a tick: the loop owns the postmaster, and a stop
			// issued from an HTTP goroutine would either be undone by the next tick or be
			// read as a fault. Handling it in the select (rather than draining once per
			// tick) also keeps a restart/reload responsive instead of waiting out an
			// interval.
			req.done <- a.runIntent(ctx, req)
		}
	}
}

// boot generates repmgr.conf (failover=manual) and starts Postgres if the data dir
// is already initialized (initdb/clone of a fresh node is handled by the entrypoint
// before the agent starts, or by the reconcile loop's clone path).
func (a *agent) boot(ctx context.Context) error {
	nid := mechanism.NodeIdentity{
		// 0 under native (#288 audit): only Repmgr.GenerateConfig reads this, and carrying a
		// repmgr node_id through a native path is how a future change accidentally starts
		// depending on one.
		NodeID:   a.repmgrNodeID(),
		NodeName: a.cfg.PodName,
		FQDN:     a.fqdn(a.cfg.PodName),
		DataDir:  a.cfg.PGDATA,
		PGBindir: a.pgBindir,
		ReplUser: a.cfg.RepmgrUser,
		ReplDB:   a.cfg.RepmgrDB,
	}
	// A base backup interrupted mid-flight leaves PG_VERSION behind, which makes HasData true
	// and takes the node off the BootstrapClone path forever (#288 review). Discard it first, so
	// everything below sees an honest picture of the directory.
	a.discardTornClone(ctx)
	// Streaming replication authenticates as the repmgr user via primary_conninfo,
	// which is deliberately passwordless (the password is not stored in repmgr.conf
	// -- the PR1 hardening). Without a credential the standby's walreceiver fails
	// with "no password supplied", so write a 0600 ~/.pgpass libpq picks up.
	//
	// FIRST, before GenerateConfig (#288 review). It writes to the postgres user's home, not
	// to PGDATA, so it is the one step here that always CAN succeed -- and on a fresh native
	// install PGDATA does not exist yet, so native's GenerateConfig legitimately fails
	// (writeManagedConf cannot create a temp file in a directory that is not there). With the
	// old ordering that failure returned early and the credential was never written, so every
	// native standby cloned successfully and then sat Running-but-NotReady forever with its
	// walreceiver failing `fe_sendauth: no password supplied`.
	if err := a.writePgpass(); err != nil {
		return err
	}
	// SKIPPED ENTIRELY on an empty data directory (#288 review), not merely best-effort.
	//
	// Writing here does real damage. process.WipeDataDir removes PGDATA's ENTRIES and leaves the
	// directory itself, so after a torn-clone discard the path exists and is empty -- and
	// native's writeManagedConf then SUCCEEDS, leaving one file behind (only the following
	// ensureInclude fails, for want of a postgresql.conf, and that error was swallowed). The next
	// BootstrapClone runs `pg_basebackup -D $PGDATA` with no flag permitting a populated target,
	// and pg_basebackup refuses: `directory "..." exists but is not empty` (verified against
	// PostgreSQL 18). Every later tick and restart repeats it, so the standby is wedged for good
	// -- the exact failure the clone marker exists to prevent, reached through its own recovery.
	//
	// Nothing is lost by waiting: both paths that create a data directory regenerate the config
	// once it exists (Native.Clone ends in Follow, and finishInitdbNative calls GenerateConfig).
	// NATIVE ONLY (#288 review). Repmgr.GenerateConfig writes /etc/repmgr/repmgr.conf and never
	// touches PGDATA, so skipping it there is pure loss: the pod would run on init-repmgr.sh's
	// version of that file for the rest of its life, which carries the password in plaintext
	// (the agent's omits it), sets failover=automatic, and -- functionally -- has NO
	// use_replication_slots, so every subsequent standby clone/follow/rejoin would run slotless
	// and re-expose the WAL-recycling gap #289 closed. Reachable in repmgr mode via an
	// interrupted ReclonePreserving: discardTornClone wipes PGDATA, HasData goes false, and the
	// skip would then persist.
	if a.cfg.Mechanism != config.MechanismNative || process.HasData(a.cfg.PGDATA) {
		if err := a.mech.GenerateConfig(ctx, nid, mechanism.ConfigOpts{Failover: "manual", UseReplicationSlots: true}); err != nil {
			return err
		}
	}
	if !process.HasData(a.cfg.PGDATA) {
		return nil
	}
	// Harden pg_hba: overwrite the image's initdb default -- which carries the legacy
	// 0.0.0.0/0 md5 catch-alls -- with a base trusting only loopback + the pod CIDR
	// (no external access), before any start (security review C1: external md5 is the
	// SUPERUSER-exposure risk; repmgrd mode keeps the legacy base). The agent is the
	// SINGLE author of pg_hba in agent mode: it writes the md5-first compat form
	// directly (md5 above each scram rule on the pod CIDR), so every node -- primary
	// and standby -- is byte-identical. This replaces the chart's former postStart
	// md5-fallback awk, which raced this write and left rejoined standbys SCRAM-only
	// (#199). Written every boot (idempotent); a clone/rejoin inherits the source's.
	if err := a.writePgHba(); err != nil {
		return err
	}
	// Only bring up data that is safe to start regardless of holdership: a
	// standby-state node comes up in recovery and follows its upstream. Primary-state
	// data is deferred to the reconcile loop, which starts it (StartLocal) only when
	// this node holds the lease and passes the highwater guard -- otherwise a fenced
	// ex-primary would come up read-write before the lease state is known and flap.
	cd, err := pg.ReadControlData(ctx, pg.OSExec{}, a.pgControlDataBin, a.cfg.PGDATA)
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
	// Track read-write role for the OnLost fence (a standby needs no fence). This is
	// the pure writer-state (NOT lease-gated: the lease flips to lost before OnLost
	// demotes, so gating on it could skip a real fence). Postgres stays read-write
	// until demoted, so a tick during the loss window still sees RW -> fence fires.
	// The Promote act also sets it synchronously, closing the promote->next-tick gap.
	a.servingRW.Store(obs.Local.Running && !obs.Local.InRecovery)
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
	// Converge md5 managed users to scram once this node is the serving primary (#199).
	// Run it OUTSIDE opMu and the fence budget: it is a local-socket SQL maintenance task
	// that touches no postmaster/mechanism state, so holding opMu (and competing with the
	// leadership-critical writes for the fence-budget window) would only starve its psql.
	if dec.Action == reconcile.Promote || dec.Action == reconcile.StayPrimary {
		a.rehashManagedUsersOnce(ctx)
	}
	// Publish what the control API serves. Gated on the API being enabled so a release
	// without it does exactly the work it did before -- notably no extra replay probe.
	if a.cfg.ControlEnabled {
		a.publishSnapshot(ctx, obs, dec)
	}
}

// localRestoredAt returns the FinishedAt of the last SUCCESSFUL restore on this volume, or ""
// (#288). Best-effort: an unreadable record means "no claim", which is the safe direction --
// it can only ever cause this node to rank lower, never to outrank a peer it should not.
func (a *agent) localRestoredAt() string {
	rec, err := a.pgbr.LastRestore()
	if err != nil {
		return ""
	}
	// Prefer restoredAt: write_status carries it across later FAILED attempts, so a mistyped
	// retry no longer erases a genuine restore's authority (#288 review). Fall back to
	// finishedAt for records written before that field existed, which is sound only when the
	// record itself is a clean restore.
	if rec.RestoredAt != "" {
		return rec.RestoredAt
	}
	if rec.Succeeded() {
		return rec.FinishedAt
	}
	return ""
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
		// #288: this volume's restore provenance, from the record restore.sh leaves on it.
		// Only a SUCCEEDED restore counts -- an interrupted one must never claim authority
		// over a peer's history -- and the record is dropped whenever the volume is re-cloned,
		// so a standby cloned from a restored primary correctly reports none of its own.
		RestoredAt: a.localRestoredAt(),
	}
	// When postgres is not running its timeline/role are unreadable via SQL. Fall
	// back to pg_controldata so the forward-rejoin and highwater guards still apply
	// to a stopped node (without this a fenced primary-state node has no timeline,
	// is started read-write, and immediately fences -- the flap).
	if !ls.Running && ls.HasData {
		if cd, err := pg.ReadControlData(ctx, pg.OSExec{}, a.pgControlDataBin, a.cfg.PGDATA); err != nil {
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
		// Process liveness (alive, not SQL-ready): a starting/recovering node is
		// alive but Running (SQL) is false; the decider waits instead of acting on
		// stale on-disk role (#181).
		LocalProcessAlive: a.sup.Running(),
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
		// Version-skew detection (Part H4), mirroring the marker check: a peer
		// gossiping a newer schema than this agent understands signals a mixed-version
		// rolling upgrade. v1 fields are stable so the position still ranks correctly;
		// this only flags it.
		for name, st := range gossip {
			if st.SchemaVersion > k8s.SchemaVersion {
				a.log.Warn("peer gossips a newer agent schema; ranking on known fields only", "peer", name, "peerSchema", st.SchemaVersion, "agentSchema", k8s.SchemaVersion)
				break
			}
		}
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
		// The restore identity is taken from gossip whether or not the peer is REACHABLE
		// (#288), unlike position above. It is durable provenance rather than a live reading,
		// and the case that matters is a reachable stale peer needing to see that another node
		// was restored -- ranking on position alone let it promote and discard that history.
		if g, ok := gossip[name]; ok && a.gossipFresh(g) {
			ps.RestoredAt = g.RestoredAt
		}
		o.Peers = append(o.Peers, ps)
	}
	m, err := a.kube.ReadMarker(ctx, a.cfg.MarkerName)
	if err != nil {
		a.log.Warn("read marker", "err", err)
	}
	if m.SchemaVersion > k8s.SchemaVersion {
		// A newer agent (mid rolling-upgrade) wrote the marker. v1 fields are stable,
		// so we read what we understand; this only flags the skew (Part H4).
		a.log.Warn("marker written by a newer agent schema; reading known fields only", "markerSchema", m.SchemaVersion, "agentSchema", k8s.SchemaVersion)
	}
	o.Marker = reconcile.MarkerState{
		Present:   m.Present,
		Malformed: m.Malformed || (m.Present && !m.TimelineOK),
		Timeline:  pg.Timeline(m.Timeline),
		Primary:   m.Primary,
	}
	// This pod's name, compared against Marker.Primary so an empty-data lease holder
	// can recognize it is not the recorded primary and release the lease (#186).
	o.LocalNode = a.cfg.PodName
	// #297: read repmgr.nodes ONLY for a promote candidate -- the holder, running and in
	// recovery, i.e. the one tick-state where the gate can fire. Every other node skips
	// the query, so this adds no steady-state cost, and RegistryRead stays false so the
	// gate is inert. The read is against the LOCAL node: repmgr.nodes replicates from the
	// primary, so this node's own copy is exactly what repmgr itself would consult when
	// asked to follow someone.
	a.readRegistryForGate(ctx, &o)
	a.observeStandbyStall(ctx, &o)
	// Cascading replication (#29): when enabled, a standby may follow another standby
	// (the pure cascadeFollowTarget decides; default off -> follow the primary).
	o.Cascade = a.cfg.CascadeReplication
	// The upstream this standby currently follows, so cascadeFollowTarget can stay
	// sticky and not oscillate when a closer peer flaps (#29 thrash fix).
	o.CurrentUpstream = a.followUpstream
	// Maintenance mode (Part H1): an operator-set annotation on the marker ConfigMap
	// suspends automatic promote/demote/fence/self-health (Decide -> NoOp) while the
	// agent keeps renewing the Lease and serving.
	o.Paused = m.Paused
	a.metr.SetPaused(m.Paused)
	// Controlled switchover (Part H2): the requested handoff target, if any.
	o.SwitchoverTarget = m.SwitchoverTarget
	// Self-health (stateful/time-based, so computed here, not in the pure Decide):
	// a holder whose primary-state postgres has been unreachable past the grace is
	// stuck (frozen/wedged), which drives a self-health failover. Suppressed while
	// paused: an operator who stops postgres during a maintenance window (passing
	// !shouldServe) must not arm the tracker -- otherwise it would latch "stuck" and
	// fire an unwanted ReleaseLease failover the moment pause is lifted. On resume a
	// still-stopped node is then treated as a startup, not a regression.
	shouldServe := o.HoldLease && !o.Paused && o.Local.HasData && !o.Local.InRecovery
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

// observeStandbyStall sets Observation.StandbyStalled when this node has been a running standby
// with no walreceiver for several consecutive ticks (#288 review).
//
// The failure it catches: a standby whose history diverged BEFORE the primary's timeline forked
// -- the shape a PITR restore leaves behind -- logs "new timeline N forked off current database
// system timeline M before current recovery point" and then waits forever for WAL that does not
// exist. Decide would otherwise return Follow on every tick indefinitely. Under repmgr this never
// surfaced because init-repmgr.sh compared timelines and re-cloned before the postmaster started;
// native has no such step, so the agent has to notice.
//
// Both halves of the eventual condition matter, and this is the half that needs state. A standby
// sitting on an older timeline is NORMAL right after a failover -- it follows onto the new one by
// streaming -- so the timeline gap alone must never escalate. An absent walreceiver is the
// distinguisher, and requiring it for stallTicks consecutive ticks keeps a reconnect blip, or a
// briefly-unreachable upstream, from triggering a re-clone.
func (a *agent) observeStandbyStall(ctx context.Context, o *reconcile.Observation) {
	if !(o.Local.Running && o.Local.InRecovery) {
		a.standbyNoReceiverTicks = 0
		return
	}
	_, streaming, err := a.prober.StreamingUpstream(ctx, a.selfConn())
	if err != nil {
		// Could not look: not evidence of a stall. Hold the counter rather than advancing it.
		return
	}
	if streaming {
		a.standbyNoReceiverTicks = 0
		o.StandbyStalled = false
		return
	}
	a.standbyNoReceiverTicks++
	o.StandbyStalled = a.standbyNoReceiverTicks >= standbyStallTicks
	if o.StandbyStalled {
		a.log.Warn("standby has had no walreceiver for several ticks; eligible for rejoin if a peer is on a newer timeline (#288)",
			"ticks", a.standbyNoReceiverTicks)
	}
}

// standbyStallTicks is how many consecutive receiver-less ticks make a standby "stalled". Six at
// the default 5s interval is ~30s -- comfortably longer than a walreceiver reconnect or a brief
// upstream restart, and far shorter than the startup probe's window.
const standbyStallTicks = 6

// readRegistryForGate populates the #297 promote-registration gate's inputs from repmgr.nodes.
// Only for a promote candidate (holder, running, in recovery); every other node leaves
// RegistryRead false, which keeps the gate inert.
//
// Native mode skips the read entirely (#288), the same treatment cleanupGhostNodes gets.
// The gate's premise is a repmgr METADATA requirement: an unregistered promoted primary is
// one no survivor can `repmgr standby follow`, because repmgr resolves an upstream by
// node_id out of repmgr.nodes. Native has no such dependency -- act() builds the follow
// target from the lease holder's identity plus the headless FQDN, and Native.Follow needs
// only upstream.Host, so a native primary is followable by DNS the moment it promotes
// (with the survivors' slots already created ahead of the routing switch, #289).
// Skipping it is also what makes "no repmgr.nodes query runs under native" literally true:
// a native cluster has no repmgr extension at all now, so this read could only ever fail,
// and it was logging a warning on every promote-candidate tick about a permanent condition.
func (a *agent) readRegistryForGate(ctx context.Context, o *reconcile.Observation) {
	if !(o.HoldLease && o.Local.Running && o.Local.InRecovery) {
		return
	}
	if a.cfg.Mechanism == config.MechanismNative {
		return
	}
	ids, rerr := a.prober.RegisteredNodeIDs(ctx, a.selfConn())
	if rerr != nil {
		// Unreadable: leave RegistryRead false so the gate cannot fire on a failed read
		// (that would refuse a legitimate promotion). Warn, do not block.
		a.log.Warn("read repmgr.nodes for the promote registration gate", "err", rerr)
		return
	}
	o.RegistryRead = true
	registered := make(map[int]bool, len(ids))
	for _, id := range ids {
		registered[id] = true
	}
	o.LocalRegistered = registered[nodeID(a.cfg.PodName)]
	for i := range o.Peers {
		o.Peers[i].Registered = registered[nodeID(o.Peers[i].Name)]
	}
}

func (a *agent) act(ctx context.Context, dec reconcile.Decision, obs reconcile.Observation) error {
	// Any action other than Follow changes (or ends) this node's standby identity, so
	// the next Follow must re-register + repoint.
	if dec.Action != reconcile.Follow {
		a.followUpstream = ""
	}
	// #289: only the primary and standby steady-state branches observe slots, so any other
	// action means this node has stopped publishing them and must RETRACT what it last
	// published. The slot alerts aggregate with max() across the release, so a pod still
	// exporting the figures it saw while primary would latch them on for the rest of its
	// process lifetime -- the alert would keep paging after the condition had moved to the
	// new primary or been resolved outright. Retracting is a no-op on a node that never
	// published any.
	switch dec.Action {
	case reconcile.Promote, reconcile.StayPrimary:
	case reconcile.Follow:
		a.lastTopologyGap = ""
		// Follow keeps the SLOT gauges: standbySlotsTick re-publishes them from a standby's own
		// point of view, so a leftover slot pinning WAL on this node stays visible. Topology has
		// no standby equivalent -- topologyTick reads the PRIMARY's connection list -- so it must
		// be retracted here, or a demoted primary would keep exporting the last view it had
		// (#288 review). That is precisely the max()-across-the-release latching ClearTopology
		// exists to prevent.
		a.metr.ClearTopology()
	default:
		a.metr.ClearSlots()
		a.metr.ClearTopology()
		// Reset the change-detection latch too (#288 review): it holds the gap string from this
		// node's previous life as primary, so a demote/re-promote cycle with the same peer still
		// off-stream would log nothing at all.
		a.lastTopologyGap = ""
	}
	switch dec.Action {
	case reconcile.Promote:
		// The node is already running as a standby (the reconcile guard); promote
		// acts on the running postmaster — do NOT Start it (that would error).
		if err := a.mech.Promote(ctx); err != nil {
			return err
		}
		a.metr.IncPromotion()
		// Now read-write: arm the OnLost fence immediately (before the next tick's
		// observation), so a lease loss right after promoting still fences.
		a.servingRW.Store(true)
		// After promotion the node is read-write. Bound ALL the post-promote
		// bookkeeping -- repmgr register, the WAL re-probe, and the marker + routing
		// apiserver writes -- under the fence budget, sharing one context so the total
		// opMu hold cannot exceed the soft-fence window and starve a lost-leadership
		// OnLost demote while this node is still RW (the register is repmgr->local PG,
		// bounded only by connect_timeout otherwise; a post-connect query hang would
		// hold opMu unbounded). H3 order: promote PG -> advance marker -> assert routing.
		wctx, cancel := context.WithTimeout(ctx, a.fenceBudget())
		defer cancel()
		_ = a.mech.RegisterPrimary(wctx)
		// #289: give every surviving standby a slot BEFORE routing switches to this node.
		// They will follow this new primary within a tick, and a Follow that arrives before
		// its slot exists streams slotless -- leaving this primary free to recycle WAL that
		// standby still needs. Sequencing it ahead of the routing switch closes that window.
		//
		// Under its OWN sub-budget, though, not the shared one: the slot pass is a slot list
		// plus up to NodeCount-1 creates plus a pod LIST plus drops, each psql allowed 10s,
		// against a total fence budget of 5s on chart defaults. Left on wctx a single slow
		// query would consume the whole budget and the promote would finish WITHOUT its
		// routing switch -- a write outage until the next tick, which is strictly worse than
		// the WAL gap this call exists to prevent. Half the budget keeps the cutover funded.
		func() {
			sctx, scancel := context.WithTimeout(wctx, a.fenceBudget()/2)
			defer scancel()
			a.slotsTick(sctx)
		}()
		if tl, _, ok, _ := a.prober.PrimaryWALPosition(wctx, a.selfConn()); ok {
			a.advanceMarker(wctx, tl, true, obs.Marker)
		}
		promoteRoutingErr := a.assertPrimaryRouting(wctx, obs)
		// LAST, for the same reason as in StayPrimary (#288 review): purely observational, so it
		// must never compete with the marker write or the routing switch. Sharing slotsTick's
		// fenceBudget()/2 sub-context left it routinely under the probe's own 10s psql timeout,
		// which logged a spurious warning on every promotion and put a second apiserver pod LIST
		// on the promote critical path.
		a.topologyTick(ctx)
		return promoteRoutingErr

	case reconcile.StayPrimary:
		// Register this primary in repmgr.nodes. In agent mode there is no repmgrd
		// sidecar to do it, and a fresh primary comes up via StartLocal (never
		// Promote), so without this repmgr.nodes stays empty and a standby's
		// init-clone (which waits for a registered primary) hangs forever. The
		// command is idempotent (--force) and self-healing; the standby clone needs
		// only that the primary is registered, so a best-effort per-tick reconcile is
		// fine (it succeeds within a tick or two of the primary opening).
		// The node is read-write here, so bound the register + marker + routing under
		// one fence-budget context (see Promote): a hung register/apiserver write must
		// not hold opMu past the soft-fence window and starve a lost-leadership fence.
		wctx, cancel := context.WithTimeout(ctx, a.fenceBudget())
		defer cancel()
		if err := a.mech.RegisterPrimary(wctx); err != nil {
			a.log.Warn("register primary in repmgr.nodes", "err", err)
		}
		// Drop repmgr.nodes records for pods a scale-down removed (#139). Bounded by
		// the same fence-budget context as the register above: a hung psql/unregister
		// must not hold opMu past the soft-fence window. Best-effort -- it resumes next
		// tick if cut short, and only ever targets ordinals above the live range.
		a.cleanupGhostNodes(wctx)
		// The slot-side twin of the ghost cleanup above (#289): publish the slot gauges,
		// then (native only) create slots for expected standbys and reclaim orphans a
		// scale-down or a repmgr->native migration left pinning WAL. Bounded by the same
		// fence-budget context. Only the mutation half is mechanism-gated -- the gauges the
		// shipped alerts read must be truthful under repmgr too (see slotsTick).
		a.slotsTick(wctx)
		// Keep the highwater marker at this primary's timeline (monotonic; written
		// only when it advances, so steady-state ticks make no API write).
		a.advanceMarker(wctx, obs.Local.Timeline, obs.Local.TimelineOK, obs.Marker)
		routingErr := a.assertPrimaryRouting(wctx, obs)
		// #288: publish the topology from pg_stat_replication. Observe-only -- see topologyTick
		// for why nothing in Decide may consume it -- and therefore LAST, after the two
		// leadership-critical writes (#288 review). On the shared fence budget (5s on chart
		// defaults, already contended by slotsTick) an extra query plus a pod LIST in FRONT of
		// them could make a slow tick skip the marker write and the Service routing assertion,
		// which is a write outage. Nothing here may cost that.
		a.topologyTick(ctx)
		return routingErr

	case reconcile.Follow:
		// #289 review: reclaim slots this node minted while it was the primary, and publish
		// the slot gauges from a standby's point of view.
		//
		// FIRST in the branch, ahead of the followUpstream latch below: that latch returns
		// early on every tick for a healthy standby that is already pointed at the right
		// upstream, which is exactly the steady state a demoted ex-primary settles into.
		// Anything placed after it would run once and never again -- so the leftover slots
		// would keep reserving WAL on this node's own volume for the rest of its life.
		a.standbySlotsTick(ctx)
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
		// Invariant 9: never follow an upstream from a different cluster.
		if err := a.assertSameCluster(ctx, dec.Target); err != nil {
			return err
		}
		// 0 under native (#288 audit): repmgr addresses a follow target by node_id out of
		// repmgr.nodes; native writes upstream.Host into primary_conninfo and has no use for
		// an id. RegisterStandby is already a no-op under native.
		up := a.repmgrPeerNodeID(dec.Target)
		regErr := a.mech.RegisterStandby(ctx, up)
		if regErr != nil {
			a.log.Warn("register standby in repmgr.nodes", "err", regErr)
		}
		// #182: a healthy standby is already streaming from the lease holder before
		// this first Follow runs -- a repmgrd->agent migration keeps primary_conninfo
		// across the roll, and a post-failover rejoin attaches before Follow. repmgr
		// standby follow then exits non-zero (slot already active), which, left
		// unlatched, repeats every tick. Skip the command when already streaming from
		// the target and latch; repointing to a NEW upstream (sender_host differs, or
		// no walreceiver yet) still falls through to Follow.
		//
		// Gate the skip on a successful register: the follow it replaces implicitly
		// requires the repmgr.nodes record (repmgr fails "unable to retrieve node
		// record" -- NOT the benign exit -- and retries when it is missing), but the
		// probe bypasses that check. A freshly-cloned standby streams before it is ever
		// registered, so latching here on a failed register would strand it without a
		// record and break a later promote. On a failed register, fall through to
		// follow (which re-establishes the record, or errors so the next tick retries).
		if regErr == nil && a.streamingFromTarget(ctx, dec.Target) {
			a.latchFollow(dec.Target)
			return nil
		}
		// Carry both the address and the node id: repmgr uses the id, a native mechanism
		// uses the host to write primary_conninfo (#287).
		upConn := a.peerMechConn(dec.Target)
		upConn.NodeID = up
		if err := a.mech.Follow(ctx, upConn); err != nil {
			// #297: this node has no row in its own metadata copy -- a freshly-cloned standby
			// whose upstream changed before it registered. It can never obtain the row (that
			// needs replication, which is what it is trying to establish), so it would retry
			// forever, Running but never Ready, silently not replicating. Re-clone from the
			// current primary: that replaces both its data AND its metadata copy, and a
			// standby in this state has no writes of its own to lose.
			//
			// Scoped narrowly on purpose. It fires ONLY on the local-record error, never on a
			// missing UPSTREAM record -- that one is the ordinary post-failover case where the
			// target simply has not promoted yet, and escalating there demotes and re-clones a
			// healthy standby (a mistake made and reverted on #286).
			if errors.Is(err, mechanism.ErrLocalRecordMissing) {
				a.log.Warn("this node is absent from its own repmgr.nodes copy; re-cloning from the current primary",
					"target", dec.Target, "err", err)
				return a.rejoinOnto(ctx, dec.Target)
			}
			return err
		}
		// repmgr standby follow applies the repoint itself (this reload is a harmless
		// no-op confirming it). The native mechanism only writes files (managed conf +
		// standby.signal) and relies on the caller to apply them -- primary_conninfo is
		// reloadable in modern PostgreSQL (this node is already InRecovery, per Decide's
		// precondition for the Follow action, so a reload -- not a full restart -- is
		// sufficient to make the walreceiver reconnect to the new upstream). Skipping
		// this would leave native mode's Follow silently inert: the file changes, but
		// nothing tells the running postmaster to pick it up (#287).
		if err := a.sup.Reload(ctx); err != nil {
			return fmt.Errorf("reload after follow: %w", err)
		}
		a.latchFollow(dec.Target)
		return nil

	case reconcile.DemoteFence:
		a.metr.IncDemote()
		return a.sup.Demote(ctx, true)

	case reconcile.RejoinForward:
		return a.rejoinOnto(ctx, dec.Target)

	case reconcile.BootstrapClone:
		// Marker around the clone so an interrupted one is discarded on the next boot rather
		// than stranding the pod with a torn PGDATA (#288 review).
		a.beginClone()
		if err := a.mech.Clone(ctx, a.peerMechConn(dec.Target)); err != nil {
			// Discard here, not only at the next boot (#288 review). If pg_basebackup's child is
			// killed while the agent survives, PGDATA is left with PG_VERSION present, so Decide
			// takes the has-data branch and fails every tick -- and discardTornClone only runs in
			// boot(), so recovery would wait for the startup probe to kill the container (600s on
			// defaults). Doing it now re-arms BootstrapClone on the very next tick.
			a.discardTornClone(ctx)
			return err
		}
		a.endClone()
		a.dropRestoreRecord("the data directory was cloned from " + dec.Target)
		return a.sup.Start(ctx)

	case reconcile.ReleaseLease:
		// Step down: release the Lease so a peer can take over (stale-winner handoff
		// at cold boot, or self-health failover). Only stop postgres if we were
		// serving read-write; a standby is already read-only, so releasing the Lease
		// is enough and we avoid churning its postmaster.
		a.dcs.Release()
		switch {
		case obs.Local.Running && !obs.Local.InRecovery:
			a.metr.IncDemote()
			return a.sup.Demote(ctx, false)
		case obs.LocalStuck && !obs.Local.InRecovery:
			// Self-health failover: a primary-state postmaster that is wedged/frozen
			// (SQL unreachable, so Running is false and the OnLost fence -- gated on the
			// already-cleared servingRW -- would NOT demote it). The agent owns this
			// PID-1 child, so force-stop it (SIGQUIT->SIGKILL on timeout) BEFORE the
			// released lease lets a standby promote: a frozen primary that later
			// unfreezes would be a second writer. Harmless if it is already down.
			a.metr.IncDemote()
			rctx, cancel := context.WithTimeout(ctx, a.cfg.RenewDeadline)
			defer cancel()
			return a.sup.Stop(rctx, process.Immediate)
		}
		return nil

	case reconcile.Switchover:
		// Controlled handoff (Part H2): the decision guard already confirmed the
		// target is a caught-up, same-timeline standby. Clear the request FIRST so a
		// later unrelated failover cannot re-trigger a handoff to the same pod; only
		// on a successful clear do we step down (release the Lease + graceful/fast
		// demote, which flushes WAL to the connected, caught-up target). The target,
		// being caught up, then wins the freed Lease and promotes. If the clear fails
		// we do NOT step down -- the request persists, so the next tick retries.
		if err := a.kube.ClearSwitchoverTarget(ctx, a.cfg.MarkerName); err != nil {
			return err
		}
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
			primaryState := !obs.Local.InRecovery
			if primaryState {
				// primary-state: clear any stray standby.signal so it opens read-write
				// (crash recovery on the same timeline -- a resume, not a promotion).
				if err := process.ClearRecoverySignal(a.cfg.PGDATA); err != nil {
					return err
				}
			}
			if err := a.sup.Start(ctx); err != nil {
				return err
			}
			if primaryState {
				// Resuming as a read-write primary (the lease holder, per the decision
				// guard). Arm the OnLost fence synchronously -- mirror the Promote path
				// -- so a lease loss during the resume window (before the next tick
				// observes the postmaster SQL-reachable) still fences. Postgres may still
				// be in crash recovery here; pre-arming only ever makes OnLost demote a
				// node that is becoming a writer (the fail-safe direction).
				a.servingRW.Store(true)
			}
			return nil
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
			if err := a.sup.Start(ctx); err != nil {
				return err
			}
			a.metr.IncRecoveryStart()
			return nil
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

	case reconcile.BootstrapInitdb:
		// Under repmgr this stays inert, exactly as before: the entrypoint initdbs inline on
		// any empty data directory before the agent ever runs, so by the time the loop sees
		// the node it already has data.
		//
		// Under native the entrypoint deliberately does NOT (#288). It cannot: the init
		// container no longer clones, so every pod would arrive with an empty PGDATA and
		// create its own cluster with its own system_identifier -- and assertSameCluster
		// (invariant 9) then refuses to rejoin any of them, leaving pods Running, never
		// Ready, holding bogus databases. Whether to initdb is a CLUSTER-WIDE decision, and
		// the lease is the only thing that can make it exactly once.
		//
		// Decide already guarantees that: BootstrapInitdb is returned only for the lease
		// holder with empty data, no reachable primary and no highwater marker -- i.e. a
		// genuine fresh install. Non-holders get Wait, then BootstrapClone once this node is
		// open, and clone with pg_basebackup through their own pre-created slot (#289).
		if a.cfg.Mechanism != config.MechanismNative {
			return nil
		}
		if obs.Local.HasData || obs.Local.Running {
			// Belt and braces: never initdb over anything. The shell function refuses too.
			return nil
		}
		return a.bootstrapInitdbNative(ctx)

	case reconcile.Wait, reconcile.NoOp:
		// Inert: never auto-start here. Starting a stopped node is an explicit
		// StartLocal decision so primary-state data is never brought up read-write
		// without passing the holdership/highwater guard.
		return nil
	}
	return nil
}

// bootstrapInitdbNative creates the cluster for a fresh native install (#288), as the lease
// holder, then starts it.
//
// The mechanics stay in the shell (`entrypoint.sh initdb` -> bootstrap_initdb) rather than
// being ported to Go here. There is exactly ONE bootstrap implementation while both
// mechanisms are live, so the two cannot drift -- and drift would be invisible until a suite
// that runs on only one mechanism failed. #290 deletes the shell wholesale; porting it then is
// mechanical. What moves into the agent now is the AUTHORITY (who initdbs, and when), which is
// the part the lease has to own.
//
// Two things must be redone afterwards, because boot() ran against an empty data directory:
//
//   - GenerateConfig: native's ensureInclude reads PGDATA/postgresql.conf, which did not exist
//     yet, so boot()'s call failed (logged, non-fatal). Without a second call the managed
//     fragment and its include line are simply absent.
//   - writePgHba: boot() returns early on !HasData, before the C1 hardening that replaces
//     initdb's legacy 0.0.0.0/0 md5 catch-alls. Skipping it would leave a fresh native cluster
//     on the legacy pg_hba until some later pod restart -- a security regression, not just an
//     inconsistency.
func (a *agent) bootstrapInitdbNative(ctx context.Context) error {
	a.log.Info("fresh install: initdb as the lease holder (#288)", "pgdata", a.cfg.PGDATA)
	// Not bounded by the fence budget: initdb plus role/database creation is legitimately
	// slower than a failover window, and this node is not serving anything yet, so there is
	// no read-write exposure for a soft fence to race.
	// Bounded, just not by the FENCE budget (#288 review). initdb plus role/database creation is
	// legitimately slower than a failover window, but "not the fence budget" must not mean "no
	// budget": bootstrap_initdb runs `pg_ctl -w start` and `pg_ctl -w stop`, and if either wait
	// never returns -- or a failed stop leaves the postmaster holding the stdout pipe, which
	// Cmd.Output waits on -- act() would hold opMu forever, the reconcile loop would stop
	// beating, and dcs.OnLost (which also takes opMu) could never fence.
	ictx, icancel := context.WithTimeout(ctx, initdbBudget)
	defer icancel()
	if out, err := a.prober.Exec.Run(ictx, nil, entrypointPath, "initdb"); err != nil {
		// This path CAN leave a data directory behind (#288 review), so it needs the same
		// cleanup as everything below. bootstrap_initdb runs initdb first and only then
		// `pg_ctl -w start` and the role/database creation, so a failure at the start -- or a
		// budget expiry, or an OOM kill -- leaves PGDATA fully initialized with NO repmgr role
		// or database. bootstrap_initdb then no-ops on it forever, and the node comes up as a
		// primary the agent can never authenticate against and no standby can clone from.
		werr := a.discardFreshDataDir()
		if werr != nil {
			return fmt.Errorf("bootstrap initdb: %w: %s (and could not discard the partial data directory, delete the PVC to recover: %v)", err, strings.TrimSpace(out), werr)
		}
		return fmt.Errorf("bootstrap initdb: %w: %s", err, strings.TrimSpace(out))
	}
	nid := mechanism.NodeIdentity{
		// 0 under native (#288 audit): only Repmgr.GenerateConfig reads this, and carrying a
		// repmgr node_id through a native path is how a future change accidentally starts
		// depending on one.
		NodeID:   a.repmgrNodeID(),
		NodeName: a.cfg.PodName,
		FQDN:     a.fqdn(a.cfg.PodName),
		DataDir:  a.cfg.PGDATA,
		PGBindir: a.pgBindir,
		ReplUser: a.cfg.RepmgrUser,
		ReplDB:   a.cfg.RepmgrDB,
	}
	// From here on the data directory EXISTS, so every failure path -- and a lease flip -- must
	// go through the same cleanup (#288 review). Returning early instead would leave PGDATA
	// initialized with no marker written: HasData true forever, never eligible for
	// BootstrapClone again, and rejected by assertSameCluster on every rejoin. That is the exact
	// state the lease-loss branch below was added to prevent, reached by a different door.
	if err := a.finishInitdbNative(ctx, nid); err != nil {
		if werr := a.discardFreshDataDir(); werr != nil {
			return fmt.Errorf("%w (and could not discard the fresh data directory, delete the PVC to recover: %v)", err, werr)
		}
		return err
	}
	return nil
}

// finishInitdbNative is everything that must happen after the cluster exists and before it
// serves. Split out so a single cleanup path covers every failure (#288 review).
func (a *agent) finishInitdbNative(ctx context.Context, nid mechanism.NodeIdentity) error {
	// FIRST, before GenerateConfig (#288 review). Both append to postgresql.conf, and the LAST
	// include wins in PostgreSQL. Native.ensureInclude documents that the agent's fragment is
	// "appended LAST so the agent's replication settings win over anything earlier" -- and on
	// every non-fresh boot that holds, because setup-config writes include_dir before the agent
	// runs. Doing it the other way round here would invert precedence on native fresh installs
	// only: a postgresql.configuration carrying wal_log_hints or hot_standby would silently
	// override the agent's authoritative value, the inverted file would be cloned verbatim to
	// every standby, and setup-config would never repair it (its grep finds the line present).
	if err := a.ensureConfdInclude(); err != nil {
		return err
	}
	if err := a.mech.GenerateConfig(ctx, nid, mechanism.ConfigOpts{Failover: "manual", UseReplicationSlots: true}); err != nil {
		return fmt.Errorf("bootstrap initdb: generate config: %w", err)
	}
	if err := a.writePgHba(); err != nil {
		return fmt.Errorf("bootstrap initdb: write pg_hba: %w", err)
	}
	// Holdership re-check before going read-write (#288 review). The exec above is
	// deliberately not fence-bounded -- initdb plus role/database creation is legitimately
	// slower than a failover window -- but that means the lease can flip during it, and OnLost
	// blocks on opMu until act() returns. Starting anyway would give the cluster two nodes that
	// each initdb'd their own data with different system_identifiers, which assertSameCluster
	// then refuses to rejoin: exactly the outcome this whole branch exists to prevent.
	if !a.dcs.IsLeader() {
		// Refusing to start is necessary but NOT sufficient (#288 review). The data directory is
		// now fully initialized, and no highwater marker exists yet -- that is written on the
		// next StayPrimary tick, which will never come. So this pod would sit with
		// HasData == true forever: never eligible for BootstrapClone again, and rejected by
		// assertSameCluster on every Follow/RejoinForward because the new holder initdb'd its
		// own cluster with a different system_identifier. A manual PVC delete would be the only
		// way out.
		//
		// This branch created the directory milliseconds ago and it has never served a client,
		// so removing it is safe and makes the pod eligible to clone from the winner instead.
		// WipeDataDir refuses a live postmaster.pid, and nothing was started.
		a.log.Warn("lost the lease during initdb; discarding the fresh data directory so this node can clone from the new holder (#288)")
		return a.discardFreshDataDir()
	}
	if err := a.sup.Start(ctx); err != nil {
		return fmt.Errorf("bootstrap initdb: start: %w", err)
	}
	// Arm the lost-leadership fence before the first tick observes this node, mirroring
	// StartLocal's primary-state arming: this node is now a writer, and pre-arming only ever
	// errs toward demoting a node that is becoming one.
	a.servingRW.Store(true)
	// Deliberately no metric bump here. IncRecoveryStart counts read-only WAL-replay starts at
	// cold boot, which this is the opposite of, and there is no existing counter for "created
	// the cluster" -- a once-per-cluster-lifetime event that a counter would describe poorly.
	// The log line above is the record.
	return nil
}

// ensureConfdInclude writes the operator's conf.d include on a fresh native install (#288).
//
// Nothing else does: the setup-config init container is guarded on postgresql.conf already
// existing and runs before PGDATA does, and the only other writer is the postStart hook, whose
// fixed pg_isready loop (~90s on the agent-mode branch) now has to cover lease acquisition AND a
// reconcile tick AND initdb.
// If it overruns, the include is silently absent and every postgresql.configuration / TLS /
// pgbackrest setting is missing -- and standbys clone that same config.
//
// Presence of the directory is a faithful proxy for "some conf.d feature is enabled": the chart
// mounts it only then. setup-config remains the authority on every later boot, including
// removal, and EnsureConfdInclude converges either way.
func (a *agent) ensureConfdInclude() error {
	entries, err := os.ReadDir(confdDir)
	if os.IsNotExist(err) {
		return nil // the chart mounts this only when a conf.d feature is enabled
	}
	if err != nil {
		// NOT the same as "the feature is off" (#288 review). A mount or permission problem here
		// would otherwise bring the cluster up with no include_dir at all -- every
		// postgresql.configuration / TLS / audit / archive_mode setting silently absent, cloned
		// to every standby -- and the later repair appends include_dir AFTER the agent's own
		// include, inverting the precedence this function is ordered to protect.
		return fmt.Errorf("bootstrap initdb: read %s: %w", confdDir, err)
	}
	if len(entries) == 0 {
		return nil
	}
	if err := pgconf.EnsureConfdInclude(filepath.Join(a.cfg.PGDATA, "postgresql.conf"), confdDir, true); err != nil {
		return fmt.Errorf("bootstrap initdb: ensure the conf.d include: %w", err)
	}
	a.log.Info("wrote the conf.d include for a fresh native install", "dir", confdDir)
	return nil
}

// discardFreshDataDir removes a data directory this branch just created, so the pod becomes
// eligible to clone instead of being stranded (#288 review).
//
// Safe because the cluster was created milliseconds ago and has never served a client, and
// WipeDataDir refuses a live postmaster.pid. An uninitialized directory is not an error: there
// is nothing to discard and nothing to strand, and WipeDataDir would refuse it anyway (it
// requires PG_VERSION), so treating that as a failure would make the harmless case loud.
func (a *agent) discardFreshDataDir() error {
	if !process.HasData(a.cfg.PGDATA) {
		return nil
	}
	// Reap any postmaster the bootstrap left running BEFORE trying to wipe (#288 review).
	// bootstrap_initdb starts a socket-only postmaster with `pg_ctl -w start`, which daemonizes
	// a process the agent does not supervise. If the budget expires (or the exec is cut short)
	// between that start and its matching stop, WaitDelay kills entrypoint.sh but NOT the
	// detached postmaster -- and then WipeDataDir refuses on the live postmaster.pid, returning
	// "delete the PVC to recover" while every later sup.Start fails on "postmaster.pid already
	// exists". The pod wedges Running/NotReady for good: the startupProbe's `pg_isready` over
	// the unix socket is SATISFIED by the orphan, so the startup grace stops protecting, while
	// selfConn() dials 127.0.0.1 and can never reach a socket-only postmaster.
	//
	// Best-effort and on its OWN context: the usual trigger for this path is the parent budget
	// having already expired, so reusing it would make the stop a guaranteed no-op.
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := a.prober.Exec.Run(sctx, nil, filepath.Join(a.pgBindir, "pg_ctl"),
		"-D", a.cfg.PGDATA, "-m", "immediate", "-w", "stop"); err != nil {
		// Nothing to stop is the common case (initdb failed before the start), and it is not an
		// error worth failing the discard over -- WipeDataDir's own interlock is the authority.
		a.log.Info("no bootstrap postmaster to stop before discarding the data directory",
			"reason", strings.TrimSpace(out), "err", err)
	}
	if err := process.WipeDataDir(a.cfg.PGDATA); err != nil {
		return fmt.Errorf("discard the fresh data directory (delete the PVC to recover): %w", err)
	}
	return nil
}

// cloneMarkerPath is a sentinel recording that a base backup into PGDATA was started and has
// not finished (#288 review).
//
// It lives in PGDATA's PARENT, not in PGDATA: pg_basebackup requires an empty target, so a
// marker inside would be destroyed by the very operation it tracks.
//
// Why it is needed. An interrupted clone -- startup-probe kill, eviction, OOM, node reboot --
// leaves PG_VERSION behind, so process.HasData reports true. reconcile.Decide only returns
// BootstrapClone for `!HasData && !Running`, so the pod instead falls into the has-data branch
// and tries to rejoin or rewind a torn directory, failing every tick forever; the only recovery
// was deleting the PVC. Under repmgr this could not happen, because init-repmgr.sh did
// `rm -rf "${PGDATA:?}"/*` before each of its five clone attempts. Moving the clone into the
// agent means the agent has to own that discard too.
func (a *agent) cloneMarkerPath() string {
	return filepath.Join(filepath.Dir(filepath.Clean(a.cfg.PGDATA)), ".pg-ha-clone-in-progress")
}

// beginClone records that a base backup is starting. Best-effort: failing to write the marker
// must not block the clone, it only costs the automatic discard if that clone is interrupted.
func (a *agent) beginClone() {
	if err := os.WriteFile(a.cloneMarkerPath(), []byte(a.cfg.PodName+"\n"), 0o600); err != nil {
		a.log.Warn("write the clone-in-progress marker; an interrupted clone will need a manual PVC delete", "err", err)
	}
}

// endClone clears the marker after a clone completes.
func (a *agent) endClone() {
	if err := os.Remove(a.cloneMarkerPath()); err != nil && !os.IsNotExist(err) {
		a.log.Warn("remove the clone-in-progress marker", "err", err)
	}
}

// discardTornClone wipes a data directory left behind by an INTERRUPTED base backup, so the
// reconcile loop sees an empty PGDATA and re-arms BootstrapClone (#288 review).
//
// The marker alone is not sufficient evidence, and treating it as such was destructive
// (#288 review, round 4). Native.Clone ends by calling Follow, which makes a psql round-trip to
// the upstream, and ReclonePreserving can fail at its rename or while removing its backup copy --
// so a Clone error very often means "the base backup COMPLETED and something after it failed".
// Wiping then would destroy a finished multi-hour backup over a transient blip, or throw away the
// only copy of a diverged node's un-replicated data.
//
// pg_controldata is the discriminator: it reads pg_control, which pg_basebackup writes LAST
// precisely so an interrupted copy is detectable. If it parses, the directory is a complete
// cluster and must be kept whatever else failed; if it does not, the copy is torn and unusable.
func (a *agent) discardTornClone(ctx context.Context) {
	if _, err := os.Stat(a.cloneMarkerPath()); err != nil {
		return // no clone was in flight
	}
	if !process.HasData(a.cfg.PGDATA) {
		// Never got as far as PG_VERSION; nothing to discard.
		a.endClone()
		return
	}
	if _, err := pg.ReadControlData(ctx, pg.OSExec{}, a.pgControlDataBin, a.cfg.PGDATA); err == nil {
		// Complete cluster: the base backup finished and only a later step failed. Keep it and
		// clear the marker so a later boot does not reconsider.
		a.log.Info("the base backup completed; keeping the data directory (a later step failed, #288)")
		a.endClone()
		return
	} else if !process.ControlFileMissing(a.cfg.PGDATA) {
		// pg_controldata failed for a reason OTHER than an absent control file -- the tool could
		// not run at all (fork/OOM), or the PVC carries a different PG major after an image bump
		// (#288 review). "Could not look" is not evidence of tornness, and a stale marker is
		// reachable without any interrupted clone (endClone only WARNS when its remove fails),
		// so wiping on this would destroy a healthy standby. Leave it and let the ordinary
		// has-data paths report the real problem.
		a.log.Warn("could not read pg_controldata; not discarding the data directory on that basis", "err", err)
		return
	}
	a.log.Warn("discarding a data directory left by an interrupted base backup so it can be re-cloned (#288)",
		"pgdata", a.cfg.PGDATA)
	if err := process.WipeDataDir(a.cfg.PGDATA); err != nil {
		a.log.Error("could not discard the torn clone; delete the PVC to recover", "err", err)
		return
	}
	a.endClone()
}

// initdbBudget bounds the one-off cluster creation. Generous rather than tight -- it covers
// initdb, a postmaster start/stop cycle and six psql calls on a possibly-contended node -- but
// finite, so a wedged pg_ctl cannot deadlock the reconcile loop (#288 review).
const initdbBudget = 5 * time.Minute

// confdDir is where the chart mounts the operator's postgresql.configuration / TLS / audit /
// pgbackrest fragments. Mounted only when one of those features is on, which is what makes its
// presence a usable signal (#288).
const confdDir = "/etc/postgresql/conf.d"

// entrypointPath is the image's entrypoint, invoked with an explicit mode. Fixed rather than
// derived: the agent runs inside that image by construction (#288).
const entrypointPath = "/usr/local/bin/entrypoint.sh"

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

// rehashManagedUsersOnce migrates the managed users (POSTGRES_USER, REPMGR_USER) from
// md5 to scram-sha-256 once this node is serving read-write, when migrateLegacyMd5Users
// is enabled. It replaces the chart's former postStart re-hash, which never ran on an
// in-process failover promotion (no container restart), so a promoted primary's md5
// users never converged (#199). The caller invokes it OUTSIDE opMu and the fence budget
// (it is a local-socket SQL task touching no postmaster state), so it uses its own bounded
// context here -- generous enough for a slow ALTER, not the 5s fence window it would have
// to share with the leadership-critical writes (which starved its psql). Runs at most once
// per process, latching only on success so a transient failure retries the next primary
// tick. Best-effort: the md5 pg_hba line keeps md5 users authenticating regardless.
func (a *agent) rehashManagedUsersOnce(ctx context.Context) {
	if !a.cfg.MigrateLegacyMd5Users || a.rehashedManagedUsers.Load() {
		return
	}
	// The superuser identity + a target DB are needed to connect over the local socket;
	// without them nothing can run, so return WITHOUT latching (retry once they appear).
	if a.cfg.PostgresUser == "" || a.cfg.PostgresDB == "" {
		a.log.Warn("md5->scram re-hash skipped: POSTGRES_USER/POSTGRES_DB unset")
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	users := []struct{ name, pass string }{
		{a.cfg.PostgresUser, a.cfg.PostgresPassword},
		{a.cfg.RepmgrUser, a.cfg.RepmgrPassword},
	}
	ok := true
	for _, u := range users {
		// A managed user with no known password cannot be re-hashed; skip it but do NOT
		// latch convergence, so it retries if the credential later appears (avoids a
		// false "converged" when RehashMd5User no-ops on an empty arg).
		if u.name == "" || u.pass == "" {
			a.log.Warn("md5->scram re-hash skipped (no credential)", "user", u.name)
			ok = false
			continue
		}
		// Connect as the superuser over the local socket (pg_hba `local all all trust`).
		if err := pg.RehashMd5User(rctx, pg.OSExec{}, a.cfg.PostgresUser, a.cfg.PostgresDB, u.name, u.pass); err != nil {
			a.log.Warn("md5->scram re-hash failed (retries next primary tick)", "user", u.name, "err", err)
			ok = false
		}
	}
	if ok {
		a.rehashedManagedUsers.Store(true)
		a.log.Info("md5->scram managed-user re-hash converged")
	}
}

// writePgHba assembles and writes the pg_hba.conf the agent owns in agent mode
// (loopback + the pod CIDR, no legacy 0.0.0.0/0 md5; user POSTGRESQL_PGHBA rules
// inserted above the network catch-alls, #144). It replaces the image's initdb
// default so external md5 access never opens. MD5Fallback is always on: it lays an
// md5 line above each scram rule so md5-stored managed-user passwords still
// authenticate -- the agent is the single author on every node, replacing the
// chart's former postStart md5-fallback awk that raced and left standbys SCRAM-only
// (#199). md5 transparently authenticates SCRAM-stored passwords too, so this is
// compat, not a downgrade, and the pod CIDR is trusted.
func (a *agent) writePgHba() error {
	content := pgconf.AssemblePgHba(pgconf.PgHbaOptions{
		ReplicationUser: a.cfg.RepmgrUser,
		PeerCIDR:        a.cfg.PgHbaPeerCIDR,
		ExtraRules:      a.cfg.PgHbaRules,
		RequireSSL:      a.cfg.TLSRequireSSL,
		ClientCertAuth:  a.cfg.TLSClientCertAuth,
		PostgresUser:    a.cfg.PostgresUser,
		MonitoringUser:  a.cfg.MonitoringUser,
		MD5Fallback:     true,
	})
	return pgconf.WritePgHba(filepath.Join(a.cfg.PGDATA, "pg_hba.conf"), content)
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
		// #288: provenance, not position. A peer deciding whether to promote needs to know
		// this volume was restored, because a PITR restore leaves it BEHIND on LSN while
		// carrying the history the operator actually asked for.
		RestoredAt: ls.RestoredAt,
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

// fenceBudget is the maximum time the primary-serving apiserver writes (highwater
// marker + routing) may hold opMu. Bounding them under the soft-fence window
// (LeaseDuration - RenewDeadline) guarantees a lost-leadership OnLost fence -- which
// must also take opMu to demote -- is never starved behind a slow or partitioned
// apiserver while this node is still read-write (the two-writer window). The writes
// are idempotent and re-asserted every tick, so a deadline-exceeded self-heals on
// the next reconcile. Floored at RetryPeriod for any odd timing config.
func (a *agent) fenceBudget() time.Duration {
	b := a.cfg.LeaseDuration - a.cfg.RenewDeadline
	if b < a.cfg.RetryPeriod {
		b = a.cfg.RetryPeriod
	}
	return b
}

// assertSameCluster enforces invariant 9: never use a peer of a DIFFERENT
// PostgreSQL cluster as a follow/rewind/reclone source (a stale or misrouted pod on
// the shared headless Service, a leftover, or a DR-restored cluster with a fresh
// system_identifier). It compares the peer's pg_control system_identifier to the
// local one. When the local data dir is empty/uninitialized there is no identity to
// protect yet (a bootstrap clone DEFINES it), so the check is a no-op. When the
// peer's identifier cannot be read it does not block -- pg_rewind and the streaming
// walreceiver also reject a sysid mismatch -- so this only refuses on a CONFIRMED
// mismatch, turning a cryptic downstream failure into a clear, actionable one.
func (a *agent) assertSameCluster(ctx context.Context, peer string) error {
	cd, err := pg.ReadControlData(ctx, pg.OSExec{}, a.pgControlDataBin, a.cfg.PGDATA)
	if err != nil || cd.SystemID == 0 {
		return nil // no local cluster identity yet -> nothing to protect
	}
	peerID, ok, perr := a.prober.SystemIdentifier(ctx, a.peerConn(peer))
	if perr != nil || !ok {
		a.log.Warn("cluster-identity check: peer system_identifier unreadable; proceeding (pg_rewind/walreceiver still enforce it)", "peer", peer, "err", perr)
		return nil
	}
	return sameClusterCheck(peer, cd.SystemID, peerID)
}

// sameClusterCheck is the invariant-9 decision: a peer is a valid replication
// source only if its PG system_identifier matches the local cluster's. Extracted
// so the enforcement (not just the controldata/SQL parsing) is unit-testable.
func sameClusterCheck(peer string, localID, peerID uint64) error {
	if peerID != localID {
		return fmt.Errorf("invariant 9: refusing %s as replication source: system_identifier %d != local %d (different cluster)", peer, peerID, localID)
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

// rejoinOnto brings this node back under target: rewind forward onto it, or -- when the
// histories diverged too far for pg_rewind -- re-clone while preserving the old data
// directory (#175). Shared by the RejoinForward decision and by the Follow path when this
// node is absent from its own repmgr.nodes copy (#297); the remedy is the same, namely take
// this node's data and metadata from the current primary.
func (a *agent) rejoinOnto(ctx context.Context, target string) error {
	// Invalidate the follow latch on ENTRY, not after the rejoin succeeds. Either point is
	// defensible -- the latch caches which upstream replication is actually pointed at, so
	// keeping it through a failed rejoin is arguably the more truthful state -- but clearing
	// it up front makes the invariant unconditional: an attempt to rejoin always invalidates
	// it. Nothing today depends on that (the escalation is only reachable when the latch
	// already differs from the target, so a stale value can never wedge the Follow path, and
	// its one consumer -- cascadeFollowTarget's stickiness -- re-checks cascadeQualifies).
	// It removes a footgun for whoever later reads the latch earlier in the decision.
	a.followUpstream = ""
	// Reset the stall counter too (#288 review). Without this, StandbyStalled stays latched: the
	// counter only clears on an observed streaming upstream, so a stall NOT caused by divergence
	// (upstream out of max_wal_senders, an invalidated slot, replication auth failing) made
	// Decide return RejoinForward on EVERY tick -- stopping, rewinding and restarting postgres
	// every 5s instead of once. Worse, each attempt that falls through to ReclonePreserving
	// leaves another .diverged.<ts> directory that nothing ever removes, so the PVC fills.
	// Zeroing here means one rejoin per stall, and a genuine re-stall has to re-earn its ticks.
	a.standbyNoReceiverTicks = 0
	// Invariant 9: never rewind/reclone onto a different cluster. Checked before the
	// demote so a healthy node is not stopped for a doomed rejoin.
	if err := a.assertSameCluster(ctx, target); err != nil {
		return err
	}
	if err := a.sup.Demote(ctx, true); err != nil {
		return err
	}
	if err := a.mech.RejoinForceRewind(ctx, a.peerMechConn(target)); err != nil {
		// Same marker as BootstrapClone (#288 review): ReclonePreserving runs the same
		// pg_basebackup, so an interrupted one leaves an equally torn PGDATA. Without it
		// discardTornClone is a no-op on the next boot, and recovery relies on the NEXT
		// ReclonePreserving renaming the torn directory aside as another `.diverged.<ts>` --
		// which nothing ever removes, so every interrupted attempt permanently consumes a full
		// data directory's worth of PVC space.
		a.beginClone()
		if err := a.mech.ReclonePreserving(ctx, a.peerMechConn(target)); err != nil {
			a.discardTornClone(ctx)
			return err
		}
		a.endClone()
		// A full re-clone: this directory's contents now come from the peer, not from
		// whatever restore the record beside it describes.
		a.dropRestoreRecord("the data directory was re-cloned from " + target)
	} else {
		// A successful pg_rewind ALSO rewrites this node's history onto the target's, so the
		// restore record beside PGDATA no longer describes these contents (#288 review).
		// Without this the claim outlived the data it described: after a controlled switchover
		// the rewound node kept its restore identity, so a later lease landing on it made it
		// skip a peer holding more WAL and promote anyway -- data loss with no restore in
		// flight. Only the node that still carries the restored history keeps the claim.
		a.dropRestoreRecord("the data directory was rewound onto " + target)
	}
	return a.sup.Start(ctx)
}

// newMechanism selects the replication mechanics from config (#287). Policy is identical
// either way -- the Lease, the election, fencing and routing all live in reconcile, which
// holds only the Mechanism interface -- so this is the single place the two differ.
//
// config.Load already rejects any value that is neither repmgr nor native at boot, so the
// error return here is unreachable for a config that went through Load -- but this fails
// loudly rather than silently defaulting to repmgr, matching the fail-fast posture the rest
// of this codebase takes on required config (no ||/?? fallbacks). Without that, a future
// mechanism value added to the enum without a case here would silently run repmgr with only
// this function's own log line as a hint, rather than surfacing the drift.
//
// "" (distinct from an unrecognised value) is accepted alongside MechanismRepmgr: it is
// what an older, pre-#287 image or a config built without going through Load's own
// empty->repmgr normalization looks like, and must keep behaving exactly as it always has.
func newMechanism(cfg *config.Config, repmgrConf, pgBindir string, log *slog.Logger) (mechanism.Mechanism, error) {
	switch cfg.Mechanism {
	case config.MechanismNative:
		// #289 has landed: the agent now owns physical slot lifecycle in native mode
		// (create before clone, reconcile against the live pod set on every primary tick,
		// drop only inactive orphans). Topology (#288) is still the outstanding blocker.
		log.Warn("using the EXPERIMENTAL native mechanism: runs a real multi-node cluster since #288 (topology from pg_stat_replication, agent-owned bootstrap), but cascadingReplication is unsupported and an existing repmgr cluster cannot be migrated in place yet (#292)",
			"mechanism", cfg.Mechanism)
		// PodName is passed twice by design: once derived into the slot name (#289) and once
		// verbatim as application_name (#288). Both are this node's identity, but they land in
		// different places -- pg_replication_slots and pg_stat_replication -- and the topology
		// probe reads whichever is available.
		return mechanism.NewNative(cfg.PGDATA, pgBindir, cfg.RepmgrPassword, slotNameFor(cfg.PodName), cfg.PodName), nil
	case config.MechanismRepmgr, "":
		return mechanism.NewRepmgr(repmgrConf, cfg.PGDATA, cfg.RepmgrPassword), nil
	default:
		return nil, fmt.Errorf("newMechanism: unrecognised MECHANISM %q (want %s|%s)", cfg.Mechanism, config.MechanismRepmgr, config.MechanismNative)
	}
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

// cleanupGhostNodes unregisters repmgr.nodes records for pods the StatefulSet no
// longer runs (ordinal >= NodeCount), left behind by a replicaCount scale-down
// (#139). Primary-only -- the agent calls it while holding the lease as primary, the
// single owner of repmgr.nodes, so there is no cross-node race. Best-effort and
// idempotent: it lists the STANDBY node_ids and unregisters only those above the live
// ordinal range, so a momentarily-unreachable live node is never touched and a row
// already gone just yields a warning the next tick retries. It lists only standby rows
// (StandbyNodeIDs) so a scaled-down ex-primary -- which `repmgr standby unregister`
// cannot remove -- is left for an operator rather than re-attempted (and re-warned)
// every tick. A non-positive NodeCount is a no-op via ghostNodeIDs, so this can never
// unregister a live node.
//
// Skipped entirely under the #287 native mechanism: it has no repmgr.nodes (native's
// Unregister is a no-op, see mechanism.Native), so StandbyNodeIDs would error on every
// primary tick forever. That is harmless (RegistryRead-style fail-open elsewhere), but
// here it is pure log noise with no cleanup to retry -- skip the call rather than warn
// on a known, permanent condition.
func (a *agent) cleanupGhostNodes(ctx context.Context) {
	if a.cfg.Mechanism == config.MechanismNative {
		return
	}
	ids, err := a.prober.StandbyNodeIDs(ctx, a.selfConn())
	if err != nil {
		a.log.Warn("list repmgr.nodes for ghost cleanup", "err", err)
		return
	}
	for _, id := range ghostNodeIDs(ids, a.cfg.NodeCount) {
		if err := a.mech.Unregister(ctx, id); err != nil {
			a.log.Warn("unregister ghost repmgr node", "node_id", id, "err", err)
			continue
		}
		a.log.Info("unregistered ghost repmgr node left by a scale-down", "node_id", id)
	}
}

// topologyTick publishes the primary's replication topology from pg_stat_replication (#288).
//
// This is what replaced repmgr.nodes. That table was a CACHE of self-reported metadata:
// nodes wrote their own rows, the rows outlived the pods (#139's ghosts), and it could
// disagree with both the lease and the observed positions. pg_stat_replication cannot go stale
// that way -- it is the primary's live connection list, so a departed pod is simply absent and
// there is no durable row to strand.
//
// OBSERVE-ONLY, under BOTH mechanisms. Two deliberate constraints:
//
//   - Nothing in reconcile.Decide may consume this, and it deliberately does not populate an
//     Observation field. A standby's row VANISHES the instant it disconnects -- i.e. exactly at
//     the failover moment a promotion is being decided -- which is the mirror of the
//     pg_stat_wal_receiver caveat in probe.go. Absence here means "not streaming right now",
//     never "this node does not exist". An unused Observation field would just invite a future
//     contributor to gate a promote on it.
//   - It runs under repmgr too, not only native. repmgr sets application_name to the node name
//     itself, so the view is equally readable there, and publishing a gauge that only ever
//     moves on the non-default mechanism is the mistake slotsTick's own comment argues against.
//
// topologyTick funds itself from its OWN budget rather than the caller's fence budget (#288 review).
// wctx is fenceBudget() -- 5s on chart defaults -- and by the time topologyTick runs it has
// already paid for RegisterPrimary, slotsTick, PrimaryWALPosition, advanceMarker and
// assertPrimaryRouting. Exhausted, ReplicationTopology fails and topologyTick returns early,
// LEAVING THE GAUGES AT THEIR PREVIOUS VALUES -- and on the promote path those were just zeroed
// by ClearTopology, so a freshly promoted primary with healthy standbys exported
// replicas_streaming = 0. Deriving from ctx keeps it observational: it cannot starve the marker
// write or the routing switch, and it cannot be starved BY them either.
// latchFollow records the upstream this node is now replicating from, and EXPIRES the restore
// claim (#288 review).
//
// The claim exists to protect a restored volume's history until the cluster has adopted it: a
// PITR rewinds this node, so ranking on LSN alone let a stale peer out-position it and promote
// pre-restore data. That danger ends the moment this node is itself following a peer. Either the
// upstream's history descends from the restore -- adopted, so the claim is moot -- or it does
// not, in which case getting here required a rewind/re-clone, which already dropped the record.
//
// Without this bound the claim was PERMANENT, and permanent provenance is its own data-loss bug:
// a node restored months ago, long since demoted cleanly and streaming happily, still outranked
// a peer holding gigabytes more WAL. It would win the lease, skip that peer in its own election,
// and promote with less WAL -- on every election for the life of the PVC.
func (a *agent) latchFollow(target string) {
	a.followUpstream = target
	a.dropRestoreRecord("this node is now following " + target + ", so the cluster has moved past the restore")
}

func (a *agent) topologyTick(ctx context.Context) {
	tctx, cancel := context.WithTimeout(ctx, a.fenceBudget())
	defer cancel()
	rows, err := a.prober.ReplicationTopology(tctx, a.selfConn())
	if err != nil {
		a.log.Warn("read replication topology", "err", err)
		return
	}
	// Cascading replication makes a standby an upstream for other standbys, so a cascading
	// child streams from a PEER and never appears in this primary's pg_stat_replication
	// (#288 review). Publishing expected-vs-streaming there would report a permanent shortfall
	// on a perfectly healthy cluster, and the gap warning would never clear. The count is only
	// meaningful in a star topology.
	if a.cfg.CascadeReplication {
		a.metr.SetTopology(observe.TopologyStats{Streaming: countStreamingReplicas(rows)})
		return
	}
	// The expected count needs the live pod set, and that is an uncached apiserver LIST. Under
	// the repmgr mechanism reconcileSlots returns before its own livePodOrdinals, so charging
	// every existing install a second LIST per primary tick for a purely observational gauge is
	// not a trade worth making (#288 review). Publish what the primary can see on its own; the
	// expected/gap half is native-only, where the same LIST is already being made.
	if a.cfg.Mechanism != config.MechanismNative {
		a.metr.SetTopology(observe.TopologyStats{Streaming: countStreamingReplicas(rows)})
		return
	}
	// The live pod set from the API, not NodeCount: that env var is baked in at render time and
	// is stale on every pod that has not rolled yet (see orphanSlot).
	live, liveErr := a.livePodOrdinals(ctx)
	selfOrd, selfOK := podOrdinal(a.cfg.PodName)
	if liveErr != nil {
		// Leave the gauges at their PREVIOUS values rather than publishing Expected: 0 --
		// exactly what observeSlots does on the same failure, and the earlier revision of this
		// code got it backwards. A published zero reads as "nothing should be streaming", so an
		// expected-vs-streaming alert goes quiet during precisely the apiserver blip an operator
		// most wants to hear about.
		a.log.Warn("list live pods for the topology view; leaving the gauges unchanged this tick", "err", liveErr)
		return
	}
	var expected int64
	{
		for ord := range live {
			if selfOK && ord == selfOrd {
				continue // the primary does not stream from itself
			}
			expected++
		}
	}

	seen := make(map[string]bool, len(rows))
	var streaming, unidentified int64
	for _, r := range rows {
		if !r.Streaming() {
			continue // exists, but still catching up: not yet a usable replica
		}
		if isCloneConnection(r) {
			continue // a base backup in flight, not a replica
		}
		streaming++
		if pod := a.resolveReplicaPod(r); pod != "" {
			seen[pod] = true
		} else {
			unidentified++
		}
	}
	a.metr.SetTopology(observe.TopologyStats{Streaming: streaming, Expected: expected, Unidentified: unidentified})

	// One log line per CHANGE, not per tick: a rolling restart legitimately parks a pod
	// off-stream for a while, and warning every 5s about it would bury everything else.
	var missing []string
	for ord := range live {
		if selfOK && ord == selfOrd {
			continue
		}
		pod := fmt.Sprintf("%s-%d", a.base, ord)
		if !seen[pod] {
			missing = append(missing, pod)
		}
	}
	sort.Strings(missing)
	state := strings.Join(missing, ",")
	if state == a.lastTopologyGap {
		return
	}
	a.lastTopologyGap = state
	if state == "" {
		a.log.Info("replication topology complete: every live peer is streaming", "streaming", streaming)
		return
	}
	a.log.Warn("live peers are not streaming from this primary",
		"pods", state, "streaming", streaming, "expected", expected, "unidentified", unidentified)
}

// countStreamingReplicas counts the rows that are real streaming standbys, excluding a base
// backup in flight (#288).
func countStreamingReplicas(rows []pg.ReplicaRow) int64 {
	var n int64
	for _, r := range rows {
		if r.Streaming() && !isCloneConnection(r) {
			n++
		}
	}
	return n
}

// resolveReplicaPod maps one pg_stat_replication row to a pod name (#288).
//
// application_name first: native writes the pod name there via primary_conninfo, and repmgr
// writes node_name, which is the same string. A standby cloned BEFORE #288 still dials with
// libpq's default ("walreceiver"), so the slot recovers it instead -- native names slots by pod
// ordinal, and slotOrdinal already understands both pg_ha_slot_<ord> and the legacy
// repmgr_slot_<node_id>. Verified against real PostgreSQL 18 with both shapes streaming to one
// primary at once. Returns "" when neither source identifies the pod, which the caller counts
// rather than hides.
func (a *agent) resolveReplicaPod(r pg.ReplicaRow) string {
	// Only an application_name that actually names a pod of THIS StatefulSet is trusted. A
	// clone in progress opens a second replication connection of its own -- pg_basebackup
	// -X stream reports application_name='pg_basebackup' (#288 review) -- and taking that at
	// face value both inflated the streaming count and hid the real pod, because the slot
	// fallback that would have identified it was never consulted.
	if ord, ok := podOrdinal(r.AppName); ok && r.AppName == fmt.Sprintf("%s-%d", a.base, ord) {
		return r.AppName
	}
	if ord, ok := slotOrdinal(r.SlotName); ok {
		return fmt.Sprintf("%s-%d", a.base, ord)
	}
	return ""
}

// isCloneConnection reports whether a pg_stat_replication row is a base backup rather than a
// standby streaming WAL (#288 review). pg_basebackup -X stream shows up as a second streaming
// connection for the duration of every clone -- fresh install, scale-up, re-clone -- and
// counting it would inflate the replica count while the pod it belongs to is still not
// replicating.
func isCloneConnection(r pg.ReplicaRow) bool { return r.AppName == "pg_basebackup" }

// slotsTick is the per-primary-tick replication-slot pass (#289): OBSERVE always, then
// RECONCILE only under the native mechanism.
//
// The split is load-bearing, not tidiness. Observation is read-only and mechanism-agnostic,
// and so is the failure it exists to catch: repmgr mode has slots too (repmgr_slot_*), they
// pin WAL in exactly the same silent way, and the chart renders the slot PrometheusRules for
// every agent-mode release regardless of mechanism. Publishing the gauges only in native mode
// would leave those alerts pinned at zero on the DEFAULT mechanism -- an alert that cannot
// fire reads as coverage while providing none, which is worse than shipping no alert at all.
// Mutation stays native-only: under the repmgr mechanism repmgr owns slot lifecycle (it
// creates/attaches slots during standby clone/follow), so two owners would fight over the
// same objects.
func (a *agent) slotsTick(ctx context.Context) {
	slots, ok := a.observeSlots(ctx)
	if !ok {
		return
	}
	a.reconcileSlots(ctx, slots)
}

// standbySlotsTick is the slot pass for a node running as a STANDBY (#289 review).
//
// A demoted primary keeps every slot it minted while it WAS the primary. Nothing removed
// them: reconcileSlots runs only on the primary branches, pg_basebackup and pg_rewind both
// exclude pg_replslot, and a plain Follow touches nothing. Those slots go inactive (their
// standbys now stream from the new primary) and an inactive slot restricts WAL removal on a
// standby exactly as it does on a primary -- so the ex-primary's own pg_wal grows without
// bound on its own volume until max_slot_wal_keep_size invalidates them. That is the same
// silent failure this whole change exists to prevent, just on the node nobody was watching.
// It also does not self-heal on a later re-promotion: by then those ordinals have live pods
// again, so the primary reconcile classifies them as live peers' slots and leaves them.
//
// The reclaim policy here is simpler than the primary's, and deliberately so: under native a
// standby has NO legitimate downstream at all. Its own slot lives on its upstream, not
// locally, and cascadingReplication + native is rejected at render time precisely because
// slot reconcile is primary-only -- so there is no configuration in which a standby is
// someone's upstream. Every agent-minted slot found locally is therefore a leftover, with no
// pod set to consult. The atomic `AND NOT active` in the drop is still what makes it safe:
// anything genuinely streaming through one survives regardless of what this decides.
//
// Observation runs under every mechanism (the gauges must be truthful about a standby
// holding WAL back too); mutation is native-only, same as the primary path.
func (a *agent) standbySlotsTick(ctx context.Context) {
	slots, ok := a.observeSlots(ctx)
	if !ok {
		return
	}
	if a.cfg.Mechanism != config.MechanismNative {
		return
	}
	for _, sl := range slots {
		if !leftoverStandbySlot(sl.Name) {
			continue
		}
		dropped, err := a.prober.DropPhysicalSlotIfInactive(ctx, a.selfConn(), sl.Name)
		if err != nil {
			a.log.Warn("drop leftover replication slot on standby", "slot", sl.Name, "err", err)
			continue
		}
		if dropped {
			a.log.Info("dropped leftover replication slot left behind by a demotion",
				"slot", sl.Name, "retained_wal_bytes", sl.RetainedWALBytes)
		}
	}
}

// leftoverStandbySlot reports whether a slot found on a STANDBY is one this agent may
// reclaim (#289 review): any name it can prove it minted, plus any legacy repmgr slot.
//
// No ordinal or pod-set test, unlike orphanSlot: a standby is never an upstream under this
// mechanism (see standbySlotsTick), so the ordinal a leftover names says nothing about
// whether it is still needed -- it names the peer that used to stream from this node back
// when it was primary. An operator's own slot, and any logical slot backing a subscription,
// stay out of reach exactly as they do on the primary path.
func leftoverStandbySlot(name string) bool {
	if strings.HasPrefix(name, legacySlotPrefix) {
		return true
	}
	_, ok := slotOrdinal(name)
	return ok && strings.HasPrefix(name, slotPrefix)
}

// observeSlots reads this node's physical replication slots and publishes the aggregate
// gauges (#289). Runs under every mechanism -- see slotsTick.
//
// Reports false when the query failed, so a caller cannot mistake "could not look" for
// "there are none": creating or dropping on that basis is how a needed slot gets destroyed.
// The gauges are left at their previous values in that case rather than zeroed, because a
// transient psql failure must not read as "the orphan is gone" and resolve the alert.
func (a *agent) observeSlots(ctx context.Context) ([]pg.SlotState, bool) {
	slots, err := a.prober.PhysicalSlots(ctx, a.selfConn())
	if err != nil {
		a.log.Warn("list physical replication slots", "err", err)
		return nil, false
	}
	a.metr.SetSlots(slotMetrics(slots))
	return slots, true
}

// reconcileSlots makes the primary's physical replication slots match the live pod set
// (#289), from the slots observeSlots already read. Native mechanism only (see slotsTick).
//
// Runs as the lease holder on a read-write primary, which is what makes it race-free:
// slots are only ever mutated by the single node that holds the lease, so there is no
// cross-node coordination to get wrong -- the same argument that makes the primary-only
// repmgr.nodes cleanup safe. Callers must also skip it while paused; act() only reaches
// the primary branches when not paused.
//
// Two directions, both idempotent:
//
//   - CREATE for every expected peer ordinal. Driven by the EXPECTED pod set (NodeCount),
//     not by observed standbys: a slot must exist BEFORE its standby tries to stream, and
//     after a promote the surviving standbys will follow this node within a tick. Waiting
//     to observe them first would leave every Follow racing slot creation, which is the
//     ordering the issue calls out. This is also why the call sits ahead of the routing
//     switch in the Promote path. Over-creating is harmless (an unused slot for an ordinal
//     that never appears is reclaimed by the drop pass below); under-creating is not.
//   - DROP every orphan (see orphanSlot), decided against the LIVE pod set read from the
//     API rather than NodeCount -- see orphanSlot for why that distinction is load-bearing.
//     The drop additionally refuses an active slot in SQL, so a slot someone is streaming
//     through survives even if the name and pod set both say reclaimable.
//
// A failed pod list SKIPS the drop pass entirely rather than falling back to NodeCount:
// without knowing what exists, the only safe answer is to reclaim nothing this tick and
// retry on the next one. Creation still runs, because creating a slot that turns out to be
// unnecessary costs nothing but a later drop.
//
// Best-effort per slot: a failure is logged and retried next tick rather than aborting the
// rest, because a single unreachable/locked slot must not block reclaiming the others --
// and the whole point is that an unreclaimed slot silently fills the volume.
func (a *agent) reconcileSlots(ctx context.Context, slots []pg.SlotState) {
	if a.cfg.Mechanism != config.MechanismNative {
		return
	}
	if a.cfg.NodeCount <= 0 {
		return
	}
	self := a.selfConn()
	have := make(map[string]pg.SlotState, len(slots))
	for _, s := range slots {
		have[s.Name] = s
	}

	// The live pod set is read FIRST because both passes need it, and because reading it
	// after the creates is what made them fight: the drop pass judges against the live set
	// while the create pass judged against NodeCount, so any ordinal inside [0, NodeCount)
	// with no live pod was created on one tick and reclaimed on the next, forever. After a
	// replicaCount 2->1 scale-down the ordinal-0 primary still holds the OLD
	// REPMGR_NODE_COUNT until the StatefulSet rolls it LAST, so that oscillation ran for the
	// whole rollout, logging a create and a drop every tick.
	live, liveErr := a.livePodOrdinals(ctx)
	if liveErr != nil {
		// No authoritative view of what exists. Creation still runs off NodeCount -- an
		// unnecessary slot costs only a later drop -- but nothing is reclaimed: dropping on a
		// stale or guessed pod set is how a needed slot gets destroyed, and an orphan
		// surviving one more tick costs only WAL, which the alert covers if it persists.
		a.log.Warn("list live pods for slot reconcile; creating from NodeCount and skipping the drop pass this tick", "err", liveErr)
	}

	selfOrd, selfOK := podOrdinal(a.cfg.PodName)
	for ord := 0; ord < a.cfg.NodeCount; ord++ {
		if selfOK && ord == selfOrd {
			continue // the primary does not stream from itself
		}
		// Skip ordinals with no live pod, so the create pass agrees with the drop pass.
		// This does not reintroduce the race the expected-set drive exists to avoid: a pod
		// that has been CREATED but is still cloning or starting is already in the live set,
		// so its slot is still minted before it streams -- and a pod that does not exist yet
		// has nothing to stream. Clone also ensures its own slot on the upstream, so even the
		// tick of latency is covered.
		if liveErr == nil && !live[ord] {
			continue
		}
		name := slotPrefix + strconv.Itoa(ord)
		if _, ok := have[name]; ok {
			continue
		}
		if err := a.prober.CreatePhysicalSlot(ctx, self, name); err != nil {
			a.log.Warn("create replication slot", "slot", name, "err", err)
			continue
		}
		a.log.Info("created replication slot for an expected standby", "slot", name)
	}

	if liveErr != nil {
		return
	}
	for _, s := range slots {
		if !orphanSlot(s.Name, a.cfg.PodName, live) {
			continue
		}
		dropped, err := a.prober.DropPhysicalSlotIfInactive(ctx, self, s.Name)
		if err != nil {
			a.log.Warn("drop orphaned replication slot", "slot", s.Name, "err", err)
			continue
		}
		if dropped {
			a.log.Info("dropped orphaned replication slot",
				"slot", s.Name, "retained_wal_bytes", s.RetainedWALBytes)
		}
	}
}

// livePodOrdinals reads the StatefulSet's actual pod set from the API and returns the set
// of ordinals present (#289).
//
// The API, not REPMGR_NODE_COUNT: that env var is baked into each pod at render time, so
// during a scale-up rollout it is stale on every pod that has not rolled yet -- see
// orphanSlot. A pod whose name carries no parseable ordinal is ignored rather than failing
// the whole read; it cannot own an agent-minted slot either way.
func (a *agent) livePodOrdinals(ctx context.Context) (map[int]bool, error) {
	names, err := a.kube.ListPodNames(ctx, a.cfg.PodSelector)
	if err != nil {
		return nil, err
	}
	live := make(map[int]bool, len(names))
	for _, n := range names {
		if ord, ok := podOrdinal(n); ok {
			live[ord] = true
		}
	}
	return live, nil
}

// slotMetrics reduces the observed slots to what the metrics surface exports (#289):
// the total count, how many are inactive, how many PostgreSQL has invalidated, and the
// largest retained-WAL figure across all slots.
//
// "Inactive" requires Reserving, i.e. a non-NULL restart_lsn. A slot the primary
// pre-created for a standby that has not arrived yet is inactive and reserves NOTHING, so
// counting it would fire PGHAReplicationSlotInactive -- whose whole premise is "it is
// accumulating WAL" -- on a slot accumulating nothing. Native mode pre-creates a slot per
// expected ordinal, so that false positive is the DEFAULT state there, not an edge case.
//
// Invalidated is counted separately because the retained-WAL gauge cannot see it: exceeding
// max_slot_wal_keep_size nulls restart_lsn, so the bytes figure collapses to zero at the
// instant the slot dies (verified against PostgreSQL 18). Without its own gauge the worst
// outcome would look identical to the healthiest one.
//
// Max rather than a per-slot label set: the alert that matters is "some slot is holding
// back too much WAL", and a single gauge answers it without making cardinality grow with
// the cluster or leaving stale series behind when a slot is dropped (this metrics surface
// is hand-written text with no per-series lifecycle, so an unbounded label set would
// leak). The slot's identity is in the agent's log line at drop time and in
// pg_replication_slots itself.
func slotMetrics(slots []pg.SlotState) observe.SlotStats {
	st := observe.SlotStats{Total: int64(len(slots))}
	for _, s := range slots {
		if !s.Active && s.Reserving {
			st.Inactive++
		}
		if s.Invalidated() {
			st.Invalidated++
		}
		if s.RetainedWALBytes > st.MaxRetainedWALBytes {
			st.MaxRetainedWALBytes = s.RetainedWALBytes
		}
	}
	return st
}

// streamingFromTarget reports whether local Postgres is already actively streaming
// from target's FQDN (#182). Both the standby's primary_conninfo and a.fqdn derive
// the upstream host from the same registered conninfo, so sender_host equals
// a.fqdn(target); compared case-insensitively with any trailing dot trimmed. A probe
// error or any mismatch returns false -- the caller then runs repmgr standby follow
// (which repoints to a new upstream, or no-ops benignly if already attached), so a
// false negative only costs one extra follow, never a missed repoint.
func (a *agent) streamingFromTarget(ctx context.Context, target string) bool {
	host, streaming, err := a.prober.StreamingUpstream(ctx, a.selfConn())
	if err != nil || !streaming {
		return false
	}
	norm := func(s string) string { return strings.ToLower(strings.TrimRight(s, ".")) }
	return norm(host) == norm(a.fqdn(target))
}

func (a *agent) startMetrics(ctx context.Context) {
	srv := &http.Server{
		Addr:    metricsAddr,
		Handler: a.metr.Handler(a.cfg.ReconcileInterval * 3),
		// Timeouts on a listener that shares a process with PID 1 and the supervised
		// postmaster: without them a client that opens connections and never sends a
		// request accumulates goroutines and buffers here indefinitely.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
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

// nodeIDBase is the repmgr node_id of ordinal 0 (node_id = nodeIDBase + ordinal),
// matching init-repmgr.sh and nodeID().
// #288 audit. Every consumer of the +1000 offset, and which of them must outlive
// mechanism.Repmgr:
//
//   - nodeID() -> NodeIdentity.NodeID, read only by Repmgr.GenerateConfig (node_id= in
//     repmgr.conf). Repmgr-only; native's GenerateConfig discards NodeIdentity entirely.
//   - nodeID() -> the #297 registry gate mapping in readRegistryForGate. Repmgr-only since
//     #288: that whole read is skipped under native.
//   - nodeID() -> RegisterStandby + Conn.NodeID in the Follow branch. Repmgr-only;
//     Native.Follow reads upstream.Host and mentions NodeID only in an error string.
//   - ghostNodeIDs(), from cleanupGhostNodes. Repmgr-only; already native-skipped.
//   - slotOrdinal()'s legacy branch. **This one must survive #294's deletion of
//     mechanism.Repmgr**: it reverses the offset to reclaim repmgr_slot_<node_id> orphans
//     left behind by a repmgr->native migration (#292). Deleting nodeIDBase with the repmgr
//     mechanism would silently strand those slots, pinning WAL forever.
//
// So the offset is NOT removable while mechanism: repmgr is selectable (#288 lists that as a
// non-goal), and even afterwards slotOrdinal keeps needing it. What #288 does instead is stop
// PROPAGATING a node id on native code paths, so no native path carries a repmgr identity.
// podOrdinal (below) and reconcile.podOrdinal are not repmgr-specific and stay.
const nodeIDBase = 1000

// repmgrNodeID is this node's repmgr node_id, or 0 under the native mechanism (#288 audit).
func (a *agent) repmgrNodeID() int {
	if a.cfg.Mechanism == config.MechanismNative {
		return 0
	}
	return nodeID(a.cfg.PodName)
}

// repmgrPeerNodeID is pod's repmgr node_id, or 0 under the native mechanism (#288 audit).
func (a *agent) repmgrPeerNodeID(pod string) int {
	if a.cfg.Mechanism == config.MechanismNative {
		return 0
	}
	return nodeID(pod)
}

// nodeID maps a pod name to its repmgr node_id (ordinal + nodeIDBase), matching
// init-repmgr.sh. Shares podOrdinal with the slot naming (#289) so the pod-name ->
// ordinal convention has exactly one implementation; 0 when the name carries none.
func nodeID(pod string) int {
	ord, ok := podOrdinal(pod)
	if !ok {
		return 0
	}
	return ord + nodeIDBase
}

// ghostNodeIDs returns the registered node_ids whose pods the StatefulSet no longer
// runs -- ordinal (node_id - nodeIDBase) >= nodeCount, since the live set is ordinals
// 0..nodeCount-1. These are the rows a replicaCount scale-down strands in repmgr.nodes
// (#139). The discriminator is purely structural (the StatefulSet always trims from the
// top ordinal), never reachability, so a live-but-momentarily-unreachable node is never
// flagged. Returns nil when nodeCount is not positive -- a misconfigured/zero count must
// never make every node look like a ghost -- and ignores ids below the base (an unknown
// numbering scheme this agent did not assign).
func ghostNodeIDs(ids []int, nodeCount int) []int {
	if nodeCount <= 0 {
		return nil
	}
	var ghosts []int
	for _, id := range ids {
		if ord := id - nodeIDBase; ord >= nodeCount {
			ghosts = append(ghosts, id)
		}
	}
	return ghosts
}

// slotPrefix names the agent's own physical replication slots (#289): slotPrefix +
// pod ordinal, e.g. pg_ha_slot_1.
//
// Ordinal-derived, not node_id-derived: the ordinal is the StatefulSet's own stable
// identity, it survives pod restarts, and it does not carry the +1000 repmgr offset into
// a mechanism that has no repmgr. The prefix is what makes ownership decidable -- the
// agent creates and drops only names it can prove it minted, so an operator's own slot
// (or a logical slot for a subscription) is never touched.
const slotPrefix = "pg_ha_slot_"

// legacySlotPrefix is repmgr's own slot naming (repmgr_slot_<node_id>).
//
// Native mode never streams through one, so an INACTIVE slot with this prefix on a
// native-mode primary is dead weight pinning WAL -- exactly the silent disk-fill #289
// exists to stop. It is therefore reclaimable by the native reconcile, which is what
// makes a repmgr->native migration (#292) safe rather than leaving a permanent orphan
// behind. The `NOT active` guard in the drop is what keeps this safe mid-migration: a
// standby still streaming through its repmgr slot holds it active, so it survives until
// it has genuinely moved onto its pg_ha_slot_ replacement.
const legacySlotPrefix = "repmgr_slot_"

// slotNameFor returns the slot a pod streams through, or "" when the pod name carries no
// parseable ordinal (nothing to derive a stable name from -- better no slot than an
// unstable one that strands a new slot on every restart).
func slotNameFor(pod string) string {
	ord, ok := podOrdinal(pod)
	if !ok {
		return ""
	}
	return slotPrefix + strconv.Itoa(ord)
}

// podOrdinal extracts the StatefulSet ordinal from a pod name.
func podOrdinal(pod string) (int, bool) {
	i := strings.LastIndex(pod, "-")
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(pod[i+1:])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// orphanSlot decides whether the primary may drop slot on the strength of its NAME and
// the LIVE POD SET (#289). Activity is not considered here -- the drop itself refuses an
// active slot atomically in SQL, which is both safer and race-free.
//
// liveOrdinals is every pod ordinal the Kubernetes API currently reports for this
// StatefulSet, and it -- not REPMGR_NODE_COUNT -- is the authority on what exists. That
// distinction is the whole safety argument here:
//
//   - NodeCount is read once at process boot from an env var baked into the pod template,
//     so during a scale-up rollout every pod that has not rolled yet (including, typically,
//     the ordinal-0 primary, which the StatefulSet rolls LAST) still holds the OLD count.
//     Deciding ownership from it would make the stale primary classify a brand-new
//     standby's just-created slot as a scale-down ghost and drop it -- while that standby
//     is briefly inactive between pg_basebackup finishing and its postmaster reattaching
//     -- reintroducing exactly the WAL gap this whole change exists to close.
//   - A pod that is mid-restart, still starting, or has never gossiped is present in the
//     live pod list, so its slot is protected. Inactivity alone is NOT evidence that a
//     consumer is gone for good; it is also what a routine restart looks like.
//
// Reclaimable, and only when the ordinal has no live pod:
//   - an agent-minted slot for a departed ordinal: the slot-side twin of the #139 ghost
//     row, and the one with real consequences (it pins WAL until the volume fills)
//   - an agent-minted slot for THIS pod, regardless of the pod set: the primary does not
//     stream from itself, so its own slot is unused while it holds the lease even though
//     its pod plainly exists.
//
// Plus one case that does not depend on the pod set at all:
//   - ANY legacy repmgr_slot_*, live ordinal or not. Native mode never streams through one,
//     so every such slot is dead weight the moment a cluster is on this mechanism -- and
//     scoping it to departed ordinals (as an earlier revision did) left a repmgr->native
//     migration with a permanent orphan for every node that survived the migration, which
//     is precisely the #292 case this is supposed to clean up. What makes it safe is the
//     atomic `AND NOT active` in the drop, not the pod set: a node still carrying its
//     stream through a repmgr slot mid-migration holds it ACTIVE, so the drop is refused
//     and the slot survives until that stream has genuinely moved to its pg_ha_slot_
//     replacement. Deciding this from liveness instead would be both weaker (it never
//     cleans up) and no safer.
//
// Everything else -- an unrecognised name, or a live peer's agent-minted slot -- is left
// alone. An empty liveOrdinals reclaims nothing but self and legacy slots: a failed or
// empty pod list must never make every standby's slot look orphaned.
func orphanSlot(name, selfPod string, liveOrdinals map[int]bool) bool {
	selfOrd, selfOK := podOrdinal(selfPod)

	ord, ok := slotOrdinal(name)
	if !ok {
		return false // not a name this agent minted, and not a legacy repmgr slot
	}
	// A legacy repmgr slot is never used by native mode; the NOT active guard in the drop
	// is what protects one still carrying a stream mid-migration.
	if strings.HasPrefix(name, legacySlotPrefix) {
		return true
	}
	// This pod's own slot: unused while it is the primary, regardless of liveness.
	if selfOK && ord == selfOrd {
		return true
	}
	if len(liveOrdinals) == 0 {
		return false
	}
	return !liveOrdinals[ord]
}

// slotOrdinal extracts the pod ordinal from a slot name the agent recognises as ownable --
// its own pg_ha_slot_<ordinal> or a legacy repmgr_slot_<node_id> (whose id is
// nodeIDBase+ordinal). Reports false for anything else, which is what keeps an operator's
// own slot, or a logical slot backing a subscription, permanently out of reach.
func slotOrdinal(name string) (int, bool) {
	if rest, ok := strings.CutPrefix(name, slotPrefix); ok {
		n, err := strconv.Atoi(rest)
		if err != nil || n < 0 {
			return 0, false
		}
		return n, true
	}
	if rest, ok := strings.CutPrefix(name, legacySlotPrefix); ok {
		n, err := strconv.Atoi(rest)
		if err != nil || n < nodeIDBase {
			return 0, false
		}
		return n - nodeIDBase, true
	}
	return 0, false
}
