// Command agent is the PID-1 PostgreSQL HA agent: it holds a Kubernetes Lease as
// the sole authority for who is primary and drives the Mechanism (native pg_ctl /
// pg_basebackup / pg_rewind since #294) to act.
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
	"github.com/cagriekin/pg-ha-agent/internal/kubecfg"
	"github.com/cagriekin/pg-ha-agent/internal/logging"
	"github.com/cagriekin/pg-ha-agent/internal/mechanism"
	"github.com/cagriekin/pg-ha-agent/internal/observe"
	"github.com/cagriekin/pg-ha-agent/internal/pg"
	"github.com/cagriekin/pg-ha-agent/internal/pgbackrest"
	"github.com/cagriekin/pg-ha-agent/internal/pgconf"
	"github.com/cagriekin/pg-ha-agent/internal/podname"
	"github.com/cagriekin/pg-ha-agent/internal/process"
	"github.com/cagriekin/pg-ha-agent/internal/reconcile"
)

const (
	metricsAddr = ":9200"
)

func main() {
	log := logging.New(os.Stdout)
	// Package-level slog calls (internal/dcs's should-never-happen election error, the
	// mechanism's reclone-cleanup warning) would otherwise go to slog's own default handler --
	// stderr, in a different format from every other line the agent emits, which is exactly
	// how a message that matters gets missed. One line makes them all the agent's format.
	slog.SetDefault(log)

	// One-shot subcommand: provision etcd RBAC and enable auth. The bundled etcd
	// image is distroless (no shell), so this runs in the pg-ha image via the etcd
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
		// #317: which apiserver route this agent took. Without it a denied-egress
		// hang and a misrouted kubeconfig produce the same dial timeout in the log.
		"apiserver", kubecfg.Source(),
		"reconcileInterval", cfg.ReconcileInterval,
		"leaseDuration", cfg.LeaseDuration,
		"renewDeadline", cfg.RenewDeadline,
		"retryPeriod", cfg.RetryPeriod,
		"nodeCount", cfg.NodeCount,
		"headlessService", cfg.HeadlessService,
		"masterService", cfg.MasterService,
		"markerName", cfg.MarkerName,
		"cascadeReplication", cfg.CascadeReplication,
		"syncReplicationSlots", cfg.SyncReplicationSlots,
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
	// repmgrModulePath is where repmgr.so would be if this image shipped it. A field
	// rather than a call so the #293 presence check is testable without a real install.
	repmgrModulePath string
	base             string    // StatefulSet name (pod name without the ordinal)
	bootAt           time.Time // agent start; the cold-boot grace fallback for PeersPending is measured from here
	// peersSeen latches which peers have been SQL-reachable at least once this
	// lifetime. Once all have, the cold-boot wait never applies again -- so a
	// steady-state failover is not delayed by a recent agent/pod restart.
	peersSeen map[string]bool
	// rewindFailures counts CONSECUTIVE non-divergence pg_rewind failures against
	// rewindFailureTarget, so a persistently unrewindable node still recovers (#298 review).
	// Classification is fail-safe -- anything that is not provably divergence is retried
	// rather than escalated -- which on its own turns a permanent, non-divergent refusal into
	// a node that never rejoins. This counter is the other half: retry the cheap thing a few
	// times, then pay for the expensive one that always works.
	rewindFailures      int
	rewindFailureTarget string
	// followUpstream is the upstream this standby is currently configured to follow, so
	// the Follow action (write primary_conninfo + standby.signal, then SIGHUP) runs only
	// when the upstream actually changes, not every tick. Reset on any non-Follow action
	// except Wait/NoOp, which do not change this node's standby identity.
	followUpstream string

	// dbNameReloadPending is set when a #308 primary_conninfo dbname patch was written
	// to disk but the follow-up postmaster reload failed, and cleared once a reload
	// succeeds. EnsurePrimaryConninfoDBName's own changed=true/false only reports
	// whether THIS call wrote a change to the FILE -- once written, every later call
	// reports changed=false regardless of whether the reload ever actually took, so
	// that signal alone cannot drive a retry. This flag is what does: the Follow case
	// reloads whenever changed is true OR this is still true, so a failed reload is
	// retried on the next tick even though the file itself needs no further change.
	dbNameReloadPending bool

	// lastSyncStandbySlots caches the comma-joined slot list synchronized_standby_slots
	// was last successfully set to (#308), so a primary tick with an unchanged standby
	// set skips the ALTER SYSTEM + reload rather than repeating it every 5s. nil means
	// "not yet reconciled this primary term" -- distinct from a pointer to "", which
	// means "reconciled, and the live value really is empty". A bare string sentinel
	// would conflate the two: a freshly-promoted primary with no active standbys yet
	// would compute desired="" on its first tick, matching an unset zero-value "" and
	// wrongly skipping the ALTER SYSTEM -- leaving whatever synchronized_standby_slots
	// this node inherited (from being cloned from a prior primary's postgresql.auto.conf,
	// or from its own previous primary term) in place, possibly naming a slot that no
	// longer exists and permanently blocking logical decoding. Reset to nil on every
	// demote (see the OnLost handling) so the next term this node is primary again
	// starts with an unconditional first reconcile, not a stale cache from before.
	lastSyncStandbySlots *string

	// standbyNoReceiverTicks counts consecutive ticks on which this node was a running standby
	// with no walreceiver. Feeds reconcile.Observation.StandbyStalled (#288 review): a diverged
	// standby that can never converge must be rejoined, but a routine reconnect -- or an upstream
	// that is briefly down -- looks identical for a tick or two, so the signal has to persist.
	standbyNoReceiverTicks int

	// standbyLastProgressLSN is the furthest WAL position seen while counting receiver-less
	// ticks, so observeStandbyStall can tell a node that is still MAKING PROGRESS from a wedged
	// one (#288 review). Zero value = nothing observed yet.
	standbyLastProgressLSN pg.LSN

	// initdbMarkerWaitOverride shortens the post-initdb wait for the postmaster to accept SQL.
	// Zero means initdbMarkerWait; only tests set it.
	initdbMarkerWaitOverride time.Duration

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

	// Transition latches for the two tamper/maintenance alarms (#298 security review),
	// touched only from the single reconcile goroutine so they need no synchronisation.
	// They keep the Error/Warn log to the moment the state flips rather than every tick;
	// the metrics counter/gauge carries the steady-state signal for alerting.
	pausedLatched       bool
	markerTamperLatched bool

	// lastMarker is the most recent SUCCESSFUL marker read, carried forward when a later
	// read fails (#298 review). Same single-goroutine ownership as the latches above.
	//
	// The zero Marker is not a safe substitute for "could not read it": Paused=false,
	// Present=false and SwitchoverTarget="" are all legitimate VALUES, so a five-second
	// apiserver hiccup used to read as "nobody paused this cluster and no highwater was
	// ever recorded" -- which un-paused a maintenance window for one tick (Decide's pause
	// interlock is the ONLY thing keeping the reconcile loop from starting a postmaster
	// under a restore Job that is mid-rewrite of PGDATA) and disarmed the #125 highwater
	// guard at the same time. A stale marker is strictly better: it errs towards staying
	// paused and towards keeping the highwater armed, and it is re-read every tick.
	lastMarker   k8s.Marker
	lastMarkerOK bool

	// fenceGen counts COMPLETED demotes -- OnLost's fence and every planned step-down. It
	// exists to sequence tick()'s servingRW derivation against them (#298 review).
	//
	// tick() samples the role with observe(), which is multi-second and network-bound, and
	// then stores the derived value OUTSIDE opMu, while OnLost demotes under opMu and stores
	// false. So a tick whose observe() saw a still-read-write postmaster could land its
	// Store(true) AFTER the fence had already cleared the latch, resurrecting a stale "this
	// node is a writer". That was benign while the latch only gated the fence itself (the next
	// tick re-derives it); it stopped being benign once SafeToRelease made the latch gate the
	// LOCK RELEASE too -- a spurious true vetoes a release that was safe, so the peer waits out
	// leaseDuration instead of milliseconds and the operator reads "may still be a read-write
	// primary" about a fence that completed cleanly.
	fenceGen atomic.Uint64

	// Server-TLS verification state (#335), same single-goroutine ownership as the latches
	// above. tlsCheckedAt throttles the probe to tlsVerifyInterval rather than running it on
	// every tick, and tlsInactiveLatched keeps the Error to the transition.
	tlsCheckedAt       time.Time
	tlsInactiveLatched bool

	// durableTLNextAt throttles the restartpoint that keeps a standby's control-file timeline
	// current (#298). Same shape as tlsCheckedAt, but stored as the NEXT due time rather than
	// the last check, so a restartpoint that did not advance the timeline can back itself off
	// (durableTLNoAdvanceInterval) without a second field. The condition it repairs changes
	// only when this node follows a new timeline, and the steady-state check is one SQL round
	// trip.
	durableTLNextAt time.Time

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
	mech := newMechanism(cfg, pgBindir)
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
		repmgrModulePath: filepath.Join(cfg.PGLibdir(), repmgrPreloadLib+".so"),
		base:             podname.Base(cfg.PodName),
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

	// #293 preload preflight, deliberately BEFORE leader election. It strips repmgr from
	// PGDATA's shared_preload_libraries under native, then refuses to continue if any
	// configuration still asks for a shared library this image does not ship.
	//
	// The refusal is fatal because it is not a degraded state the reconcile loop can work
	// around: every postmaster start will fail, forever, and letting the loop proceed would
	// bury the one message that explains why under PostgreSQL's own `could not access file
	// "repmgr"`. Exiting puts the actionable text in the agent's log and the pod's restart
	// reason instead -- the same reasoning as the control-API failure above.
	//
	// And it runs HERE, not after boot(), precisely so os.Exit cannot strand the lease. Past
	// a.dcs.Run this node can hold leadership -- boot() legitimately takes minutes when it
	// clones -- and exiting from there skips the only release path (the tick loop's ctx.Done
	// branch), so healthy peers would wait out the full LeaseDuration before promoting, once
	// per CrashLoopBackOff restart (#293 review). Nothing here needs leadership: it is local
	// file surgery on PGDATA plus a stat.
	if err := a.preflightPreload(); err != nil {
		a.log.Error("preload preflight", "err", err)
		os.Exit(1)
	}
	// Same placement, same reasoning: a data directory whose postgresql.auto.conf still carries
	// recovery GUCs is one this agent cannot steer, and no amount of reconciling fixes it
	// (#294 review).
	if err := a.migrateForeignRecoveryConfig(); err != nil {
		a.log.Error("recovery-config preflight", "err", err)
		os.Exit(1)
	}

	// Leadership: OnLost demotes synchronously (the fence-ordering guarantee)
	// before the lock can be re-acquired by anyone.
	//
	// dcsDone closes when Run has finished unwinding, which is what actually frees the
	// lease: the etcd backend revokes its session lease in a deferred releaseSession and
	// the K8s backend empties the Lease after OnStoppedLeading. The shutdown path below
	// waits on it before closing the client (#298 review).
	dcsDone := make(chan struct{})
	go func() {
		defer close(dcsDone)
		a.dcs.Run(ctx, a.cfg.PodName, dcs.Callbacks{
			OnAcquired: func(context.Context) {
				a.metr.SetLeader(true)
				a.log.Info("acquired leadership")
			},
			// Involuntary loss only (never Release or shutdown): the chart's
			// PGHAAgentLeaseRenewFailing alert rates this counter, and until it was wired
			// here nothing incremented it, so the alert could never fire (#298 review).
			OnRenewFailure: func() {
				a.metr.IncRenewFailure()
				a.log.Warn("lease lost involuntarily: renew/keepalive lapsed (DCS unreachable?)")
			},
			// Consulted by the backend after OnLost returns, before it frees the lock
			// (#298 review). servingRW is the agent's own answer to "might a read-write
			// postmaster still be up here": OnLost clears it only on a COMPLETED demote and
			// deliberately keeps it set on a failed one, which is exactly the state in which
			// an immediate handoff would let a peer promote beside a live writer.
			//
			// The cost of holding is NOT just "the LeaseDuration/TTL the peer would have waited
			// anyway", and it is worth stating plainly: the hold lasts as long as this node
			// cannot prove the writer is dead. On the K8s backend client-go lets the RECORDED
			// holder re-acquire, so this node wins the same Lease back one retryPeriod later (2s
			// on chart defaults, well inside the 15s leaseDuration) and resumes renewing; on etcd
			// the orphaned key expires at TTL and this node's queued candidate -- lowest create
			// revision -- wins it back. So a postmaster genuinely stuck in uninterruptible sleep
			// keeps leadership until the PROCESS dies (tick clears the latch on
			// !LocalProcessAlive) or an operator intervenes. That is the deliberate trade -- no
			// second writer, at the price of no failover -- and the warning each backend logs
			// plus pg_ha_agent_fences_total / the renew-failure counter are the operator's signal.
			SafeToRelease: func() bool { return !a.servingRW.Load() },
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
				derr := a.sup.Demote(dctx, true)
				// stopProvedDead, not `derr != nil` (#298 review). Stop returns
				// context.DeadlineExceeded on the arm where the deadline expired, the SIGKILL
				// landed and the child was REAPED -- an outcome that proves there is no writer
				// left. A SIGQUIT'd postmaster with a large shutdown checkpoint routinely
				// outlives RenewDeadline (10s on chart defaults), so on the ordinary
				// apiserver-partition fence this branch fired for a fence that had completed
				// cleanly: servingRW stayed set, SafeToRelease vetoed the release, the peer
				// waited out the full LeaseDuration instead of taking an immediate handoff, and
				// the operator read "may still be a read-write primary" mid-incident about a
				// node with nothing running on it.
				if !a.stopProvedDead(derr) {
					// KEEP the latch set (#298 review). Clearing it on a FAILED demote is the one
					// direction tick()'s own rule forbids -- "cleared only on positive evidence",
					// fail-safe is to demote on uncertainty -- and a failed Stop is precisely the
					// case where a read-write postmaster may still be up. With the latch cleared,
					// the next lease loss takes the "not read-write; no fence needed" branch and
					// skips the fence on the one node that most needs it. The tick loop re-derives
					// it from the observed role either way, so holding it costs nothing.
					a.log.Error("fence demote failed; keeping this node marked read-write so a later lease loss still fences", "err", derr)
					return
				}
				if derr != nil {
					a.log.Warn("fence: the postmaster did not exit before the deadline and was killed; it is reaped, so the fence is complete", "err", derr)
				}
				// Through the shared helper, not a hand-inlined copy of its two lines (#298
				// review). The bump-before-clear ordering it establishes is subtle and
				// load-bearing (deriveServingRW's gate is a check-then-act pair, so the
				// generation must never become visible later than the false it announces) --
				// having it written out twice is exactly how one of the two ends up reordered
				// by a later edit. This is a completed demote like the others; only the reason
				// for it differs.
				a.clearServingRWForPlannedStepDown()
			},
		})
	}()

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
			derr := a.sup.Demote(sctx, false) // graceful (fast) on planned shutdown
			// stopProvedDead, not `derr == nil` (#298 review). A Fast/SIGINT shutdown of a large
			// busy database legitimately outruns the 30s budget, and Stop then SIGKILLs and REAPS
			// it -- returning context.DeadlineExceeded for an outcome in which no writer survives.
			// Reading that as "did not complete" left servingRW set, so SafeToRelease vetoed the
			// release and the K8s backend kept the Lease: every peer then waited out the full
			// LeaseDuration (15s on chart defaults) of write outage, which is exactly the outage
			// the dcsDone ordering below exists to eliminate, defeated on the ordinary
			// slow-checkpoint shutdown. There is no next tick here to re-derive the latch.
			if a.stopProvedDead(derr) {
				if derr != nil {
					a.log.Warn("planned shutdown: the postmaster did not exit before the stop deadline and was killed; it is reaped, so the lease can be released immediately", "err", derr)
				}
				// A COMPLETED demote is positive evidence this node is no longer a writer, so
				// disarm the fence latch here exactly as the switchover and ReleaseLease paths
				// do (#298 review). Without it every planned termination of a read-write
				// primary ran OnLost's fence branch on the way out: pg_ha_agent_fences_total
				// incremented and the log said "lost leadership; demoting (fence)" for a node
				// that had shut down cleanly, so a maintenance window with a couple of primary
				// restarts paged PGHAAgentFlapping ("suggests instability"). It also removes the
				// worse variant, where the queued OnLost won opMu and re-stopped the postmaster
				// with Immediate/SIGQUIT instead of the intended Fast, forcing crash recovery on
				// the next start.
				a.clearServingRWForPlannedStepDown()
			} else {
				a.log.Warn("planned shutdown: demote did not complete; leaving this node marked read-write", "err", derr)
			}
			a.opMu.Unlock()
			cancel()
			// WAIT for the election goroutine to unwind before closing the client (#298
			// review). Releasing the lease is the last thing Run does -- the etcd backend
			// revokes its session lease in a deferred releaseSession, the K8s backend empties
			// the Lease after OnStoppedLeading -- and Close() tearing the gRPC connection down
			// underneath it made that revoke fail silently ("client connection is closing"), so
			// the election key survived for the full session TTL. Every planned primary
			// termination then cost a peer a full LeaseDuration (15s on chart defaults) of write
			// outage instead of an immediate handoff. Bounded, because a shutdown must not hang
			// on an unreachable DCS: past the grace we close anyway and the lease expires on TTL,
			// which is exactly the old behaviour.
			select {
			case <-dcsDone:
			case <-time.After(a.cfg.LeaseDuration):
				a.log.Warn("shutdown: leader election did not unwind in time; releasing the lease is left to TTL expiry")
			}
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

// boot writes the node-local files the agent owns -- .pgpass, the managed
// postgresql.conf fragment, pg_hba.conf -- and starts Postgres if the data dir is already
// initialized in a role that is safe to start regardless of holdership. initdb and clone of
// a fresh node belong to the reconcile loop (the lease decides which).
func (a *agent) boot(ctx context.Context) error {
	nid := mechanism.NodeIdentity{
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
	// Same treatment for an interrupted bootstrap (#288 review). A clone and an initdb fail the
	// same way -- PG_VERSION present, the directory unusable -- and neither is reachable through
	// the ordinary has-data paths.
	a.discardTornInitdb(ctx)
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
	// The HasData gate was mechanism-specific until #294 (Repmgr.GenerateConfig wrote
	// /etc/repmgr/repmgr.conf, outside PGDATA, and had to run unconditionally). With Native the
	// only implementation, the config this writes lives INSIDE PGDATA, so the gate is simply
	// correct: there is nothing to generate before the directory exists.
	if process.HasData(a.cfg.PGDATA) {
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
	// SUPERUSER-exposure risk). The agent is the
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
	cd, err := a.readControlData(ctx)
	if err != nil {
		a.log.Warn("boot: read pg_controldata; deferring start to reconcile", "err", err)
		return nil
	}
	if cd.InRecovery {
		// #308: patch dbname into primary_conninfo unconditionally on every cold start of
		// a standby, BEFORE the tick loop's own Follow-path patch (main.go's `case
		// reconcile.Follow`) ever runs. That path only patches after actually invoking
		// `repmgr standby follow`, which it skips whenever the node is already streaming
		// from its target on the very first tick -- true for a just-cloned standby, since
		// cloning already establishes streaming. Patching here instead of relying on that
		// tick is what makes a fresh install deterministic rather than a race against how
		// fast the standby's walreceiver reports itself as streaming.
		if changed, err := a.ensurePrimaryConninfoDBName(); err != nil {
			a.log.Warn("boot: ensure dbname in primary_conninfo", "err", err)
		} else if changed {
			a.log.Info("boot: patched dbname into primary_conninfo")
		}
		// RE-ASSERT standby.signal before starting (#298 review). InRecovery is derived from
		// pg_controldata ALONE, never from the presence of the file, so the two can disagree --
		// and every disagreement was resolved in the unsafe direction here. RejoinForceRewind
		// moves standby.signal aside for pg_rewind's crash-recovery step and restores it only
		// via an in-process defer: a container kill / OOM / node reboot inside that window
		// (pg_rewind legitimately runs for minutes) leaves a directory whose control file says
		// "in archive recovery" with no signal file. This branch then started it read-write --
		// crash recovery on the LIVE PRIMARY'S TIMELINE, before any lease or marker check had
		// run, i.e. a second writer. Creating the file first is idempotent, costs one syscall,
		// and makes the on-disk role the authority it is meant to be.
		if err := process.SetRecoverySignal(a.cfg.PGDATA); err != nil {
			return err
		}
		return a.sup.Start(ctx)
	}
	a.log.Info("boot: on-disk primary state; deferring start until reconcile confirms holdership + highwater", "state", cd.State)
	return nil
}

func (a *agent) tick(ctx context.Context) {
	a.metr.Beat()
	// Sampled BEFORE observe() so deriveServingRW can tell whether a demote completed while
	// this tick was looking (#298 review; see the fenceGen field).
	gen := a.fenceGen.Load()
	obs := a.observe(ctx)
	// Track read-write role for the OnLost fence (a standby needs no fence). This is
	// the pure writer-state (NOT lease-gated: the lease flips to lost before OnLost
	// demotes, so gating on it could skip a real fence). Postgres stays read-write
	// until demoted, so a tick during the loss window still sees RW -> fence fires.
	// The Promote act also sets it synchronously, closing the promote->next-tick gap.
	//
	// Cleared only on positive evidence (#298 review). Running is SQL reachability,
	// not process liveness: a wedged/overloaded read-write postmaster fails the probe
	// while still being a writer, and clearing the latch on that uncertainty made
	// OnLost skip the fence ("not read-write; no fence needed") for exactly the node
	// that most needs it -- a second writer once it thaws. Fail-safe is to demote on
	// uncertainty: hold the last known value until the probe answers again or the
	// postmaster process is actually gone.
	a.deriveServingRW(gen, obs)
	// Verify server TLS from the postmaster itself while it is answering (#335). Gated on
	// Running because `SHOW ssl` needs a connection: on a cloning or recovering node the probe
	// would fail for a reason that has nothing to do with TLS, and the throttle would then
	// spend its interval on a question that could not be asked.
	if obs.Local.Running {
		a.verifyTLSActive(ctx)
		// Only a standby has a restartpoint to force, and only one that is actually streaming
		// has a newer timeline to record (#298).
		if obs.Local.InRecovery {
			a.ensureDurableTimeline(ctx)
		}
	}
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
	// err == nil as well as the action (#288 review, round 2). The gate is on what the tick
	// DECIDED, and a Promote that failed leaves this node a standby: topologyTick would then read
	// an empty pg_stat_replication, compute expected from the live pod set and publish
	// replicas_streaming=0 / replicas_expected=N -- plus a "live peers are not streaming from
	// this primary" warning -- from a node that is not the primary, pointing an operator at the
	// wrong pod mid-failover. rehashManagedUsersOnce is gated the same way for the same reason:
	// it is a primary-only convergence step.
	if err == nil && (dec.Action == reconcile.Promote || dec.Action == reconcile.StayPrimary) {
		a.rehashManagedUsersOnce(ctx)
		// Same reason, same place (#288 review, second pass). topologyTick is PURELY
		// observational -- gauges only -- and it makes an apiserver pod LIST plus a psql query,
		// both hangable. Inside act() it held opMu for its own fenceBudget() ON TOP of the
		// branch's wctx, doubling the worst-case read-write hold to 2x the soft-fence window on
		// every primary tick (10s vs a 5s budget and a 15s /healthz staleness threshold on chart
		// defaults) -- so a hung LIST could starve the lost-leadership fence it must never
		// delay. Out here it cannot, and its extra LIST stays off the promote critical path.
		a.topologyTick(ctx)
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
	// An ADOPTED restore has no claim left to make (#288 review, round 2). Once this volume's
	// history became the cluster's -- this node promoted on it, and the highwater marker
	// records that timeline -- later elections are decided by position alone, exactly as they
	// were before any restore happened. The record itself stays on disk: it is the only
	// provenance for where this PGDATA came from, and GET /v1/status still reports it.
	if rec.AdoptedAt != "" {
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

// maxRestoreClockSkew bounds how far ahead of now a peer's gossiped restore
// timestamp may sit before it is rejected as forged/corrupt (#298 security review).
// A restore's FinishedAt is always in the past by the time a peer gossips and this
// node reads it; a far-future value (e.g. "9999-01-01T00:00:00Z") would sort
// lexically above every real timestamp in reconcile.restoredAfter and hand the lease
// to a node with LESS WAL, discarding committed history. The window is generous
// enough to absorb any realistic NTP skew between pods.
const maxRestoreClockSkew = time.Hour

// validRestoreProvenance returns rst when it is a parseable RFC3339 timestamp no
// further ahead than maxRestoreClockSkew, else "" (no provenance). Empty stays
// empty. This guards only gossip-sourced (peer) values, which a namespace writer can
// forge onto a pod annotation; the local record comes from this node's own volume and
// is trusted. Dropping to "" is the safe direction -- it can only lower a peer's rank,
// never raise it.
func validRestoreProvenance(rst string, now time.Time) string {
	if rst == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, rst)
	if err != nil || t.After(now.Add(maxRestoreClockSkew)) {
		return ""
	}
	return rst
}

// validPeerName reports whether name is one of THIS cluster's StatefulSet pods
// (<base>-<ordinal>), the only strings safe to build a libpq conninfo host from
// (#298 security review). dcs.Leader() and the switchover-target annotation are
// DCS/ConfigMap values a namespace writer can forge; an unvalidated value such as
// "evil.svc port=5432 sslmode=disable x" would inject libpq conninfo keywords
// (fqdn appends ".<headless>") and make this node dial an attacker host with the
// replication PGPASSWORD set. Peer names the agent derives itself (a.base-<n>) are
// already safe; this gates the ones that arrive over the wire.
func (a *agent) validPeerName(name string) bool {
	if name == "" {
		return false
	}
	ord, ok := podname.Ordinal(name)
	return ok && name == fmt.Sprintf("%s-%d", a.base, ord)
}

// markerTimelineSaneDelta is how many timelines above the highest OBSERVED node
// timeline the marker highwater may sit before it is treated as implausible (#298
// security review). A timeline advances by one per promotion, so even a long cold
// boot leaves the marker at most a handful ahead of what nodes report; a gap of
// hundreds cannot be a real cluster and is the signature of a forged/corrupt marker.
// The threshold is deliberately generous so ordinary cold-boot skew never trips it.
const markerTimelineSaneDelta pg.Timeline = 100

// markerTamperSuspected reports whether the primary marker looks forged or corrupt:
// unparseable, or a timeline implausibly far above every node's observed timeline.
// Detection only -- unsafeToServe still fails closed on these -- so this never
// changes a failover decision; it only makes a forced write-outage diagnosable.
func markerTamperSuspected(o reconcile.Observation) (bool, string) {
	if !o.Marker.Present {
		return false, ""
	}
	if o.Marker.Malformed {
		return true, "marker timeline is unparseable"
	}
	maxTL := pg.Timeline(0)
	haveObs := false
	if o.Local.TimelineOK {
		maxTL, haveObs = o.Local.Timeline, true
	}
	for _, p := range o.Peers {
		if p.TimelineOK && (!haveObs || p.Timeline > maxTL) {
			maxTL, haveObs = p.Timeline, true
		}
	}
	// Only judge against reality when SOME node's timeline is readable; with none we
	// cannot tell a legitimate cold-boot-ahead marker from a forged one. The subtraction
	// is guarded by the `>` so it cannot underflow the unsigned Timeline.
	if haveObs && o.Marker.Timeline > maxTL && o.Marker.Timeline-maxTL > markerTimelineSaneDelta {
		return true, "marker timeline is implausibly far above every observed node"
	}
	return false, ""
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
		if cd, err := a.readControlData(ctx); err != nil {
			a.log.Warn("read pg_controldata", "err", err)
		} else {
			ls.Timeline, ls.TimelineOK = cd.Timeline, cd.TimelineOK
			ls.InRecovery = cd.InRecovery
			ls.LSN, ls.LSNOK = cd.LSN, cd.LSNOK // checkpoint LSN: position for gossip ranking while stopped
		}
	}
	// The Lease holderIdentity is DCS state a namespace writer can forge, and it flows
	// into a libpq conninfo host for a follower (Follow -> peerMechConn -> conninfo).
	// Reject any value that is not one of THIS cluster's pod names before it can inject
	// conninfo keywords or redirect replication to an attacker host (#298 security
	// review). An unknown leader is safer than a forged one: the standby waits rather
	// than dialing it.
	leader := a.dcs.Leader()
	if leader != "" && !a.validPeerName(leader) {
		a.log.Error("ignoring lease holder identity that is not a valid cluster pod name (possible tampering)", "holder", leader)
		leader = ""
	}
	o := reconcile.Observation{
		HoldLease:      a.dcs.IsLeader(),
		LeaderIdentity: leader,
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
		// Bounded like topologyTick/slotsTick (#298 review): the REST config sets no
		// client Timeout, so an unbounded apiserver call on the reconcile goroutine can
		// hang on a blackholed connection, stopping metr.Beat() until /healthz goes
		// stale and the kubelet kills PID 1 -- and the postmaster it supervises --
		// exactly when the apiserver is unreachable. Best-effort: a deadline miss means
		// no gossip this tick.
		gctx, gcancel := context.WithTimeout(ctx, a.fenceBudget())
		g, gerr := a.kube.ReadPeerStatuses(gctx, a.cfg.PodSelector, a.cfg.PodName)
		gcancel()
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
	// Peers are probed CONCURRENTLY (#298). Each probe is a psql connect bounded only by
	// PGCONNECT_TIMEOUT, so probing serially costs (unreachable peers) x (connect timeout)
	// inside the reconcile loop.
	// On a 5-node cluster that has just lost its network, that is ~40s of a tick whose
	// interval is 5s: /healthz goes stale at reconcileInterval*3, so the agent could be
	// liveness-killed for the crime of having dead peers -- the pathology that made every
	// peer-addressed command take an explicit connect timeout in the first place. The probes
	// are independent reads with no shared state, so the fan-out needs no coordination beyond
	// writing each result to its own slot.
	//
	// Results land by INDEX, not by append order, so o.Peers stays deterministic: the
	// promote-distance and role-label logic downstream reads this slice, and a peer order that
	// varied with which probe returned first would make two identical clusters disagree.
	type peerProbe struct {
		ps  reconcile.PeerState
		set bool
	}
	// max(0, NodeCount): config.Load only requires REPMGR_NODE_COUNT to PARSE, so a negative
	// value reaches here and `make` with a negative length PANICS -- which for PID 1 is a
	// crash-loop over the postmaster it supervises. Probe nothing instead (#298).
	probes := make([]peerProbe, max(0, a.cfg.NodeCount))
	var pwg sync.WaitGroup
	for i := 0; i < a.cfg.NodeCount; i++ {
		name := a.base + "-" + strconv.Itoa(i)
		if name == a.cfg.PodName {
			continue
		}
		pwg.Add(1)
		go func(i int, name string) {
			defer pwg.Done()
			ns := a.prober.Probe(ctx, a.peerConn(name))
			probes[i] = peerProbe{set: true, ps: reconcile.PeerState{
				Name: name, Reachable: ns.Reachable, Role: ns.Role,
				Timeline: ns.Timeline, TimelineOK: ns.TimelineOK, LSN: ns.WriteLSN, LSNOK: ns.LSNOK,
			}}
		}(i, name)
	}
	pwg.Wait()
	for _, pr := range probes {
		if !pr.set {
			continue
		}
		name, ps := pr.ps.Name, pr.ps
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
			ps.RestoredAt = validRestoreProvenance(g.RestoredAt, time.Now())
		}
		o.Peers = append(o.Peers, ps)
	}
	// Bounded for the same reason as the gossip read above (#298 review); the marker
	// is re-read every tick, so a deadline miss self-heals.
	mctx, mcancel := context.WithTimeout(ctx, a.fenceBudget())
	m, err := a.kube.ReadMarker(mctx, a.cfg.MarkerName)
	mcancel()
	if err != nil {
		// FAIL CLOSED, do not fall through to the zero Marker (#298 review) -- see the
		// lastMarker field comment for what the zero value silently asserts. A genuinely
		// ABSENT marker is not this path: ReadMarker returns Marker{Present:false} with a
		// nil error for NotFound, so deleting the ConfigMap (a documented recovery step)
		// still reaches Decide as the real observation it is.
		if a.lastMarkerOK {
			a.log.Warn("read marker failed; reusing the last successful read for this tick", "err", err)
			m = a.lastMarker
		} else {
			a.log.Warn("read marker failed and no earlier read is available; this tick will take no action", "err", err)
			o.MarkerUnreadable = true
		}
	} else {
		a.lastMarker, a.lastMarkerOK = m, true
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
	// Cross-check the marker highwater against observed reality (#298 security review).
	// The marker is a ConfigMap a namespace writer can forge, and unsafeToServe trusts
	// its timeline: a wildly-high (or unparseable) value trips the guard on every node
	// and freezes all promotions -- a write outage. We keep failing closed there (an
	// untrusted highwater is not safe to serve on), but a marker that cannot possibly be
	// real is surfaced loudly and counted so the outage reads as tampering, not silence.
	if suspect, why := markerTamperSuspected(o); suspect {
		a.metr.IncMarkerTamper()
		if !a.markerTamperLatched {
			a.log.Error("primary marker looks tampered or corrupt; automatic promotion may be frozen until it is corrected (possible tampering)", "reason", why, "markerTimeline", uint32(o.Marker.Timeline))
			a.markerTamperLatched = true
		}
	} else {
		a.markerTamperLatched = false
	}
	// This pod's name, compared against Marker.Primary so an empty-data lease holder
	// can recognize it is not the recorded primary and release the lease (#186).
	o.LocalNode = a.cfg.PodName
	// Pause-gated, exactly like LocalStuck below (#298 review). Both are stateful,
	// time-based signals computed here rather than in the pure Decide, and both feed a
	// DESTRUCTIVE branch -- LocalStuck a self-health ReleaseLease, StandbyStalled a
	// RejoinForward that stops postgres, runs pg_rewind, and on divergence leaves an
	// unreaped .diverged.<ts> copy. Only one of the two was suppressed while paused.
	//
	// A pause is expected to be long (the chart ships PGHAAgentPausedTooLong for HOUR-long
	// ones), and it is exactly when a standby legitimately loses its walreceiver: the
	// operator restarts the primary, drains its node, or runs the documented in-place PITR.
	// The counter climbed past standbyStallTicks during all of that -- Decide returns NoOp
	// while paused, so nothing happened yet -- and then the settling window the counter
	// exists to provide was already spent: on the FIRST unpaused tick `newer != nil &&
	// StandbyStalled` held and Decide short-circuited to RejoinForward before the Follow
	// branch that would have reattached the standby in seconds. Treat a resume as a start,
	// not as a continuation.
	if m.Paused {
		a.standbyNoReceiverTicks = 0
		a.standbyLastProgressLSN = pg.LSN{}
	} else {
		a.observeStandbyStall(ctx, &o)
	}
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
	// Maintenance mode is an in-namespace kill switch for automatic failover, so make
	// entering/leaving it non-silent (#298 security review): log on the transition (the
	// gauge above carries steady state). The lease-loss fence in dcs.OnLost is NOT gated
	// by pause, so a paused primary that loses its lease still demotes -- pause suspends
	// automatic promotion/self-health, not the split-brain fence.
	if m.Paused && !a.pausedLatched {
		a.log.Warn("maintenance mode active: automatic promotion/demotion/self-health suspended (the lease-loss fence still applies)")
		a.pausedLatched = true
	} else if !m.Paused && a.pausedLatched {
		a.log.Info("maintenance mode cleared: automatic failover resumed")
		a.pausedLatched = false
	}
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
		a.standbyLastProgressLSN = pg.LSN{}
		o.StandbyStalled = false
		return
	}
	// An absent walreceiver is NOT the same as a stuck node, and conflating them rewinds healthy
	// standbys (#288 review). A standby replaying from restore_command -- pgbackrest archive-get,
	// which the chart configures whenever pgbackrest is enabled -- has NO pg_stat_wal_receiver
	// row at all while it works through archived WAL, and its local timeline is still the
	// pre-fork one, so `newer != nil && StandbyStalled` both held and Decide escalated to
	// RejoinForward: postgres stopped, pg_rewind or a full ReclonePreserving on a node that was
	// catching up correctly and would have converged on its own. Archive catch-up after a
	// failover is exactly when this fires, and it is not native-gated, so it reached the default
	// repmgr mechanism too.
	//
	// Replay/receive position is the discriminator: archive recovery advances it, a wedged
	// standby does not. Only ticks with NO forward progress count toward the stall.
	if recv, ok, err := a.prober.StandbyReceiveLSN(ctx, a.selfConn()); err == nil && ok {
		advanced := recv.Greater(a.standbyLastProgressLSN)
		a.standbyLastProgressLSN = recv
		if advanced {
			// Progress without a walreceiver: archive recovery (or a very fresh restart). Hold
			// the counter at zero -- this node needs no rejoin, it needs time.
			a.standbyNoReceiverTicks = 0
			o.StandbyStalled = false
			return
		}
	}
	a.standbyNoReceiverTicks++
	// A stall only escalates once this node has actually been pointed at an upstream (#288
	// review). A standby freshly repointed at a just-promoted primary looks identical to a
	// diverged one: Follow writes primary_conninfo and reloads, and the walreceiver may
	// legitimately take a while to attach -- the new primary's first StayPrimary tick has not
	// necessarily created the slot yet, sender slots can be momentarily exhausted, DNS may still
	// be settling. Firing there stops, rewinds and possibly re-clones a perfectly healthy
	// standby.
	//
	// The latch is a PRECONDITION, not a delay: latchFollow runs at the end of the same Follow
	// that writes primary_conninfo, so it is set on the first repoint tick. What buys the
	// settling time is stallTicks plus the no-progress requirement above -- and latchFollow
	// ZEROES the counter, so the window genuinely restarts at each repoint rather than carrying
	// a count earned while the old upstream was down (#294 review).
	o.StandbyStalled = a.standbyNoReceiverTicks >= standbyStallTicks && a.followUpstream != ""
	if o.StandbyStalled {
		a.log.Warn("standby has had no walreceiver for several ticks; eligible for rejoin if a peer is on a newer timeline (#288)",
			"ticks", a.standbyNoReceiverTicks)
	}
}

// standbyStallTicks is how many consecutive receiver-less, NO-PROGRESS ticks make a standby
// "stalled". 36 at the default 5s interval is ~3 minutes.
//
// Raised from six (~30s) after review round 3. The no-progress rule in observeStandbyStall
// distinguishes archive recovery from a wedge by watching the replay position ADVANCE, and that
// leaves one gap: a single restore_command fetch -- `pgbackrest archive-get` pulling one 16MB
// segment from a throttled or degraded S3 repository -- can itself take longer than the window,
// freezing both LSNs with no walreceiver row. Post-failover archive catch-up is exactly when
// this node also sees a peer on a newer timeline, so the escalation fired on a standby that was
// converging correctly, stopping postgres to pg_rewind or fully re-clone it. Not native-gated
// either: this reaches the default repmgr mechanism.
//
// The asymmetry is what sets the value. Escalating late costs a standby a few more minutes of
// being behind, while nothing is down and no client is affected; escalating wrongly destroys a
// healthy standby's data directory and can leave a .diverged.<ts> copy behind. So the window is
// sized well past any plausible single-segment fetch rather than tight enough to be prompt.
const standbyStallTicks = 36

// clearServingRWForPlannedStepDown drops the read-write latch on a step-down whose demote or
// stop is PROVEN to have ended with no writer left -- a planned one this agent performed, or
// the lost-leadership fence.
//
// dcs.OnLost fires for a voluntary Release() too -- k8sdcs and etcddcs both invoke it
// unconditionally, and only OnRenewFailure is filtered to involuntary loss -- and it gates
// solely on servingRW. Left set through a planned handoff, the release that follows counted
// a metr.IncFence() and logged "lost leadership; demoting (fence)": three switchovers or
// self-health handoffs in one maintenance window tripped the chart's PGHAAgentFlapping page
// (increase(pg_ha_agent_fences_total[15m]) > 2, "suggests instability"), and the log told
// whoever was reading it mid-incident that the node had been fenced when it had in fact
// stepped down on request (#298 review).
//
// Call this ONLY where stopProvedDead accepts the demote's or stop's outcome -- nil, or the
// deadline expiry whose SIGKILL was delivered AND reaped (#298 review; the precondition used to
// read "has just returned nil", which the reaped-kill callers in act() legitimately do not
// satisfy). That is the positive evidence tick() requires: clearing on anything weaker -- an
// unreachable SQL probe on a postmaster that is still alive -- disarms the fence for the one
// node that most needs it.
func (a *agent) clearServingRWForPlannedStepDown() {
	// Bumped BEFORE the clear, not after it (#298 review). deriveServingRW's gate is a
	// check-then-act pair, so the generation has to be visible no LATER than the false it
	// announces: with the bump second, a tick could pass its check (generation still old),
	// have the clear land, and then store its stale true over it. Bumping first makes every
	// interleaving end at false -- see the post-store re-check in deriveServingRW.
	a.fenceGen.Add(1) // a completed demote: invalidate any read-write observation in flight
	a.servingRW.Store(false)
}

// stopProvedDead reports whether a Demote/Stop error still amounts to positive evidence
// that the postmaster is gone (#298 review).
//
// ChildPostmaster.Stop returns ctx.Err() -- context.DeadlineExceeded -- on the arm where the
// deadline expired, SIGKILL was delivered AND the child was reaped: p.clear() has already run
// there, so the writer is provably dead. It returns a wrapped ctx.Err() on the genuinely bad
// arm (SIGKILL undeliverable to a process in uninterruptible sleep, "leaving it supervised"),
// where the handle is deliberately kept. err != nil therefore cannot distinguish the two, and
// Running() is what does -- exactly the test RestartLocal and the control-API restart already
// apply. Shared so the fence and shutdown paths cannot drift from them again.
//
// Any error that is NOT a deadline expiry (a signal that could not be sent, a nil process) is
// never treated as proof: those say nothing about whether the postmaster died.
func (a *agent) stopProvedDead(err error) bool {
	if err == nil {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) && !a.sup.Running()
}

// deriveServingRW applies tick()'s observed role to the fence latch, discarding an observation
// that a demote has overtaken.
//
// gen is fenceGen as it stood before observe() ran. Only the TRUE direction is gated: storing
// false can never resurrect a writer, and the two safe-direction cases (a standby, a dead
// process) must always be allowed through. A stale true is the only value that misleads both
// consumers of this latch -- OnLost's fence decision and SafeToRelease's release veto.
//
// The generation is checked TWICE, before and after the store (#298 review). One check ahead
// of it is a check-then-act pair, not an atomic decision: OnLost runs on its own goroutine and
// does not hold opMu at the moment it clears the latch, so a demote landing in the window
// between the check and the store slipped through and the stale true stood until the NEXT tick
// re-derived it -- a full reconcileInterval of SafeToRelease vetoing a release that was safe,
// which is the whole failure this gate exists to prevent. Both demote sites bump fenceGen
// before clearing servingRW, so a bump observed after the store means the false either has
// landed or is about to, and re-asserting it here is correct in both orders.
func (a *agent) deriveServingRW(gen uint64, obs reconcile.Observation) {
	// msg is the caller's, because the two checks report DIFFERENT events and an operator reading
	// the fence path must be able to tell them apart: the pre-store check discards a sample that
	// never reached the latch, the post-store one corrects a value that already did.
	overtaken := func(msg string) bool {
		if a.fenceGen.Load() == gen {
			return false
		}
		a.log.Info(msg)
		return true
	}
	switch {
	case obs.Local.Running:
		if obs.Local.InRecovery {
			a.servingRW.Store(false) // a standby: the safe direction is never gated
			return
		}
		// A demote completed while observe() was in flight, so this sample predates it. Leave
		// the latch where the demote put it; the next tick re-derives from scratch.
		if overtaken("discarding a read-write observation a demote overtook: this node was demoted while the tick was observing") {
			return
		}
		a.servingRW.Store(true)
		if overtaken("re-asserting a demote's read-only state over a read-write observation it overtook: the demote landed between this tick's generation check and its store") {
			a.servingRW.Store(false)
		}
	case !obs.LocalProcessAlive:
		a.servingRW.Store(false) // process gone: nothing left to fence
	}
}

func (a *agent) act(ctx context.Context, dec reconcile.Decision, obs reconcile.Observation) error {
	// Any action other than Follow changes (or ends) this node's standby identity, so
	// the next Follow must re-register + repoint.
	//
	// Wait and NoOp are exempt, because neither changes it (#298 review). Both are explicitly
	// "observe again, touch nothing" -- Decide's own reason for the reachable-standby Wait is
	// "standby but no known leader; KEEP the current upstream", and NoOp is maintenance pause --
	// so clearing the latch there erased a fact that was still true. Two things depended on it:
	//
	//   - releaseSlotOnFormerUpstream needs the former upstream's name to drop this node's slot
	//     there after a cascade re-home. One no-leader Wait tick mid-failover (routine: the
	//     lease is briefly empty after ReleaseOnCancel) blanked it, so the next Follow called
	//     that helper with "" and returned at its guard -- leaking an inactive, WAL-pinning slot
	//     on the old intermediate until max_slot_wal_keep_size invalidated it and paged someone.
	//     Nothing else reclaims it: the upstream's own pass deliberately keeps any slot whose
	//     ordinal has a live pod.
	//   - cascadeFollowTarget's #29 anti-thrash stickiness reads the latch, so it lost its
	//     hysteresis on every Wait tick.
	if dec.Action != reconcile.Follow && dec.Action != reconcile.Wait && dec.Action != reconcile.NoOp {
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
		// #308's cache goes too: this node has stopped being primary, and
		// synchronized_standby_slots is a primary-side reconcile.
		a.lastSyncStandbySlots = nil
	case reconcile.NoOp, reconcile.Wait:
		// Wait rides with NoOp (#298 review). Both are "observe again, touch nothing" -- Wait is
		// what Decide returns when there is no known leader, which the Follow-latch exemption
		// above already calls routine ("the lease is briefly empty after ReleaseOnCancel") --
		// and neither ends this node's slot holdings. Falling into the retract branch below
		// zeroed pg_ha_agent_replication_slots_{inactive,invalidated,max_retained_wal_bytes} on
		// EVERY Wait tick, so a standby sitting on an inactive, WAL-pinning slot through a
		// step-down cooldown or an apiserver/etcd partition resolved all three slot alerts and
		// restarted their `for:` clocks (5m/15m/1h) -- a blind window over the disk-fill hazard
		// #289 exists to catch, opened precisely when WAL accumulation is most likely.
		//
		// Pause is NOT a demotion (#294 review). Decide returns NoOp for maintenance mode, and
		// it must not reach the retract branch below: on a PAUSED PRIMARY, still serving and
		// still holding slots, ClearSlots() every tick silences all three slot alerts and
		// resets their `for:` clocks (5m/15m/1h) on every tick. A pause is the state
		// most likely to accumulate slot WAL, and the chart ships PGHAAgentPausedTooLong for
		// hour-long pauses, so that was a long blind window over a real hazard.
		//
		// The slot gauges are therefore LEFT STANDING. Nothing refreshes them while paused
		// (slotsTick runs from the primary/standby branches, which NoOp skips), so they hold
		// their last observed value -- truthful for a paused node, and the alternative is
		// silence. Topology is still retracted, for the reason the Follow branch gives: it is
		// the PRIMARY's connection list, it has no standby equivalent, and a stale one latched
		// under max() is worse than none.
		a.metr.ClearTopology()
		a.lastTopologyGap = ""
		a.lastSyncStandbySlots = nil
	default:
		a.metr.ClearSlots()
		a.metr.ClearTopology()
		// Reset the change-detection latch too (#288 review): it holds the gap string from this
		// node's previous life as primary, so a demote/re-promote cycle with the same peer still
		// off-stream would log nothing at all.
		a.lastTopologyGap = ""
		// #308: any action other than serving as primary ends this node's current primary term,
		// so the next time it becomes primary must start with an unconditional first reconcile
		// rather than a cache left over from a previous term.
		a.lastSyncStandbySlots = nil
	}
	switch dec.Action {
	case reconcile.Promote:
		// The node is already running as a standby (the reconcile guard); promote
		// acts on the running postmaster — do NOT Start it (that would error).
		//
		// beatDuring, like every other long mechanism call (#298 review). Native.Promote
		// runs `pg_ctl -w promote`, which waits for the promotion to finish: a standby with
		// a large backlog of received-but-unreplayed WAL -- the ordinary failover case --
		// can sit here for up to PGCTLTIMEOUT (60s, and nothing in this image lowers it),
		// holding opMu with no metr.Beat(). /healthz goes stale at reconcileInterval*3 (15s
		// on chart defaults), and a concurrent dcs.OnLost fence blocks on opMu for the same
		// span. Clone, the initdb path, RejoinForceRewind and ReclonePreserving are all
		// wrapped for exactly this; Promote was the one that was not.
		// Armed BEFORE the promote, not after it (#298 review). A non-nil error from
		// `pg_ctl -w promote` is NOT proof this node did not become a writer: on a standby
		// with a large unreplayed backlog pg_ctl gives up at PGCTLTIMEOUT (60s, nothing in
		// this image lowers it) and exits non-zero while the promotion signal has already
		// been written and the postmaster completes the promotion anyway. With the latch set
		// only on the success path, that timeout returned with servingRW still false, so the
		// lease loss that followed took OnLost's "not read-write; no fence needed" branch and
		// skipped the demote on a node that was becoming a writer -- two primaries until the
		// next tick. Pre-arming only ever makes OnLost demote a node that is becoming read-write
		// (the fail-safe direction), which is exactly the reasoning StartLocal already applies.
		a.servingRW.Store(true)
		if err := a.beatDuring(func() error { return a.mech.Promote(ctx) }); err != nil {
			return err
		}
		a.metr.IncPromotion()
		// Holdership re-check before publishing this node as the primary, exactly as
		// finishInitdbNative does after its own unbounded exec (#298 review). `pg_ctl -w
		// promote` is bounded only by PGCTLTIMEOUT (60s; nothing in this image lowers it) and
		// the chart's default LeaseDuration is 15s, so on the ordinary failover case -- a
		// standby with a large unreplayed backlog -- the lease can lapse and be acquired by a
		// peer while this call is still running. OnLost cannot correct it first: it blocks on
		// opMu, which tick() holds for the whole of act(). Continuing anyway ran advanceMarker
		// and assertPrimaryRouting, pointing the write Service selector and the
		// pg-role=primary label at a pod that no longer holds the lease -- on top of the
		// genuine new primary.
		//
		// servingRW stays ARMED and nothing is demoted here: this node really is read-write
		// now, and the fence is OnLost's job (it runs the moment act() releases opMu). All
		// this branch must not do is claim the routing.
		if !a.dcs.IsLeader() {
			a.log.Warn("lost the lease during promote; not advancing the highwater marker or claiming the write Service -- the lost-leadership fence will demote this node (#298)",
				"node", a.cfg.PodName)
			return nil
		}
		// After promotion the node is read-write. Bound ALL the post-promote
		// bookkeeping -- the slot pass, the WAL re-probe, and the marker + routing
		// apiserver writes -- under the fence budget, sharing one context so the total
		// opMu hold cannot exceed the soft-fence window and starve a lost-leadership
		// OnLost demote while this node is still RW. H3 order: promote PG -> advance
		// marker -> assert routing.
		wctx, cancel := context.WithTimeout(ctx, a.fenceBudget())
		defer cancel()
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
		var promoteSlots []pg.SlotState
		var promoteOwned []string
		var promoteSlotsRead bool
		func() {
			sctx, scancel := context.WithTimeout(wctx, a.fenceBudget()/2)
			defer scancel()
			promoteSlots, promoteOwned, promoteSlotsRead = a.slotsTick(sctx)
		}()
		if tl, _, ok, _ := a.prober.PrimaryWALPosition(wctx, a.selfConn()); ok {
			a.advanceMarker(wctx, tl, true, obs.Marker)
			// ADOPTION is what expires the restore claim (#288 review, second pass). This node
			// promoted and the marker now records its history, so the cluster has taken the
			// restored timeline as its own and the claim has done its job.
			//
			// Deliberately NOT on the Follow path, which was the first attempt: under native,
			// Follow only writes primary_conninfo + standby.signal and reloads, with no rewind
			// at all, so a DIVERGED standby reaches it too. A PITR lands on
			// pgbackrest.restore.podOrdinal, which need not be the lease holder; that pod comes
			// up read-write, takes DemoteFence then Follow, and would have dropped its claim
			// ~10s after boot -- racing the holder's own tick, which needs that very record to
			// decide to release the lease to it. Losing that race discards the restore.
			//
			// STAMPED, not unlinked (#288 review, round 2). Expiring the claim and destroying
			// the record are different things, and doing the second to achieve the first is a
			// regression: dropRestoreRecord exists for volumes whose CONTENTS stopped matching
			// the record (a clone, a rewind, a wipe), and a promotion changes no contents. The
			// earlier revision left GET /v1/status.lastRestore permanently empty after a
			// SUCCESSFUL PITR -- the provenance #276 built the file for -- and silently erased
			// that history on any ordinary failover promote months later.
			//
			// Guarded on there BEING a claim, and mirrored on StayPrimary, because this branch
			// alone never fires on the documented restore path (#288 review, round 4) -- see
			// adoptRestoreIfServing and the StayPrimary comment.
			a.adoptRestoreIfServing(obs)
		}
		// H3 order: promote PG -> advance marker -> assert routing -> sync slots. Routing
		// drives the pod pg-role labels and the read/write Service endpoints, so it must
		// win the fence-budget context if anything is going to; #308's slot reconciliation
		// is new, best-effort, and self-healing on the next tick either way, so it runs
		// LAST, not ahead of routing (a hung slot query must not starve the assertion that
		// actually matters for correctness this tick).
		routingErr := a.assertPrimaryRouting(wctx, obs)
		a.assertSyncStandbySlots(wctx, promoteSlots, promoteOwned, promoteSlotsRead)
		return routingErr

	case reconcile.StayPrimary:
		// The steady-state primary tick. A standby resolves its upstream from the lease
		// holder's identity plus the headless FQDN, so there is no catalog to publish to
		// and no registration step.
		//
		// The node is read-write here, so bound the marker + routing under one
		// fence-budget context (see Promote): a hung apiserver write must not hold opMu
		// past the soft-fence window and starve a lost-leadership fence.
		wctx, cancel := context.WithTimeout(ctx, a.fenceBudget())
		defer cancel()
		// A scale-down leaves no registry ghosts, but it does leave slot residue, and that
		// is what this handles (#289): publish the slot gauges, create slots for
		// expected standbys, and reclaim orphans a scale-down or a repmgr->native migration
		// left pinning WAL. Bounded by the same fence-budget context -- a hung psql must not
		// hold opMu past the soft-fence window -- and best-effort, resuming next tick.
		//
		// Under its OWN sub-budget, for the reason the Promote branch gives (#298 review):
		// left on wctx a single slow query consumes the whole fence budget (5s on chart
		// defaults) and the tick finishes WITHOUT advanceMarker and WITHOUT
		// assertPrimaryRouting -- a write Service still pointing at the old pod. This branch
		// carries MORE than Promote's on the same budget (a slot list, a pod LIST, up to
		// NodeCount-1 creates each allowed 10s by Prober.Timeout, the drop pass, and #308's
		// ALTER SYSTEM + reload), and it is the ONLY branch a restored primary ever takes --
		// the documented scale-to-0 restore comes back through StartLocal -> StayPrimary and
		// never through Promote, so its very first routing assertion happens here, on a tick
		// whose slot pass has to create every peer slot from scratch.
		var staySlots []pg.SlotState
		var stayOwned []string
		var staySlotsRead bool
		func() {
			sctx, scancel := context.WithTimeout(wctx, a.fenceBudget()/2)
			defer scancel()
			staySlots, stayOwned, staySlotsRead = a.slotsTick(sctx)
		}()
		// Keep the highwater marker at this primary's timeline (monotonic; written
		// only when it advances, so steady-state ticks make no API write).
		a.advanceMarker(wctx, obs.Local.Timeline, obs.Local.TimelineOK, obs.Marker)
		// THE path that expires a restore claim in practice (#288 review, round 4). The
		// documented restore procedure is scale to 0, restore into the target ordinal's PVC,
		// scale up -- and pgbackrest runs with --target-action=promote, so that pod comes back
		// holding primary-state data. A lease-holding node with initialized, stopped data takes
		// StartLocal ("lease holder, initialized but stopped") and StayPrimary forever after; it
		// never passes through Promote at all. With the stamp only on the Promote branch a
		// successful restore's claim never expired -- and a permanent claim VETOES every peer in
		// moreAdvancedPeer, so months later this node promotes, or is handed the lease, over a
		// reachable peer holding more WAL: invariant 8 violated and committed WAL discarded, with
		// no restore anywhere in sight.
		a.adoptRestoreIfServing(obs)
		// The topology gauges are NOT published here. They were, and an extra query plus a pod
		// LIST on this branch's already-contended fence budget could make a slow tick skip the
		// marker write or the Service routing assertion above -- so topologyTick runs from
		// tick(), outside opMu, once act() has returned (see its own comment).
		routingErr := a.assertPrimaryRouting(wctx, obs)
		// #308: keep synchronized_standby_slots current as standbys scale up/down. After the
		// routing switch, not before -- same fence-budget priority as the Promote case.
		a.assertSyncStandbySlots(wctx, staySlots, stayOwned, staySlotsRead)
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
		// Fence-bounded (#294 review). This runs inside act(), i.e. under opMu, and does an
		// uncached apiserver LIST plus psql; tick()'s own context has no deadline, so a hung
		// call would hold opMu past the soft-fence window and starve the dcs.OnLost demote,
		// which cannot take opMu. topologyTick was moved out of act() and given exactly this
		// budget in this same change, for exactly this reason.
		sctx, scancel := context.WithTimeout(ctx, a.fenceBudget())
		a.standbySlotsTick(sctx)
		scancel()
		// Repoint only when the upstream actually CHANGES. Follow rewrites
		// primary_conninfo and reloads the postmaster, so running it on every tick of a
		// healthy standby would SIGHUP it for nothing.
		if a.followUpstream == dec.Target {
			return nil
		}
		// Never dial a target that is not one of this cluster's pods (#298 security
		// review): belt-and-suspenders behind the LeaderIdentity sanitisation in observe(),
		// so no forged DCS value can reach a conninfo even if a future caller sets Target
		// from another source.
		if !a.validPeerName(dec.Target) {
			return fmt.Errorf("refusing to follow %q: not a valid cluster pod name (possible tampering)", dec.Target)
		}
		// Invariant 9: never follow an upstream from a different cluster.
		if err := a.assertSameCluster(ctx, dec.Target); err != nil {
			return err
		}
		// The upstream's conninfo: the native mechanism writes upstream.Host into
		// primary_conninfo, and the Lease holder's pod name is always current.
		upConn := a.peerMechConn(dec.Target)
		// #182: a healthy standby may already be streaming from the lease holder before
		// this first Follow runs -- a post-failover rejoin attaches before Follow does.
		// Skip the command when already streaming from the target; repointing to a NEW
		// upstream (sender_host differs, or no walreceiver yet) still falls through.
		//
		// There is no registry to consult: a standby resolves its upstream from the lease
		// holder plus the headless FQDN, so streaming from the target is the whole question.
		//
		// A followRan flag rather than an early return: the #308 dbname convergence below
		// MUST run on the already-streaming path too (its own comment explains why).
		followRan := !a.streamingFromTarget(ctx, dec.Target)
		if followRan {
			if err := a.mech.Follow(ctx, upConn); err != nil {
				// Follow needs no registry row, so it has no unrecoverable state: it writes
				// primary_conninfo and standby.signal, and a failure here is transient,
				// retried on the next tick.
				return err
			}
		}
		// #308: converge dbname in primary_conninfo on EVERY tick that reaches here --
		// whether this tick just ran a real `repmgr standby follow` (which writes
		// primary_conninfo without dbname; PG17+'s sync_replication_slots worker
		// requires it) or skipped it via the already-streaming shortcut above. Running
		// it unconditionally here, not only after a real Follow, matters specifically
		// when a PRIOR tick's reload below failed: that tick returns an error without
		// latching followUpstream, so the next tick re-enters this case, finds itself
		// already streaming (the file was already patched, just not reloaded), and
		// would otherwise take the shortcut and latch without ever retrying the reload
		// -- stranding a standby whose on-disk conninfo has dbname but whose running
		// postmaster does not. EnsurePrimaryConninfoDBName is a no-op, no-reload read
		// when dbname is already active, so this costs nothing on the common path.
		//
		// changed alone cannot drive the retry once the FILE is already correct
		// (EnsurePrimaryConninfoDBName then reports changed=false forever, even though
		// the reload that was supposed to apply it never actually succeeded) -- hence
		// dbNameReloadPending, sticky across ticks until a reload actually succeeds.
		changed, err := a.ensurePrimaryConninfoDBName()
		if err != nil {
			a.log.Warn("ensure dbname in primary_conninfo", "err", err)
		} else if changed || a.dbNameReloadPending {
			if err := a.sup.Reload(ctx); err != nil {
				a.dbNameReloadPending = true
				return fmt.Errorf("reload after patching primary_conninfo dbname: %w", err)
			}
			a.dbNameReloadPending = false
		}
		// Native's Follow only WRITES files (the managed conf fragment + standby.signal)
		// and relies on the caller to apply them -- primary_conninfo is
		// reloadable in modern PostgreSQL (this node is already InRecovery, per Decide's
		// precondition for the Follow action, so a reload -- not a full restart -- is
		// sufficient to make the walreceiver reconnect to the new upstream). Skipping
		// this would leave native mode's Follow silently inert: the file changes, but
		// nothing tells the running postmaster to pick it up (#287).
		//
		// Gated on a Follow having actually RUN (rebase review): neither parent reloaded on the
		// already-streaming shortcut, and doing so SIGHUPs a healthy standby for nothing --
		// worse, a failing reload there returns an error without latching, so the branch
		// re-enters every tick with nothing to apply. It also double-reloaded on the #308 path.
		if followRan {
			if err := a.sup.Reload(ctx); err != nil {
				return fmt.Errorf("reload after follow: %w", err)
			}
		}
		// Release this node's slot on the upstream it just LEFT (#294, live-cluster finding).
		// Ordered after the reload so the walreceiver is already attached to the new upstream
		// through the new slot -- the old one is then provably unused by this node.
		// Fence-bounded, and this one needs it most (#294 review): it deliberately dials the
		// FORMER upstream, which under cascade is often the node that just died, so its 10s
		// connect_timeout alone would stall the reconcile goroutine against a /healthz staleness
		// threshold of 3x the interval -- while holding opMu.
		rctx, rcancel := context.WithTimeout(ctx, a.fenceBudget())
		a.releaseSlotOnFormerUpstream(rctx, a.followUpstream, dec.Target)
		rcancel()
		a.latchFollow(dec.Target)
		return nil

	case reconcile.DemoteFence:
		a.metr.IncDemote()
		// Every demote is bounded (#298 review): ChildPostmaster.Stop escalates to SIGKILL
		// only when its context expires, and tick()'s carries no deadline -- so a frozen
		// postmaster would block act() with opMu held, stopping the heartbeat and leaving
		// the second writer up. RenewDeadline, matching the OnLost fence.
		dctx, dcancel := context.WithTimeout(ctx, a.cfg.RenewDeadline)
		defer dcancel()
		return a.sup.Demote(dctx, true)

	case reconcile.RejoinForward:
		return a.rejoinOnto(ctx, dec.Target)

	case reconcile.BootstrapClone:
		// Never clone from a target that is not one of this cluster's pods (#298
		// security review): the clone source becomes primary_conninfo and dials with the
		// replication PGPASSWORD set.
		if !a.validPeerName(dec.Target) {
			return fmt.Errorf("refusing to clone from %q: not a valid cluster pod name (possible tampering)", dec.Target)
		}
		// pg_basebackup demands a byte-empty target, but Decide's "empty data" is HasData
		// (PG_VERSION) -- so a stray entry in a database-less PGDATA parks the node in a
		// permanent BootstrapClone/`exists but is not empty` loop. Observed live (#298
		// review): the disk-loss suite emptied PGDATA under a running postmaster, the dying
		// postmaster's core dump landed in its cwd, and the replacement pod wedged for good.
		// A clone interrupted before PG_VERSION reaches the same state (discardTornClone's
		// no-PG_VERSION branch clears only the marker). PG_VERSION is absent on this path,
		// so the entries are debris by definition; clear them here, on every attempt, and
		// name what was thrown away.
		// Guarded on HasData: only a database-less directory is debris. PG_VERSION present
		// here would mean Decide and the filesystem disagree -- leave it alone and let
		// pg_basebackup refuse loudly rather than delete anything that claims to be a cluster.
		if !process.HasData(a.cfg.PGDATA) {
			if removed, derr := process.ClearDebrisDataDir(a.cfg.PGDATA); derr != nil {
				return fmt.Errorf("clear pre-clone debris from %s: %w", a.cfg.PGDATA, derr)
			} else if len(removed) > 0 {
				a.log.Warn("cleared non-database debris from PGDATA before cloning", "removed", removed)
			}
		}
		// Marker around the clone so an interrupted one is discarded on the next boot rather
		// than stranding the pod with a torn PGDATA (#288 review).
		a.beginClone()
		if err := a.beatDuring(func() error { return a.mech.Clone(ctx, a.peerMechConn(dec.Target)) }); err != nil {
			// Discard here, not only at the next boot (#288 review). If pg_basebackup's child is
			// killed while the agent survives, PGDATA is left with PG_VERSION present, so Decide
			// takes the has-data branch and fails every tick -- and discardTornClone only runs in
			// boot(), so recovery would wait for the startup probe to kill the container (600s on
			// defaults). Doing it now re-arms BootstrapClone on the very next tick.
			a.discardTornClone(ctx)
			return err
		}
		a.endClone()
		// A clone REPLACES whatever this volume was, so any initdb marker left by an earlier
		// bootstrap attempt on this pod is now meaningless -- and actively dangerous, because a
		// clone taken from a cluster created before the completion sentinel existed carries no
		// sentinel either, which is exactly the shape discardTornInitdb wipes (#288 review).
		a.endInitdb()
		a.dropRestoreRecord("the data directory was cloned from " + dec.Target)
		// #308: patch primary_conninfo's dbname while stopped so Start below picks it up
		// with no extra reload. Best-effort: a failure here must not block a fresh clone
		// from starting -- physical replication works without it.
		if _, err := a.ensurePrimaryConninfoDBName(); err != nil {
			a.log.Warn("ensure dbname in primary_conninfo", "err", err)
		}
		// Latch the CLONE SOURCE as this node's upstream (#294, second live-cluster finding).
		//
		// Factually true -- Native.Clone ends by writing primary_conninfo pointing at the
		// source, so this node streams from it the moment Start returns -- and load-bearing for
		// releaseSlotOnFormerUpstream. Clone provisions this node's slot ON THE SOURCE, which is
		// always the lease holder, so under cascading replication that slot is stranded the
		// instant the node re-homes onto an intermediate. Without this latch, followUpstream is
		// still "" on that first post-clone Follow, the release returns at its own guard, and
		// nothing reclaims the slot: the primary's drop pass deliberately keeps any slot whose
		// ordinal has a live pod. Observed live -- a node whose first Follow was already the
		// cascade hop left an inactive slot on the primary, while a node that transited the
		// leader first was cleaned up correctly. Whichever branch a node takes is a race on
		// whether its cascade parent already qualifies, so the leaky path is the LIKELIER one
		// for later ordinals.
		//
		// Safe for the stickiness logic in cascadeFollowTarget: it only honours a latched
		// upstream that is not the leader, and this one always is.
		a.latchFollow(dec.Target)
		return a.sup.Start(ctx)

	case reconcile.ReleaseLease:
		// Step down: demote/stop the local writer FIRST, then release the Lease so a
		// peer can take over (stale-winner handoff at cold boot, or self-health
		// failover). The ordering is load-bearing on the K8s backend (#298 review):
		// client-go's ReleaseOnCancel empties the Lease record BEFORE OnStoppedLeading
		// runs, so a standby can acquire the freed Lease and promote within
		// ~RetryPeriod of Release() -- while a slow demote or the SIGQUIT->SIGKILL
		// escalation on a frozen postmaster can take up to RenewDeadline. Releasing
		// first therefore opened a two-writer window; holding the Lease through the
		// stop fences it (no peer can win a Lease we still hold), and only a COMPLETED
		// demote/stop is followed by the release. On a demote/stop error we keep the
		// Lease and let the next tick (or the OnLost fence at lease expiry) retry --
		// releasing with a possibly-live writer up is the one unrecoverable ordering.
		// Only stop postgres if we were serving read-write; a standby is already
		// read-only, so releasing the Lease is enough and we avoid churning its
		// postmaster.
		switch {
		case obs.Local.Running && !obs.Local.InRecovery:
			a.metr.IncDemote()
			// Bounded (see DemoteFence). Here the bound is also what makes demoting
			// before releasing safe: an unbounded hang would hold the Lease forever,
			// so no peer could take over either.
			dctx, dcancel := context.WithTimeout(ctx, a.cfg.RenewDeadline)
			err := a.sup.Demote(dctx, false)
			dcancel()
			if err != nil {
				// The handoff is still ABANDONED on any error -- a SIGKILL'd postmaster did
				// not run its shutdown checkpoint, so WAL it had written but not yet streamed
				// would be lost if a peer promoted now, and this node keeping the Lease and
				// coming back on the next tick loses nothing. But when stopProvedDead says
				// the child was killed AND REAPED, the read-write latch is provably wrong,
				// and leaving it armed is the spurious-fence bug this PR fixes elsewhere: a
				// lease lapse before the next tick would run OnLost's fence branch on a node
				// with nothing running, incrementing pg_ha_agent_fences_total and paging
				// PGHAAgentFlapping (#298 review).
				if a.stopProvedDead(err) {
					a.log.Warn("release lease: the postmaster did not exit before the demote deadline and was killed; it is reaped, so the read-write latch is cleared, but the Lease is kept until a clean step-down", "err", err)
					a.clearServingRWForPlannedStepDown()
				}
				return err
			}
			a.clearServingRWForPlannedStepDown()
		case obs.LocalStuck && !obs.Local.InRecovery:
			// Self-health failover: a primary-state postmaster that is wedged/frozen
			// (SQL unreachable, so Running is false). The agent owns this PID-1 child,
			// so force-stop it (SIGQUIT->SIGKILL on timeout) while we still hold the
			// Lease: a frozen primary that later unfreezes would be a second writer.
			// Harmless if it is already down.
			a.metr.IncDemote()
			rctx, cancel := context.WithTimeout(ctx, a.cfg.RenewDeadline)
			defer cancel()
			if err := a.sup.Stop(rctx, process.Immediate); err != nil {
				// Same reading as the arm above (#298 review): a reaped SIGKILL is positive
				// evidence the wedged writer is gone, so the latch must not stay armed even
				// though the Lease is kept for a clean step-down.
				if a.stopProvedDead(err) {
					a.log.Warn("release lease: the wedged postmaster was killed and reaped, so the read-write latch is cleared", "err", err)
					a.clearServingRWForPlannedStepDown()
				}
				return err
			}
			a.clearServingRWForPlannedStepDown()
		}
		// NOTE the clears live INSIDE the two arms above, not here (#298 review). Falling
		// through this switch means neither arm ran -- a node whose SQL probe is failing
		// while its postmaster is still alive and not yet past the stuck grace, which is
		// exactly the "uncertain, may still be a writer" state tick() refuses to clear the
		// latch on. Clearing it here would release the Lease AND disarm the OnLost fence for
		// that node, so a peer promotes while a possibly-live writer is still up.
		a.dcs.Release()
		return nil

	case reconcile.Switchover:
		// Controlled handoff (Part H2): the decision guard already confirmed the
		// target is a caught-up, same-timeline standby. Clear the request FIRST so a
		// later unrelated failover cannot re-trigger a handoff to the same pod; only
		// on a successful clear do we step down (graceful/fast demote, THEN release
		// the Lease). The target, being caught up, then wins the freed Lease and
		// promotes. If the clear fails we do NOT step down -- the request persists,
		// so the next tick retries.
		//
		// FENCE-BOUNDED, like every other apiserver call on this goroutine (#298 review).
		// This node is a SERVING READ-WRITE primary here -- that is the precondition for a
		// switchover -- and the REST config sets no client Timeout, so an unbounded Get+Update
		// against a blackholed apiserver blocks act() with opMu held: no further metr.Beat(),
		// /healthz stale at reconcileInterval*3 (15s on defaults) so the kubelet SIGKILLs PID 1
		// and the postmaster under it, and dcs.OnLost -- which must take opMu to demote -- can
		// never fence the still-read-write node. A deadline miss just leaves the request
		// standing, which the next tick retries; that is exactly the failure this branch
		// already handles.
		cctx, ccancel := context.WithTimeout(ctx, a.fenceBudget())
		cerr := a.kube.ClearSwitchoverTarget(cctx, a.cfg.MarkerName)
		ccancel()
		if cerr != nil {
			return cerr
		}
		// Demote BEFORE releasing (same two-writer reasoning as ReleaseLease above,
		// #298 review): the graceful demote flushes WAL to the connected, caught-up
		// target while we still hold the Lease; only then is the Lease freed for the
		// target to win. On a demote error the handoff is abandoned (the request was
		// already cleared; the operator re-requests) rather than releasing with a
		// possibly-live writer up.
		if obs.Local.Running && !obs.Local.InRecovery {
			a.metr.IncDemote()
			// Bounded (see DemoteFence), for the reason ReleaseLease gives.
			dctx, dcancel := context.WithTimeout(ctx, a.cfg.RenewDeadline)
			err := a.sup.Demote(dctx, false)
			dcancel()
			if err != nil {
				// Abandoned on any error, for the reason above -- a killed postmaster
				// skipped its shutdown checkpoint, so promoting the target now would
				// drop WAL this node had written but not yet streamed, while abandoning
				// loses nothing. A REAPED kill still proves the writer is gone, though,
				// so the latch is cleared rather than left to arm a spurious fence on
				// the next lease lapse (#298 review).
				if a.stopProvedDead(err) {
					a.log.Warn("switchover: the postmaster did not exit before the demote deadline and was killed; it is reaped, so the read-write latch is cleared, but the handoff is abandoned and must be re-requested", "err", err)
					a.clearServingRWForPlannedStepDown()
				}
				return err
			}
			// Same as ReleaseLease, and inside the guard for the same reason: only a
			// COMPLETED demote is proof this node is no longer a writer (#298 review).
			a.clearServingRWForPlannedStepDown()
		}
		a.dcs.Release()
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
			} else {
				// standby-state: ASSERT standby.signal rather than trusting it to be there
				// (#298 review). The branch was asymmetric -- it removed the file for
				// primary-state data but never created it for standby-state data -- and
				// InRecovery comes from pg_controldata, not from the file, so the two can
				// disagree. A control file reading "in archive recovery" with the signal
				// missing (the RejoinForceRewind window, a lost file after an unclean node
				// reboot, a manual removal) was started READ-WRITE here, on the live
				// primary's timeline. StartRecovery one branch below already does this;
				// doing it here too makes both arms state what they intend on disk.
				if err := process.SetRecoverySignal(a.cfg.PGDATA); err != nil {
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
		serr := a.sup.Stop(rctx, process.Immediate)
		cancel()
		// The Stop error is INSPECTED, not discarded (#298 review), with the same distinction
		// runIntent draws. Stop has a documented path where it gives up and deliberately
		// leaves the child supervised: SIGKILL is undeliverable to a process in
		// uninterruptible sleep, so on a wedged PV it returns "postmaster did not exit ...
		// after SIGKILL (PGDATA I/O wedged?); leaving it supervised" without clearing its
		// handle. Start then sees a live handle and returns nil ("still running"), so act()
		// returned nil: no IncReconcileError, no error log, nothing -- every 5s tick reported
		// a successful restart while the single-node database was down and the only signal
		// was an IncDemote claiming a demote had happened.
		//
		// context.DeadlineExceeded ALONE is the normal escalation (killed and reaped), so it
		// is not fatal -- but the "left supervised" error wraps ctx.Err() too, which is why
		// Running() is part of the test.
		if serr != nil {
			// stopProvedDead is this test, shared with the fence, the planned shutdown and the
			// control-API restart so the four cannot drift (#298 review).
			if !a.stopProvedDead(serr) {
				return fmt.Errorf("restart local: could not prove the postmaster is dead, so not reporting a restart: %w", serr)
			}
			a.log.Warn("restart local: the postmaster did not exit before the stop deadline and was killed; starting it back up", "err", serr)
		}
		return a.sup.Start(ctx)

	case reconcile.BootstrapInitdb:
		// The entrypoint deliberately does NOT initdb on the agent path (#288). It cannot: the
		// init container does not clone, so every pod would arrive with an empty PGDATA and
		// create its own cluster with its own system_identifier -- and assertSameCluster
		// (invariant 9) then refuses to rejoin any of them, leaving pods Running, never
		// Ready, holding bogus databases. Whether to initdb is a CLUSTER-WIDE decision, and
		// the lease is the only thing that can make it exactly once.
		//
		// Decide already guarantees that: BootstrapInitdb is returned only for the lease
		// holder with empty data, no reachable primary and no highwater marker -- i.e. a
		// genuine fresh install. Non-holders get Wait, then BootstrapClone once this node is
		// open, and clone with pg_basebackup through their own pre-created slot (#289).
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
	// Bounded, just not by the FENCE budget (#288 review). initdb plus role/database creation is
	// legitimately slower than a failover window, and this node is not serving anything yet, so
	// there is no read-write exposure for a soft fence to race -- but "not the fence budget"
	// must not mean "no budget": bootstrap_initdb runs `pg_ctl -w start` and `pg_ctl -w stop`,
	// and if either wait never returns -- or a failed stop leaves the postmaster holding the
	// stdout pipe, which Cmd.Output waits on -- act() would hold opMu forever, the reconcile
	// loop would stop beating, and dcs.OnLost (which also takes opMu) could never fence.
	//
	// The budget cannot be the only protection, because the failure that matters is the agent
	// never returning from here at all: the kubelet can SIGKILL this container mid-bootstrap
	// (see initdbMarkerPath). Hence the marker, which makes the next boot recover instead.
	// Same pre-flight as BootstrapClone, for the same reason (#298 review): `initdb -D`
	// refuses a target that is not byte-empty, while the caller's emptiness test is
	// HasData (PG_VERSION). A bootstrap SIGKILLed while initdb was still laying out
	// subdirectories -- or a core dump the kernel wrote into a dying postmaster's cwd --
	// leaves PGDATA non-empty with no PG_VERSION, and every later tick then decides
	// BootstrapInitdb and fails on "directory exists but is not empty", forever.
	// discardTornInitdb cannot help: its no-PG_VERSION branch only clears the marker.
	// With PG_VERSION absent the entries are debris by definition, and ClearDebrisDataDir
	// refuses an initialized directory and a live postmaster on its own.
	if !process.HasData(a.cfg.PGDATA) {
		if removed, derr := process.ClearDebrisDataDir(a.cfg.PGDATA); derr != nil {
			return fmt.Errorf("clear pre-initdb debris from %s: %w", a.cfg.PGDATA, derr)
		} else if len(removed) > 0 {
			a.log.Warn("cleared non-database debris from PGDATA before initdb", "removed", removed)
		}
	}
	a.beginInitdb()
	ictx, icancel := context.WithTimeout(ctx, initdbBudget)
	defer icancel()
	// Exec.Run hands every child a credential-stripped environment (#298 security
	// review), but bootstrap_initdb is the one child that legitimately NEEDS the
	// cluster passwords -- it creates the superuser and the replication role, and its
	// first act is `: "${POSTGRES_PASSWORD:?}"` / `: "${REPMGR_PASSWORD:?}"`. Hand
	// them back through the unfiltered extra slice; relying on inheritance here made
	// a fresh native install die on that guard, discard the (empty) directory and
	// retry the same failure every tick, forever (#298 review). An unset
	// PostgresPassword stays unset so the entrypoint's diagnostic names the missing
	// variable instead of failing later in SQL.
	bootstrapEnv := []string{"REPMGR_PASSWORD=" + a.cfg.RepmgrPassword}
	if a.cfg.PostgresPassword != "" {
		bootstrapEnv = append(bootstrapEnv, "POSTGRES_PASSWORD="+a.cfg.PostgresPassword)
	}
	var out string
	err := a.beatDuring(func() error {
		var rerr error
		out, rerr = a.prober.Exec.Run(ictx, bootstrapEnv, entrypointPath, "initdb")
		return rerr
	})
	// Log it on SUCCESS too (#288 review, round 4). Because the AGENT runs the bootstrap here
	// rather than the container entrypoint, initdb's output, the transient postmaster's startup
	// lines and "PostgreSQL initialization complete" are captured into this pipe instead of the
	// pod log -- so without this a fresh install would leave nothing behind but a one-line agent
	// message. This is once per cluster lifetime.
	if err == nil {
		if trimmed := strings.TrimSpace(out); trimmed != "" {
			a.log.Info("bootstrap initdb output", "out", trimmed)
		}
	}
	if err != nil {
		// This path CAN leave a data directory behind (#288 review), so it needs the same
		// cleanup as everything below. bootstrap_initdb runs initdb first and only then
		// `pg_ctl -w start` and the role/database creation, so a failure at the start -- or a
		// budget expiry, or an OOM kill -- leaves PGDATA fully initialized with NO repmgr role
		// or database. bootstrap_initdb then no-ops on it forever, and the node comes up as a
		// primary the agent can never authenticate against and no standby can clone from.
		werr := a.discardFreshDataDir()
		if werr != nil {
			// Marker deliberately LEFT in place: the directory really is torn, so the next boot
			// must still see it and retry the discard.
			return fmt.Errorf("bootstrap initdb: %w: %s (and could not discard the partial data directory, delete the PVC to recover: %v)", err, strings.TrimSpace(out), werr)
		}
		a.endInitdb()
		return fmt.Errorf("bootstrap initdb: %w: %s", err, strings.TrimSpace(out))
	}
	nid := mechanism.NodeIdentity{
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
		a.endInitdb()
		return err
	}
	// Cleared only once the directory is a finished cluster (bootstrap_initdb wrote its
	// completion sentinel) or is gone again. Anything else leaves the marker standing on
	// purpose, which is what makes the next boot's discardTornInitdb able to recover.
	a.endInitdb()
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
	// Write the highwater marker NOW, not on the next StayPrimary tick (#288 review, second
	// pass). The lease-loss branch above already reasons that an initialized PGDATA with no
	// marker is unrecoverable; the success path left the same window open. If the agent dies
	// (OOM, kill) between Start and its next tick, the lease expires, a peer acquires it and
	// sees empty PGDATA, no marker and no REACHABLE primary -- this pod's postmaster is up but
	// its container just restarted -- so it takes BootstrapInitdb and creates a SECOND cluster.
	// Two system_identifiers then make assertSameCluster refuse every Follow/RejoinForward on
	// both pods: both wedge Running/NotReady and only a PVC delete recovers.
	//
	// With the marker present, the peer takes the #170 "empty data with a marker present; settle
	// before initdb" path instead and clones. Best-effort by design: advanceMarker logs and
	// carries on, and a failure here is no worse than the previous behaviour.
	//
	// The read has to WAIT for the postmaster (#288 review, round 3). sup.Start is
	// fire-and-forget -- ChildPostmaster.Start returns as soon as exec.Cmd.Start does -- so a
	// single PrimaryWALPosition here dialled 127.0.0.1 microseconds later and was refused;
	// connect_timeout bounds a connect, it does not retry one. ok was therefore false on
	// essentially every fresh install, the marker was never written, and the window this block
	// exists to close stayed open. Poll instead, briefly: the cluster was just stopped cleanly by
	// bootstrap_initdb, so it has no recovery to replay and comes up in about a second.
	if tl, ok := a.waitForPrimaryTimeline(ctx, a.markerWait()); ok {
		// MarkerState zero value = absent, which is the truth for a cluster created seconds ago
		// and makes shouldAdvanceMarker treat any readable timeline as an advance.
		//
		// FENCE-BOUNDED, like the other two advanceMarker call sites (#298 review). sup.Start
		// above has already put a read-write postmaster up and armed servingRW, and this runs
		// on the reconcile goroutine with opMu held -- so an unbounded apiserver write here has
		// exactly the starvation shape every other call on this path was bounded against: no
		// heartbeat, a liveness kill of PID 1 over a stalled apiserver, and an OnLost fence
		// that cannot run. Best-effort already, so a deadline miss costs one reconcile interval.
		mctx, mcancel := context.WithTimeout(ctx, a.fenceBudget())
		a.advanceMarker(mctx, tl, true, reconcile.MarkerState{})
		mcancel()
	} else {
		// Not fatal: the next StayPrimary tick writes it. Logged because the gap between here
		// and that tick is exactly the unrecoverable window described above.
		a.log.Warn("could not read the new cluster's timeline in time to write the highwater marker; the next primary tick will",
			"waited", a.markerWait())
	}
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

// initdbMarkerPath is the initdb twin of cloneMarkerPath: a sentinel recording that the
// multi-step cluster bootstrap was started and has not finished (#288 review).
//
// Beside PGDATA rather than inside it, for the same reason -- initdb requires an empty target,
// so a marker within would be destroyed by the operation it tracks.
//
// Why the budget is not enough on its own. bootstrapInitdbNative bounds the exec with
// initdbBudget, and every error return calls discardFreshDataDir, so a slow or failing
// bootstrap already cleans up after itself. What neither covers is the agent not returning at
// all: `pg_ctl -w start` inside bootstrap_initdb satisfies the chart's startupProbe (plain
// `pg_isready`, answered over the unix socket), which retires the startup grace and arms the
// liveness probe -- and /healthz goes stale after ~3x the reconcile interval because act() holds
// opMu for the whole exec without beating. On a contended node the kubelet can therefore SIGKILL
// the container mid-bootstrap, with the same effect as an OOM kill or a node reboot: PGDATA is
// initialized but carries no repmgr role or database, bootstrap_initdb no-ops on it forever
// (PG_VERSION exists), and the pod comes up as a primary the agent can never authenticate
// against. No error is returned to clean up after, so only a next-boot check can recover it.
func (a *agent) initdbMarkerPath() string {
	return filepath.Join(filepath.Dir(filepath.Clean(a.cfg.PGDATA)), ".pg-ha-initdb-in-progress")
}

// bootstrapCompletePath is the positive evidence bootstrap_initdb writes as its LAST action.
// The marker alone cannot justify wiping: endInitdb only WARNS when its remove fails, so a
// stale marker is reachable over a bootstrap that DID complete -- and by the next boot that
// directory may be a serving primary. pg_controldata is no help here (unlike the torn-clone
// case, where pg_basebackup writes pg_control last on purpose): initdb writes a valid control
// file immediately, so a half-bootstrapped directory parses perfectly.
func (a *agent) bootstrapCompletePath() string {
	return filepath.Join(a.cfg.PGDATA, ".pg-ha-bootstrap-complete")
}

// beginInitdb records that a cluster bootstrap is starting. Best-effort, like beginClone: a
// missing marker costs only the automatic discard if that bootstrap is interrupted.
func (a *agent) beginInitdb() {
	if err := os.WriteFile(a.initdbMarkerPath(), []byte(a.cfg.PodName+"\n"), 0o600); err != nil {
		a.log.Warn("write the initdb-in-progress marker; an interrupted bootstrap will need a manual PVC delete", "err", err)
	}
}

// endInitdb clears the marker after a bootstrap completes.
func (a *agent) endInitdb() {
	if err := os.Remove(a.initdbMarkerPath()); err != nil && !os.IsNotExist(err) {
		a.log.Warn("remove the initdb-in-progress marker", "err", err)
	}
}

// discardTornInitdb wipes a data directory left behind by an INTERRUPTED cluster bootstrap, so
// the reconcile loop sees an empty PGDATA and can bootstrap again (#288 review).
//
// Destructive only on positive evidence of tornness: the marker says a bootstrap was in flight,
// and the absence of bootstrapCompletePath says it never finished. A directory that finished is
// KEPT whatever else happened, and the marker is cleared so no later boot reconsiders it.
//
// Discarding is safe precisely because this is a bootstrap: the directory was created by this
// pod moments earlier and has never served. The transient postmaster bootstrap_initdb runs is
// socket-only, so no peer can have cloned from it either.
func (a *agent) discardTornInitdb(ctx context.Context) {
	if _, err := os.Stat(a.initdbMarkerPath()); err != nil {
		return // no bootstrap was in flight
	}
	if !process.HasData(a.cfg.PGDATA) {
		// Never got as far as PG_VERSION; nothing to discard.
		a.endInitdb()
		return
	}
	if _, err := os.Stat(a.bootstrapCompletePath()); err == nil {
		a.log.Info("the cluster bootstrap completed; keeping the data directory (stale in-progress marker, #288)")
		a.endInitdb()
		return
	} else if !os.IsNotExist(err) {
		// Could not look. Not evidence of tornness -- leave the directory alone and let the
		// ordinary has-data paths report whatever is really wrong.
		a.log.Warn("could not read the bootstrap-complete sentinel; not discarding the data directory on that basis", "err", err)
		return
	}
	a.log.Warn("discarding a data directory left by an interrupted cluster bootstrap so it can be created again (#288)",
		"pgdata", a.cfg.PGDATA)
	if err := a.discardFreshDataDir(); err != nil {
		a.log.Error("could not discard the torn bootstrap; delete the PVC to recover", "err", err)
		return
	}
	a.endInitdb()
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
	if _, err := a.readControlData(ctx); err == nil {
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

// initdbMarkerWait bounds the poll for the newly created cluster to accept SQL, so the highwater
// marker can be written before this function returns (#288 review, round 3). Short on purpose:
// bootstrap_initdb stopped the cluster cleanly, so there is no recovery to replay -- and the
// fallback (the next StayPrimary tick) costs one reconcile interval, not correctness.
const initdbMarkerWait = 15 * time.Second

// markerWait is initdbMarkerWait unless overridden (tests set it short so they do not spend the
// full budget waiting for a postmaster that a fake exec will never start).
func (a *agent) markerWait() time.Duration {
	if a.initdbMarkerWaitOverride > 0 {
		return a.initdbMarkerWaitOverride
	}
	return initdbMarkerWait
}

// waitForPrimaryTimeline polls the local postmaster until it answers as a read-write primary,
// returning its timeline. Bounded by budget; false when the postmaster did not become readable
// in time (or ctx ended first).
func (a *agent) waitForPrimaryTimeline(ctx context.Context, budget time.Duration) (pg.Timeline, bool) {
	wctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for {
		if tl, _, ok, _ := a.prober.PrimaryWALPosition(wctx, a.selfConn()); ok {
			return tl, true
		}
		select {
		case <-wctx.Done():
			return 0, false
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// confdDir is where the chart mounts the operator's postgresql.configuration / TLS / audit /
// pgbackrest fragments. Mounted only when one of those features is on, which is what makes its
// presence a usable signal (#288).
const confdDir = "/etc/postgresql/conf.d"

// entrypointPath is the image's entrypoint, invoked with an explicit mode. Fixed rather than
// derived: the agent runs inside that image by construction (#288).
const entrypointPath = "/usr/local/bin/entrypoint.sh"

// pgpassPath is the postgres user's home in the pg-ha image; written/owned by the
// postgres uid the agent runs as. Fixed (not $HOME) because after gosu the agent
// may inherit root's HOME, which postgres cannot write.
const pgpassPath = "/var/lib/postgresql/.pgpass"

// writePgpass writes a 0600 .pgpass with the replication credential so a passwordless
// primary_conninfo (the password is deliberately never written into the managed config --
// the PR1 hardening) can still authenticate streaming replication. It also exports
// PGPASSFILE so the walreceiver child (and the agent's psql/pg_basebackup children) find it
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
			// A password this agent cannot SASLprep is a PERMANENT skip, not a retry: no
			// later tick can produce a verifier the client would match, and the md5 line the
			// agent writes above each scram rule keeps the user authenticating exactly as it
			// does today. Retrying would only log the same warning every 5s forever, and
			// blocking the latch would keep the OTHER user's converged re-hash from ever
			// being recorded (#298 review).
			if errors.Is(err, pg.ErrNeedsSASLprep) {
				a.log.Warn("md5->scram re-hash skipped: this password needs SASLprep normalisation, which the agent does not implement; the md5 hash is kept and the pg_hba md5 fallback keeps it working",
					"user", u.name)
				continue
			}
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

// repmgrPreloadLib is the shared_preload_libraries entry #293 removes. It names repmgr.so
// in the server's module directory.
const repmgrPreloadLib = "repmgr"

// migrateForeignRecoveryConfig clears the GUCs that decide where this standby streams from out
// of postgresql.auto.conf, so the agent's own fragment becomes authoritative (#292).
//
// #294 added this as a REFUSAL, because there was no migration to offer; #292 makes it act.
//
// This guards the one upgrade path 2.0.0 otherwise fails SILENTLY: a cluster created by a 1.x
// release. 1.x had no `ha.agent.mechanism`, so nothing in the chart can detect it --
// pg.validateRemovedRepmgrdValues sees no stale key, MECHANISM: "native" renders clean, and the
// release installs. But repmgr already wrote primary_conninfo and
// primary_slot_name = repmgr_slot_<node_id> into every standby's auto.conf; PostgreSQL reads
// auto.conf AFTER every include, so those outrank the agent's managed fragment -- the same
// precedence Native.Clone avoids `pg_basebackup -R` to stay clear of. Nothing strips them, and
// EnsurePrimaryConninfoDBName only patches dbname INTO whatever it finds, actively preserving
// the stale value.
//
// The failure was invisible: Follow writes the fragment, Reload succeeds, and the walreceiver
// keeps the old upstream through a slot that no longer exists. streamingFromTarget never
// matches, Follow re-runs every tick, and the stall window then escalates to pg_rewind and
// possibly a full re-clone. Both the CHANGELOG and the chart say this path needs #292's in-place
// migration first; invariant 4 says that has to be enforced, not merely documented.
//
// Stripping the lines instead would be worse: sequencing it against the catalog and the slots is
// exactly what #292 is for, and a half-migration is harder to reason about than a pod that will
// not start with a message naming the issue.
func (a *agent) migrateForeignRecoveryConfig() error {
	autoConf := filepath.Join(a.cfg.PGDATA, "postgresql.auto.conf")
	found, err := pgconf.ForeignRecoveryConfig(autoConf)
	if err != nil {
		// Unreadable is not evidence. Refusing on it would be its own outage -- the same
		// asymmetry as the module-directory check above.
		a.log.Warn("could not read postgresql.auto.conf; skipping the recovery-config check", "path", autoConf, "err", err)
		return nil
	}
	if len(found) == 0 {
		return nil
	}
	// #292 turns what #294 could only refuse into a migration. The settings are REMOVED rather
	// than rewritten: whole lines, so the agent's own fragment becomes the single definition of
	// where this node streams from.
	//
	// Why this is safe to do automatically, when the catalog cleanup deliberately is not:
	//   - It touches configuration only. No data, no slots, no catalog, no timeline.
	//   - It is reversible. Pin the previous chart and image and repmgr's own `standby follow`
	//     rewrites both settings on its next run; nothing has been destroyed.
	//   - It runs with the postmaster STOPPED, from the startup preflight, so it cannot race
	//     PostgreSQL's own wholesale rewrite of auto.conf.
	//   - The alternative is worse: refusing leaves every 1.x consumer with no upgrade path at
	//     all, and the effective upstream is preserved either way -- the agent derives it from
	//     the lease, not from the file being removed.
	//
	// NO identity check here, deliberately (#290 review). An earlier cut read pg_controldata
	// before and after and compared SystemID/Timeline -- but RemoveRecoveryConfig only rewrites
	// postgresql.auto.conf and cannot touch pg_control, so neither branch was reachable. It was
	// false assurance, and it cost two unbounded pg_controldata execs on every boot, before
	// leader election, where a stalled one would hang startup.
	//
	// What actually makes this safe is the operation's scope: configuration only, no data, no
	// slots, no catalog, no timeline. The identity guarantees that DO matter -- refusing to
	// follow or rewind onto a different cluster -- are enforced by assertSameCluster on the
	// paths that move data, where a mismatch is possible.
	// CARRIED FORWARD, not just deleted (#298, found by the first live run of the repmgrd->2.0.0
	// roll). The paragraph above says "the effective upstream is preserved either way -- the agent
	// derives it from the lease" -- and during this very migration that is false. Rolling a live
	// failoverMode:repmgrd release, the StatefulSet replaces the highest ordinal first: that pod
	// comes up as the ONLY agent in the cluster, while the real primary is still a 1.x pod running
	// repmgrd and no agent at all. So no agent-held lease names a leader, Decide returns Wait
	// ("standby but no known leader; keep the current upstream"), and nothing ever writes the
	// fragment this function just declared authoritative. The node had `standby.signal` and no
	// primary_conninfo at all -- PostgreSQL logged `specified neither "primary_conninfo" nor
	// "restore_command"`, it never streamed, readiness never passed, and the rollout stopped
	// there with the cluster half migrated. Observed as a 10s livelock: acquire the lease,
	// refuse to promote on the #171 equal-timeline guard, release, wait, re-acquire.
	//
	// Reading the value first and seeding the agent's own fragment with it keeps the node
	// streaming from wherever repmgr had pointed it, which is what "preserved either way" was
	// supposed to mean. It is the same upstream, so this changes no topology; the first Follow
	// the agent performs once the cluster is fully migrated overwrites the fragment properly.
	// ...but only for a node that is actually a STANDBY (#298 review). PostgreSQL does not strip
	// primary_conninfo from postgresql.auto.conf on promotion, so an ex-standby that repmgr later
	// promoted still carries a STALE one -- and the primary is the LAST pod the roll replaces. On
	// that pod the carry-forward would seed a dead upstream into the fragment and, worse,
	// pre-create pg_ha_slot_0 on a peer that is a standby by then: an inactive slot on a node
	// whose reconcileSlots only runs while it is primary, i.e. a permanent WAL pin, which is the
	// silent disk-fill #289 exists to stop. standby.signal is the authoritative local answer and
	// costs one stat, so the preflight can ask it without the pg_controldata exec the comment
	// above rules out.
	inherited, inheritedSlot := "", ""
	if !process.HasRecoverySignal(a.cfg.PGDATA) {
		a.log.Info("in-place migration (#292): no standby.signal, so this data directory is a primary's; not carrying the stale repmgr upstream forward",
			"path", autoConf)
	} else {
		var ierr, slerr error
		inherited, ierr = pgconf.PrimaryConninfoValue(autoConf)
		inheritedSlot, slerr = pgconf.PrimarySlotNameValue(autoConf)
		if slerr != nil {
			a.log.Warn("in-place migration (#292): could not read the inherited primary_slot_name", "path", autoConf, "err", slerr)
		}
		if ierr != nil {
			// Not fatal: the migration below is still the right thing to do, and a node that ends
			// up without an upstream is recoverable by hand. Say so rather than failing the boot.
			a.log.Warn("in-place migration (#292): could not read the inherited primary_conninfo; the upstream will not be carried forward",
				"path", autoConf, "err", ierr)
		}
	}
	removed, err := pgconf.RemoveRecoveryConfig(autoConf)
	if err != nil {
		return fmt.Errorf("in-place migration (#292): clear the repmgr recovery config from %s: %w", autoConf, err)
	}
	if inherited != "" {
		// Narrow optional interface rather than a Mechanism method: carrying a value the agent
		// did not choose is a migration concern, not a mechanism operation.
		type recoverySeeder interface {
			SeedManagedRecovery(primaryConninfo, slotName string) error
		}
		if s, ok := a.mech.(recoverySeeder); !ok {
			// Never silent: a mechanism that cannot seed leaves this node with standby.signal
			// and no upstream, which is the deadlock this whole block exists to prevent.
			a.log.Warn("in-place migration (#292): this mechanism cannot carry the inherited upstream forward; the node may come up as a standby with no primary_conninfo",
				"upstream", inherited)
		} else {
			// PRE-CREATE this agent's own slot on the inherited upstream (#298, observed live).
			// Seeding repmgr's slot into the fragment is not enough on its own: boot() runs
			// GenerateConfig immediately after this, and writeManagedConf always writes
			// n.SlotName alongside a non-empty conninfo -- so the fragment is rewritten to
			// pg_ha_slot_N within milliseconds, and that slot does not exist on a still-repmgrd
			// primary. A walreceiver whose named slot is missing does not fall back to slotless
			// streaming; it fails with `replication slot "..." does not exist` forever, which is
			// the same stalled rollout by another route. Creating it here makes the fragment
			// GenerateConfig is about to write valid, and it is the same call Follow would make
			// before pointing at an upstream.
			//
			// NOT best-effort, despite the seeding below (#298 review). GenerateConfig overwrites
			// primary_slot_name unconditionally -- deliberately, because pg_rewind copies the
			// source's fragment into this PGDATA and the source's slot must never be inherited --
			// so the seeded repmgr_slot_N does NOT survive to act as a fallback: a failed
			// pre-create lands the node on a slot that does not exist, i.e. exactly the stalled
			// rollout. Retried across the attempt budget, and loud at ERROR when it still fails,
			// because nothing downstream repairs it until an agent-held lease names a leader.
			slot := slotNameFor(a.cfg.PodName)
			if h := upstreamHost(inherited); h != "" && slot != "" {
				ci := pg.ConnInfo{Host: h, Port: 5432, User: a.cfg.RepmgrUser, DB: a.cfg.RepmgrDB, Password: a.cfg.RepmgrPassword}
				var cerr error
				for attempt := 1; attempt <= slotPrecreateAttempts; attempt++ {
					sctx, scancel := context.WithTimeout(context.Background(), 15*time.Second)
					cerr = a.prober.CreatePhysicalSlot(sctx, ci, slot)
					scancel()
					if cerr == nil {
						a.log.Info("in-place migration (#292): pre-created this node's replication slot on the inherited upstream",
							"upstream", h, "slot", slot, "attempt", attempt)
						break
					}
					a.log.Warn("in-place migration (#292): could not pre-create this node's slot on the inherited upstream; retrying",
						"upstream", h, "slot", slot, "attempt", attempt, "of", slotPrecreateAttempts, "err", cerr)
					if attempt < slotPrecreateAttempts {
						time.Sleep(slotPrecreateBackoff)
					}
				}
				if cerr != nil {
					a.log.Error("in-place migration (#292): could not pre-create this node's slot on the inherited upstream; GenerateConfig will still name it, so this standby will loop on `replication slot does not exist` until a leader is elected -- create it on the upstream by hand to unblock the roll",
						"upstream", h, "slot", slot, "err", cerr)
				}
			} else {
				a.log.Warn("in-place migration (#292): no host in the inherited upstream, or no parseable pod ordinal, so this node's slot cannot be pre-created",
					"upstream", inherited, "slot", slot)
			}
			// repmgr's OWN slot goes into the fragment, not this agent's ordinal slot: it is
			// already there on the upstream and already reserving WAL for this node, so it is the
			// correct value for the window before GenerateConfig runs.
			if seerr := s.SeedManagedRecovery(inherited, inheritedSlot); seerr != nil {
				a.log.Warn("in-place migration (#292): could not seed the inherited upstream into the agent fragment",
					"err", seerr)
			} else {
				a.log.Info("in-place migration (#292): carried the inherited upstream into the agent's fragment so this node keeps streaming through the roll",
					"upstream", inherited, "slot", inheritedSlot)
			}
		}
	}
	a.log.Info("in-place migration (#292): cleared the repmgr recovery config; the agent's own fragment is now authoritative",
		"path", autoConf, "removed", strings.Join(removed, ","),
		"note", "the legacy repmgr_slot_* on the upstream is reclaimed once this node is streaming through its own slot; the repmgr database, role and extension are left alone (opt-in cleanup)")
	return nil
}

// preflightPreload is run()'s single #293 entry point: strip, then verify. The order is
// what makes a direct 1.x -> repmgr-free-image jump survivable -- the strip removes the
// request before the check looks for it.
func (a *agent) preflightPreload() error {
	// The strip is NOT fatal (#294 review). Only assertPreloadedLibsPresent earns the exit: its
	// condition -- a library the image does not ship -- is a postmaster that will never start.
	// A failed strip is a different animal: it means a transient read/write problem (EIO, a
	// read-only remount, ENOSPC on the atomic temp write), and repmgr.so is still shipped today,
	// so leaving the line in place breaks nothing. Exiting there would CrashLoopBackOff a cluster
	// that would otherwise have run fine, before leader election, on a node that only needed the
	// next tick. The check below still catches it if the library really is missing.
	if err := a.dropRepmgrPreload(); err != nil {
		a.log.Warn("could not strip the repmgr preload from PGDATA; continuing (the presence check below is the backstop)", "err", err)
	}
	return a.assertPreloadedLibsPresent()
}

// dropRepmgrPreload removes `shared_preload_libraries = 'repmgr'` from PGDATA under the
// native mechanism (#293).
//
// The line is written INTO THE DATA DIRECTORY by images/pg-ha/entrypoint.sh at initdb
// time and cloned verbatim to every standby, so it outlives any chart change and any helm
// rollback -- the fix has to come from inside the running node. Once the repmgr package
// leaves the image (#290) a data directory still requesting repmgr.so is not a degraded
// cluster but a postmaster that refuses to start, on every pod at once.
//
// NATIVE ONLY, deliberately. A cluster running the repmgr-free image is native by
// definition (#294 deletes mechanism.Repmgr), so cleaning native nodes is sufficient to
// make that image safe -- while a node still on `mechanism: repmgr` keeps its preload,
// because the repmgr extension's own functions are what we would be gambling with and
// there is nothing to gain by touching it (#293).
//
// One call site is enough. run() calls this from preflightPreload before leader election,
// so it precedes boot() and therefore every sup.Start, and:
//   - A native standby cloned (or restored) from a still-dirty source inherits the line and
//     carries it until its next restart. Harmless while repmgr.so is present, and stripped
//     on that pod's next start, so the cluster converges without chasing the ~10 sup.Start
//     call sites.
//   - A cluster that skips this release entirely and jumps straight to the repmgr-free
//     image is still rescued: the strip runs before that pod's first start.
//
// An empty PGDATA needs no special case: postgresql.conf does not exist yet, and
// EnsureNoPreloadLibrary treats that as nothing to do.
func (a *agent) dropRepmgrPreload() error {
	// BOTH files, not just postgresql.conf (#293 review). postgresql.auto.conf also lives in
	// PGDATA, is read LAST -- so it wins over postgresql.conf and every conf.d fragment --
	// and is precisely where `ALTER SYSTEM SET shared_preload_libraries` lands. Cleaning only
	// postgresql.conf while the presence check below still treats auto.conf as fatal would
	// turn an admin's one-off ALTER SYSTEM into the exact outage this exists to prevent: an
	// unremediable CrashLoopBackOff on the repmgr-free image, fixable only by hand-editing a
	// file on the PVC of a pod that will not start. EnsurePrimaryConninfoDBName (#308) already
	// rewrites auto.conf from here, so this is an established file to own -- and the preflight
	// runs before any postmaster start, so nothing is competing for it.
	for _, name := range []string{"postgresql.conf", "postgresql.auto.conf"} {
		confPath := filepath.Join(a.cfg.PGDATA, name)
		changed, err := pgconf.EnsureNoPreloadLibrary(confPath, repmgrPreloadLib)
		if err != nil {
			return fmt.Errorf("remove the repmgr preload from %s: %w", name, err)
		}
		if changed {
			a.log.Info("removed repmgr from shared_preload_libraries in PGDATA; this cluster no longer needs repmgr.so (#293)",
				"path", confPath)
		}
	}
	return nil
}

// assertPreloadedLibsPresent fails the boot when the configuration still requests repmgr
// while the image does not ship repmgr.so (#293 acceptance).
//
// Without this the operator sees PostgreSQL's own `FATAL: could not access file "repmgr":
// No such file or directory` in a crash-loop, on every pod simultaneously, with nothing
// naming the cause or the fix -- and `helm rollback` does not help, because the offending
// line is in the data directory rather than the release. Ungated by mechanism: the trigger
// is "requested but genuinely absent", which is fatal either way.
//
// The scan covers the three places a preload can still come from after the strip above:
// postgresql.conf (a repmgr-mechanism node, or one the strip could not parse),
// postgresql.auto.conf (ALTER SYSTEM), and each conf.d fragment (the chart's own
// postgresql.configuration passthrough, which loads via include_dir and therefore
// OVERRIDES whatever postgresql.conf says). Modelled on the PG_MAJOR/postgres-binary
// mismatch check in newAgent, which fails startup for the same reason: a misconfiguration
// the pod cannot recover from is better reported than discovered.
func (a *agent) assertPreloadedLibsPresent() error {
	soPath := a.repmgrModulePath
	if _, err := os.Stat(soPath); err == nil {
		return nil // the library is here; whether anything uses it is not this check's business
	} else if !os.IsNotExist(err) {
		// A stat that fails for any other reason (permissions, a broken mount) is not
		// evidence of absence, and refusing to boot on it would be its own outage.
		a.log.Warn("could not stat the repmgr module; skipping the preload presence check", "path", soPath, "err", err)
		return nil
	}
	// Require positive evidence that we are looking in the RIGHT place before refusing to
	// start anything. This check's false positive is catastrophic and asymmetric: a wrong
	// module directory would make every repmgr-mechanism pod refuse to boot on a cluster
	// where repmgr.so is present and working -- far worse than the crash-loop it exists to
	// explain. An absent module directory is evidence of a bad path (a distro layout change,
	// a PG_MAJOR the image was not built for), not of an absent library, so downgrade to a
	// warning there. The directory existing but not holding repmgr.so is the real signal.
	moduleDir := filepath.Dir(soPath)
	if _, err := os.Stat(moduleDir); err != nil {
		a.log.Warn("the server module directory is not readable; skipping the preload presence check",
			"dir", moduleDir, "err", err)
		return nil
	}
	paths := []string{
		filepath.Join(a.cfg.PGDATA, "postgresql.conf"),
		filepath.Join(a.cfg.PGDATA, "postgresql.auto.conf"),
	}
	fragments, err := filepath.Glob(filepath.Join(confdDir, "*.conf"))
	if err != nil {
		return fmt.Errorf("boot: list %s: %w", confdDir, err)
	}
	paths = append(paths, fragments...)
	// An operator-set dynamic_library_path disarms the refusal entirely. PostgreSQL resolves
	// an unqualified entry against THAT path, not against pkglibdir alone, so a cluster
	// loading repmgr.so from an extra directory has a present, working library that the stat
	// above cannot see -- and refusing would hard-exit every pod over a library that loads
	// fine (#293 review). Same asymmetry as the missing-directory case: the cost of a false
	// refusal dwarfs the cost of falling back to PostgreSQL's own error message.
	for _, p := range paths {
		overridden, err := pgconf.SetsDynamicLibraryPath(p)
		if err != nil {
			return fmt.Errorf("check dynamic_library_path: %w", err)
		}
		if overridden {
			a.log.Warn("dynamic_library_path is set, so the module search path is not just pkglibdir; skipping the preload presence check",
				"path", p, "module", soPath)
			return nil
		}
	}
	for _, p := range paths {
		requested, err := pgconf.PreloadsLibrary(p, repmgrPreloadLib)
		if err != nil {
			return fmt.Errorf("check shared_preload_libraries: %w", err)
		}
		if requested {
			// #293 review made this remediation mechanism-specific, because telling a
			// repmgr-MECHANISM node to drop the library would have started the postmaster
			// without the repmgr extension's functions and silently disabled failover.
			// That branch is gone rather than renamed: #294 made MECHANISM=repmgr a hard
			// config.Load error, so a non-native mechanism can never reach this code -- the
			// agent exits before it is constructed. Dropping the library is now the only
			// correct advice, and a stale conditional here would be untested advice that
			// merely looks careful.
			fix := fmt.Sprintf("remove %q from shared_preload_libraries (in postgresql.configuration if the chart put it there, or from %s directly), then restart the pod", repmgrPreloadLib, p)
			return fmt.Errorf("refusing to start: %s sets shared_preload_libraries to include %q, but this image does not ship %s. "+
				"This is the #293 migration step: the line was written into the DATA DIRECTORY at initdb time by an older image, so it survives a chart downgrade and a helm rollback. "+
				"Fix: %s -- shared_preload_libraries is a postmaster parameter",
				p, repmgrPreloadLib, soPath, fix)
		}
	}
	return nil
}

// tlsVerifyInterval throttles the #335 `SHOW ssl` probe. Not every tick: this is a
// configuration property that changes only across a reload or a restart, so a per-tick psql
// (5s on chart defaults) would buy nothing and add a connection to every tick forever. Not
// once-per-boot either -- the gauge must be able to clear on its own after an operator fixes
// the config and reloads, and to re-arm if someone turns `ssl` off underneath a running server.
const tlsVerifyInterval = time.Minute

// slotPrecreateAttempts/slotPrecreateBackoff bound the repmgr-migration slot pre-create (#298
// review). Retried rather than one-shot because a failure is NOT self-healing: GenerateConfig
// names the slot regardless, so the standby loops on `replication slot does not exist` until
// somebody creates it. Bounded because this runs in the startup preflight, before leader
// election, where an unbounded wait is its own outage.
const (
	slotPrecreateAttempts = 3
	slotPrecreateBackoff  = 3 * time.Second
)

// durableTLInterval throttles the restartpoint check. Generous, because the condition it
// repairs arises only when this node starts following a new timeline.
// 15s, not a minute (#298, found by re-running the failover suite). The stamp is taken BEFORE the
// comparison -- deliberately, so a failing read cannot spin -- which means the FIRST tick after a
// boot consumes the whole interval, and at that moment the node has usually not started streaming
// the new timeline yet, so there is nothing to repair. The repair therefore landed one full
// interval later: observed as a standby that acquired the lease, refused on the highwater guard,
// and only became promotable 55s afterwards -- long enough that a failover window can close first
// (it made the disk-loss stage of test-agent-failover skip seven assertions). Four ticks is
// cheap: the steady-state check is a single SQL round trip.
const durableTLInterval = 15 * time.Second

// durableTLNoAdvanceInterval backs the check off after a restartpoint that did NOT move the
// durable timeline (#298 review).
//
// The no-op case is not free. PostgreSQL skips the restartpoint only when no NEWER checkpoint
// record has been replayed; a standby working through a replay backlog behind a timeline switch
// (or one held back by recovery_min_apply_delay) keeps replaying checkpoint records on the OLD
// timeline, so every CHECKPOINT performs a real restartpoint -- a full flush of dirty shared
// buffers -- while the durable timeline stays put because the switch itself has not been
// replayed yet. At durableTLInterval that is a forced checkpoint every 15 seconds on a node
// already saturated with replay I/O, for a repair that cannot land until replay catches up.
// Retrying a minute apart still records the timeline long before checkpoint_timeout (5 minutes on
// chart defaults) would have, which is the window this whole check exists to close.
const durableTLNoAdvanceInterval = time.Minute

// durableTLBudget bounds the WHOLE check, not each call inside it (#298 review).
//
// Every call here is individually capped by Prober.Timeout, but they are sequential: two reads
// plus a CHECKPOINT is 30s of worst case inside a tick whose /healthz staleness threshold is
// reconcileInterval*3 (15s on chart defaults). A tick that overruns it gets PID 1 -- and the
// postmaster it supervises -- SIGKILLed by the kubelet, which is the starvation shape every other
// blocking call in this file is bounded against. A CHECKPOINT is the one call here that is slow
// while perfectly healthy (a restartpoint flushes dirty shared buffers), so this is a real
// budget, not a theoretical one. Cutting psql short does not abort the server-side checkpoint; it
// keeps running and the next interval's read sees the result.
const durableTLBudget = 10 * time.Second

// ensureDurableTimeline forces a restartpoint when this standby's DURABLE timeline lags the one
// it is streaming (#298, found by the first live run of the repmgrd->2.0.0 migration).
//
// The promotion guards read durable state. A standby's control-file timeline advances only at a
// restartpoint, so after following a promotion it can stream the new timeline for up to
// checkpoint_timeout (5 minutes on chart defaults) while pg_control still records the old one.
// StandbyTimeline hides that WHILE the walreceiver is attached -- it takes the GREATEST including
// received_tli -- but that term disappears the instant the upstream dies, which is exactly when
// promotion is decided: the node then reads below the marker highwater and refuses (#125). On a
// freshly migrated cluster every standby is in that state at once, so the cluster could not fail
// over at all until something forced a checkpoint. Recording the timeline while the node is still
// streaming is what closes the window.
//
// The comparison is against the timeline the GUARD would read once the walreceiver detaches --
// GREATEST(checkpoint TLI, min_recovery_end_timeline) -- not against pg_controldata's checkpoint
// TLI alone (#298 review). min_recovery_end_timeline lives in the same control file, is part of
// the expression unsafeToServe reads, and advances on any flush during recovery, long before the
// next restartpoint. Measuring against the checkpoint TLI alone therefore called a node stale
// that was already promotable, and forced a full restartpoint on it every interval, forever.
//
// Best-effort and throttled: a failed checkpoint is not an incident, it just leaves the node in
// the state it was already in, and the next tick past the interval tries again.
func (a *agent) ensureDurableTimeline(ctx context.Context) {
	if !a.durableTLNextAt.IsZero() && time.Now().Before(a.durableTLNextAt) {
		return
	}
	a.durableTLNextAt = time.Now().Add(durableTLInterval)
	dctx, dcancel := context.WithTimeout(ctx, durableTLBudget)
	defer dcancel()
	self := a.selfConn()
	durable, reported, ok, rerr := a.prober.RecoveryTimelines(dctx, self)
	if rerr != nil || !ok || reported <= durable {
		return
	}
	if cerr := a.prober.Restartpoint(dctx, self); cerr != nil {
		a.log.Warn("could not force a restartpoint to record the streamed timeline; this node may refuse to promote until one happens on its own",
			"durableTimeline", uint32(durable), "streamedTimeline", uint32(reported), "err", cerr)
		return
	}
	// RE-READ, do not assume (#298 review). A CHECKPOINT on a standby returning success is not
	// proof the durable timeline moved: PostgreSQL SKIPS the restartpoint when no checkpoint
	// record NEWER than the last one has been replayed yet ("skipping restartpoint, already
	// performed at ..."), and the control file takes its timeline from that replayed record. A
	// standby that is streaming a new timeline but has not yet replayed a checkpoint on it is
	// exactly the case this function targets, so the no-op is the LIKELY outcome there -- and the
	// earlier form logged "now: <streamed>" regardless, asserting a repair that had not happened
	// while silently re-running the same no-op every interval.
	after, _, aok, aerr := a.prober.RecoveryTimelines(dctx, self)
	if aerr != nil || !aok {
		a.log.Warn("forced a restartpoint but could not re-read the control file to confirm the durable timeline advanced",
			"was", uint32(durable), "streamedTimeline", uint32(reported), "err", aerr)
		return
	}
	if after <= durable {
		// Back off rather than repeat this every interval: see durableTLNoAdvanceInterval. The
		// node is not promotable yet, but hammering a restartpoint does not make it so.
		a.durableTLNextAt = time.Now().Add(durableTLNoAdvanceInterval)
		a.log.Warn("forced a restartpoint but the durable timeline did not advance (PostgreSQL skips a restartpoint until a newer checkpoint record is replayed); this node still refuses to promote on the highwater guard (#125) and will retry",
			"durableTimeline", uint32(after), "streamedTimeline", uint32(reported), "retryIn", durableTLNoAdvanceInterval.String())
		return
	}
	a.log.Info("forced a restartpoint so the control file records the timeline this standby is streaming (#298)",
		"was", uint32(durable), "now", uint32(after))
}

// verifyTLSActive alarms when the operator asked for server TLS and the RUNNING postmaster is
// serving plaintext (#335).
//
// The failure this exists for is silent by construction: the chart renders `ssl = on` into the
// conf.d ConfigMap, mounts the certificate Secret, and the release goes Ready -- every signal an
// operator can read says TLS is on, while `SHOW ssl` says off and clients connect in the clear.
// #335 reported it as a missing conf.d include on a first-boot pod, but the include is only one
// of the ways to get here (an operator-declared `ssl` in postgresql.configuration, ALTER SYSTEM,
// an unreadable key) and the cost is identical in all of them. So this checks the OUTCOME, from
// the postmaster itself, rather than any of the inputs.
//
// Deliberately detection-only: it does NOT try to repair the include. Three writers already
// converge that line -- the entrypoint at initdb (both mechanisms), finishInitdbNative on a
// fresh native install, and the setup-config init container on every later boot -- so a missing
// include is repaired by the next pod start anyway, and appending `include_dir` here would put
// it AFTER the agent's own `include 'pg-ha-agent.conf'` on a running native node, silently
// handing an operator's wal_log_hints/hot_standby precedence over the agent's until the next
// GenerateConfig repositions it (the exact inversion Native.ensureInclude exists to prevent).
// Repairing the one cause that self-heals, at the price of a real regression in the others, is
// a bad trade; being loud is the fix #335 actually asks for.
//
// The loudness has three channels, because each reaches a different reader: the gauge
// (pg_ha_agent_tls_inactive) is what an alert can page on, the Error log names the cause and the
// remedy for whoever opens `kubectl logs`, and the readiness probe -- rendered by the chart, not
// here -- takes the pod out of the client-facing Services so nothing keeps talking plaintext to
// it. The headless Service sets publishNotReadyAddresses, so replication and the agent's own
// peer probes are unaffected by that.
func (a *agent) verifyTLSActive(ctx context.Context) {
	if !a.cfg.TLSEnabled {
		return
	}
	now := time.Now()
	if !a.tlsCheckedAt.IsZero() && now.Sub(a.tlsCheckedAt) < tlsVerifyInterval {
		return
	}
	a.tlsCheckedAt = now
	vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	on, err := a.prober.SSLActive(vctx, a.selfConn())
	if err != nil {
		// A failed query is NOT evidence of plaintext, and treating it as such would raise a
		// TLS alarm every time the server is merely busy or mid-restart -- the alarm would then
		// mean nothing when it mattered. Leave the gauge where it is and retry next interval.
		a.log.Warn("could not verify that server TLS is active; leaving the previous verdict in place", "err", err)
		return
	}
	a.metr.SetTLSInactive(!on)
	switch {
	case on && a.tlsInactiveLatched:
		a.tlsInactiveLatched = false
		a.log.Info("server TLS is active again", "ssl", "on")
	case !on && !a.tlsInactiveLatched:
		a.tlsInactiveLatched = true
		a.log.Error("postgresql.tls.enabled is set but this server reports `ssl = off` -- clients are being served in PLAINTEXT. "+
			"The usual cause is that PGDATA/postgresql.conf carries no `include_dir` for the chart's conf.d, so tls.conf was never read (#335); "+
			"an `ssl` set in postgresql.configuration or via ALTER SYSTEM does the same. "+
			"Fix: check `SHOW config_file` and that the file ends with include_dir = '"+confdDir+"', then reload -- `ssl` is a sighup parameter. "+
			"This pod is failing its readiness probe until then, so it serves no client traffic",
			"confd", confdDir)
	}
}

// ensurePrimaryConninfoDBName patches dbname=<repmgr db> into primary_conninfo in
// postgresql.auto.conf, where repmgr's own clone/follow/rejoin machinery writes it
// without one (confirmed live -- repmgr's physical-replication-only conninfo writer
// carries host/port/user/application_name, never dbname). PostgreSQL 17+'s
// sync_replication_slots worker requires dbname to be present (#308); physical
// replication itself works fine without it, so this is a correctness fix with no
// downside, safe to call unconditionally (a no-op when dbname is already set, or when
// there is no primary_conninfo line at all -- e.g. on a primary).
func (a *agent) ensurePrimaryConninfoDBName() (bool, error) {
	return pgconf.EnsurePrimaryConninfoDBName(filepath.Join(a.cfg.PGDATA, "postgresql.auto.conf"), a.cfg.RepmgrDB)
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
	// Bounded like the observe()-side reads (#298 review): this patch runs on the
	// reconcile goroutine every tick; unbounded it can stall the loop into a
	// liveness kill during an apiserver partition.
	pctx, pcancel := context.WithTimeout(ctx, a.fenceBudget())
	defer pcancel()
	if err := a.kube.PublishStatus(pctx, a.cfg.PodName, st); err != nil {
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

// beatDuring keeps the reconcile-loop heartbeat alive across one long mechanism
// operation (#298).
//
// The heartbeat is otherwise struck once per tick, at the top of tick(), while mechanism
// operations run inside act() under opMu -- so nothing beats while one runs. Fine for the
// bounded calls; fatal for pg_basebackup and pg_rewind, which legitimately take as long as
// the data takes to copy. /healthz goes stale at reconcileInterval*3 (15s on chart
// defaults) and liveness gives up after 10 x 10s, so the kubelet SIGKILLs the container --
// and the postmaster under it -- about 115s into any clone, on every retry.
//
// Deliberately NOT a free-running goroutine: /healthz must keep meaning "the reconcile loop
// is progressing", so the beat is armed only around a call known to be long, and a wedge
// anywhere else still goes stale and still gets restarted. The residual cost is that a hung
// pg_basebackup looks alive; the clone marker and discardTornClone recover that directory.
func (a *agent) beatDuring(fn func() error) error {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Floored: config.Load requires a positive interval, but a zero value reaches here
		// from a hand-built Config (tests), and NewTicker PANICS on one -- which for PID 1
		// is a crash-loop over the postmaster it supervises.
		every := a.cfg.ReconcileInterval
		if every <= 0 {
			every = time.Second
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				a.metr.Beat()
			}
		}
	}()
	err := fn()
	close(stop)
	<-done
	// One final beat, so the window between the last tick of the helper and the next real
	// tick cannot itself go stale on a long-running operation that finished just after a beat.
	a.metr.Beat()
	return err
}

// controlDataTimeout bounds every pg_controldata exec, matching Prober's own default SQL
// budget. pg_controldata reads a file, so it is normally instant -- but "reads a file" is
// precisely why it is not free: on a degraded PV (a stuck EBS volume, an NFS server that
// stopped answering) the read enters uninterruptible sleep and never returns.
const controlDataTimeout = 10 * time.Second

// readControlData runs pg_controldata under controlDataTimeout.
//
// Every caller passed act()'s ctx, which is the run loop's ROOT context and carries no
// deadline (#298 review). ReadControlData adds no timeout of its own -- unlike Prober.psql,
// which caps every SQL probe -- and OSExec's cmd.WaitDelay only takes effect AFTER the
// context is cancelled, so with a deadline-less context nothing bounded the exec at all.
// assertSameCluster is called from act()'s Follow branch and from rejoinOnto, both holding
// opMu: a wedged PGDATA blocked the reconcile goroutine indefinitely, metr.Beat() stopped so
// /healthz went stale, and dcs.OnLost -- which needs opMu to demote -- could never fence the
// node. That is the exact starvation shape every apiserver and psql call in this file is
// already bounded against.
func (a *agent) readControlData(ctx context.Context) (pg.ControlInfo, error) {
	cctx, cancel := context.WithTimeout(ctx, controlDataTimeout)
	defer cancel()
	return pg.ReadControlData(cctx, pg.OSExec{}, a.pgControlDataBin, a.cfg.PGDATA)
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
	cd, err := a.readControlData(ctx)
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

// assertSyncStandbySlots reconciles synchronized_standby_slots to the primary's current
// LIVE standby set (#308), so a logical failover slot's decode position tracks the
// live standby set through scale-up/down and promote. Idempotent and self-healing like
// assertPrimaryRouting/cleanupGhostNodes -- called from both Promote and StayPrimary,
// bounded by the same fence-budget context -- and best-effort (logged, never returned)
// so a slot-sync hiccup can never fail the routing assertion that follows it. No-op
// entirely when cfg.SyncReplicationSlots is false (byte-identical behavior to today).
//
// "Live" is the `owned` set reconcileSlots returns -- one slot per live pod ordinal, read
// from the Kubernetes API -- and deliberately NOT "the walsender is currently attached".
// (It was repmgr.nodes registration until #294 deleted that table; the live pod set is the
// same signal with no self-reported cache in the way.) An earlier revision derived the
// desired set from pg_replication_slots.active, which meant a standby restart, a rolling
// upgrade, or a brief network blip emptied synchronized_standby_slots -- and an empty value
// lets a logical slot's decode position advance past exactly the standby that is about to
// need it (a primary failure during that window silently diverges the subscriber from the
// new primary, the precise hazard this feature exists to prevent). A pod that is
// mid-restart is still in the live pod list, so this survives the same transients that
// broke the active-based version.
//
// Still intersected with the slots that actually EXIST: a pod can appear a moment before
// its physical slot is created, and naming a slot that does not exist is the same "blocks
// all logical decoding" failure this whole feature exists to prevent, so a live-but-not-yet-
// slotted standby is excluded until its slot shows up (typically the next tick).
// existing is the slot list slotsTick already read this tick, and read says whether that read
// succeeded (#289 widened PhysicalSlots from []string to []SlotState; this caller needs only
// existence). Passed in rather than re-queried: both run inside the same fenceBudget() window on
// a read-write node, and a second round-trip there competes with the marker write and the
// routing switch. An empty-but-successful read is NOT a skip -- see the unconditional first
// reconcile below.
func (a *agent) assertSyncStandbySlots(ctx context.Context, existing []pg.SlotState, owned []string, read bool) {
	if !a.cfg.SyncReplicationSlots {
		return
	}
	if !read {
		// Covers BOTH "the slot query failed" and "the live pod set was unreadable, so the owned
		// set is a guess" (#294 review). Either way this fails CLOSED: the GUC keeps whatever it
		// holds. Clearing it on an unreadable input would drop #308's guarantee on a healthy
		// cluster over one apiserver blip.
		a.log.Warn("could not establish this primary's owned slot set this tick; leaving synchronized_standby_slots as it is")
		return
	}
	existingSet := make(map[string]bool, len(existing))
	for _, s := range existing {
		existingSet[s.Name] = true
	}
	// `owned` IS the candidate set: reconcileSlots maintains exactly one slot per live
	// standby pod and returns them in ordinal order, and it is the same authority that
	// CREATES them, so the two cannot disagree (#294). nil is a legitimate answer -- a
	// single-node cluster owns no standby slots -- and must still reconcile, because
	// desired=="" is what clears a GUC left by a previous topology.
	// Only slots that ACTUALLY EXIST may be named. synchronized_standby_slots pointing at a
	// missing slot makes the primary refuse to release WAL and log repeatedly, so a slot that
	// was only just created this tick waits for the next one -- `existing` was read before
	// the create pass ran. Cheap: one extra tick of a standby not yet being waited on.
	var slots []string
	for _, name := range owned {
		if existingSet[name] {
			slots = append(slots, name)
		}
	}
	// Comma-joined is PostgreSQL's own list format for this GUC, and doubles as the
	// cache key: an unchanged standby set (the common steady-state tick) skips the
	// ALTER SYSTEM + reload entirely, rather than repeating it every 5s.
	desired := strings.Join(slots, ",")
	if a.lastSyncStandbySlots != nil && desired == *a.lastSyncStandbySlots {
		return
	}
	if err := a.prober.SetSynchronizedStandbySlots(ctx, a.selfConn(), slots); err != nil {
		a.log.Warn("set synchronized_standby_slots", "err", err, "slots", desired)
		return
	}
	a.lastSyncStandbySlots = &desired
	a.log.Info("reconciled synchronized_standby_slots", "slots", desired)
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
// directory (#175). Reached from the RejoinForward decision.
// rewindFailureLimit is how many consecutive non-divergence pg_rewind failures against one
// target are tolerated before rejoinOnto escalates to ReclonePreserving.
//
// Three, because the two failure shapes it has to separate live on very different timescales.
// A transient blip (source restarting, credentials mid-rotation, connections exhausted) clears
// within a tick or two, so three attempts paced by the rejoin path cost nothing and almost
// always avoid the re-clone. A permanent refusal never clears, and every extra attempt is
// another interval with this node out of the cluster -- so the limit stays small.
const rewindFailureLimit = 3

func (a *agent) rejoinOnto(ctx context.Context, target string) error {
	// Never rewind/re-clone onto a target that is not one of this cluster's pods
	// (#298 security review): rejoinOnto builds conninfos to it and dials with the
	// replication PGPASSWORD set.
	if !a.validPeerName(target) {
		return fmt.Errorf("refusing to rejoin onto %q: not a valid cluster pod name (possible tampering)", target)
	}
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
	a.standbyLastProgressLSN = pg.LSN{}
	// Invariant 9: never rewind/reclone onto a different cluster. Checked before the
	// demote so a healthy node is not stopped for a doomed rejoin.
	if err := a.assertSameCluster(ctx, target); err != nil {
		return err
	}
	// Bounded (see DemoteFence).
	dctx, dcancel := context.WithTimeout(ctx, a.cfg.RenewDeadline)
	derr := a.sup.Demote(dctx, true)
	dcancel()
	if derr != nil {
		return derr
	}
	if err := a.beatDuring(func() error { return a.mech.RejoinForceRewind(ctx, a.peerMechConn(target)) }); err != nil {
		// Escalate to a full re-clone ONLY on genuine divergence (#298 review).
		// RejoinForceRewind already classifies its failures -- ErrRewindDiverged for
		// "pg_rewind cannot proceed", a plain error for the transient ones (source
		// unreachable, slot provisioning, recovery-config write) whose contract is
		// "the caller retries next tick" -- but this caller escalated on ANY error,
		// paying a full re-clone plus a preserved .diverged.<ts> copy of PGDATA for a
		// network blip: exactly the #178 escalation the classifier exists to prevent.
		if errors.Is(err, mechanism.ErrRewindUnreachable) {
			// Could not CONNECT to the target: retry, and do NOT count it toward the backstop
			// below (#298 review). The backstop converges a permanent LOCAL refusal by
			// escalating; a target this node cannot reach is the one class where escalating
			// converges on nothing, because ReclonePreserving dials the same target with the
			// same credentials and fails the same way -- after renaming PGDATA aside and
			// leaving an unreaped .diverged.<ts> copy behind. Counting it turned three ticks
			// (~15s on chart defaults) of a restarting target -- or one whose pod name had not
			// propagated yet -- into a multi-hour base backup of a standby whose history was
			// fine. (A target that ACCEPTS the connection and then REFUSES the session -- too
			// many clients, no pg_hba entry, a rotated credential -- is this class TOO, and is
			// exempt for the same reason: ReclonePreserving dials the same source with the same
			// credentials through the same pg_hba, so it is refused identically. See
			// isSourceRejection. What still counts is a TARGET-side refusal, which a re-clone
			// genuinely does resolve.)
			// The streak is left ALONE rather than reset: an unreachable tick is no evidence
			// about a genuine local refusal that was already accumulating against this target.
			return err
		}
		if !errors.Is(err, mechanism.ErrRewindDiverged) {
			// Not divergence, so the data directory is not touched -- but count it. A refusal
			// that is permanent and non-divergent (pg_rewind wanting a config the target does
			// not have, say) would otherwise retry forever and leave this node out of the
			// cluster indefinitely, which is the failure mode the fail-safe classification
			// buys at the price of needing a backstop. After rewindFailureLimit consecutive
			// failures against the SAME target, re-clone: strictly slower and it preserves the
			// old directory (#175), but it always converges.
			if target != a.rewindFailureTarget {
				a.rewindFailureTarget, a.rewindFailures = target, 0
			}
			a.rewindFailures++
			if a.rewindFailures < rewindFailureLimit {
				return err
			}
			a.log.Warn("pg_rewind has failed repeatedly for a non-divergence reason; escalating to a data-preserving re-clone",
				"target", target, "attempts", a.rewindFailures, "last_err", err)
		}
		a.rewindFailures, a.rewindFailureTarget = 0, ""
		// Same marker as BootstrapClone (#288 review): ReclonePreserving runs the same
		// pg_basebackup, so an interrupted one leaves an equally torn PGDATA, and without the
		// marker discardTornClone is a no-op on the next boot.
		//
		// It does NOT bound PVC growth, which an earlier version of this comment claimed
		// (#288 review, second pass): ReclonePreserving renames PGDATA aside to
		// `.diverged.<ts>` and only removes it after the clone SUCCEEDS, so an interrupted
		// attempt still leaves that copy behind and nothing ever reaps it. The marker reclaims
		// the torn NEW copy; the preserved old one is a deliberate forensic artifact whose
		// cleanup is an operator task -- it is the only surviving copy of a diverged history
		// someone may need.
		a.beginClone()
		if err := a.beatDuring(func() error { return a.mech.ReclonePreserving(ctx, a.peerMechConn(target)) }); err != nil {
			a.discardTornClone(ctx)
			return err
		}
		a.endClone()
		// A full re-clone: this directory's contents now come from the peer, not from
		// whatever restore the record beside it describes.
		a.dropRestoreRecord("the data directory was re-cloned from " + target)
	} else {
		// A rewind that worked ends the streak (#298 review). Without this the counter only
		// ever cleared on the escalation itself, so two failed attempts followed by a SUCCESS
		// left it at 2: the next unrelated blip against the same primary counted as the third
		// consecutive failure and bought a full ReclonePreserving for a single transient error.
		// The backstop is for a refusal that is PERSISTENT, and a success proves it was not.
		a.rewindFailures, a.rewindFailureTarget = 0, ""
		// A successful pg_rewind ALSO rewrites this node's history onto the target's, so the
		// restore record beside PGDATA no longer describes these contents (#288 review).
		// Without this the claim outlived the data it described: after a controlled switchover
		// the rewound node kept its restore identity, so a later lease landing on it made it
		// skip a peer holding more WAL and promote anyway -- data loss with no restore in
		// flight. Only the node that still carries the restored history keeps the claim.
		a.dropRestoreRecord("the data directory was rewound onto " + target)
	}
	// #308: same dbname patch as BootstrapClone/Follow. Native writes primary_conninfo into
	// its OWN fragment (with dbname, via Conn.conninfo()), so this only ever matters for a
	// value inherited in postgresql.auto.conf from a 1.x volume that migrateForeignRecoveryConfig
	// has not yet cleared. Called unconditionally because it is a cheap no-op when dbname is
	// already present, and because being correct here must not depend on where the value came from.
	if _, err := a.ensurePrimaryConninfoDBName(); err != nil {
		a.log.Warn("ensure dbname in primary_conninfo", "err", err)
	}
	// RE-LATCH the upstream this rejoin just pointed replication at (#298 review). The latch
	// is invalidated on entry above, and leaving it empty leaks a slot: BOTH rejoin outcomes
	// provision THIS node's slot on `target` (RejoinForceRewind ends in Native.Follow ->
	// ensureSlotOnUpstream, and ReclonePreserving -> Clone -> ensureSlotOnUpstream), so when
	// the next tick re-homes this node -- which cascadeFollowTarget routinely does -- act()
	// calls releaseSlotOnFormerUpstream with an EMPTY former and returns at its `former == ""`
	// guard. orphanSlot deliberately keeps any slot whose ordinal has a live pod, so nothing
	// else reclaims it and an inactive slot pins WAL on the rejoin target until
	// max_slot_wal_keep_size invalidates it. This is the same reason BootstrapClone latches
	// dec.Target after its own clone. It also re-arms observeStandbyStall, which requires a
	// non-empty latch, so a standby that stalls right after a rejoin can escalate again.
	a.latchFollow(target)
	return a.sup.Start(ctx)
}

// newMechanism builds the replication mechanics (#287, #294).
//
// One implementation since #294 deleted mechanism.Repmgr. The Mechanism INTERFACE stays --
// policy (the Lease, the election, fencing, routing) lives in reconcile, which holds only the
// interface, and that seam is what made the repmgr-to-native migration survivable one method
// at a time. A rejected MECHANISM value is caught in config.Load, so this cannot silently run
// something other than what the operator asked for.
func newMechanism(cfg *config.Config, pgBindir string) mechanism.Mechanism {
	// PodName is passed twice by design: once derived into the slot name (#289) and once
	// verbatim as application_name (#288). Both are this node's identity, but they land in
	// different places -- pg_replication_slots and pg_stat_replication -- and the topology
	// probe reads whichever is available.
	return mechanism.NewNative(cfg.PGDATA, pgBindir, cfg.RepmgrPassword, slotNameFor(cfg.PodName), cfg.PodName)
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

// latchFollow records the upstream this node is now replicating from.
//
// It deliberately does NOT expire the restore claim (#288 review, second pass): see the
// adoption drop on the Promote path. Following a peer is not evidence the cluster adopted this
// volume's restored history -- under native a diverged standby reaches Follow with no rewind.
func (a *agent) latchFollow(target string) {
	a.followUpstream = target
	// Restart the stall window on every repoint (#294 review). The counter only cleared on an
	// OBSERVED streaming walreceiver or inside rejoinOnto, so it carried straight across a
	// Follow -- and the comment on StandbyStalled claiming "stallTicks buys the settling time"
	// was false whenever the counter was already at the threshold when the repoint arrived.
	//
	// That is the ordinary failover, not an edge case: the primary dies, this standby loses its
	// walreceiver and climbs past standbyStallTicks (~3 min) while `newer == nil` suppresses
	// escalation, then a peer promotes. On the very next tick after this latch the counter is
	// already over, the new walreceiver has not attached yet and replay has not moved -- so
	// StandbyStalled fires, `newer` is now non-nil, and Decide returns RejoinForward: postgres
	// stopped, pg_rewind, possibly a full ReclonePreserving leaving a .diverged.<ts> copy, on a
	// standby that would have been streaming seconds later.
	//
	// A repoint is exactly the event the window is meant to time, so it starts here.
	a.standbyNoReceiverTicks = 0
	a.standbyLastProgressLSN = pg.LSN{}
}

// releaseSlotOnFormerUpstream drops THIS node's slot on the upstream it just stopped
// streaming from (#294).
//
// Found on a live cascade, not in review: every standby's first clone comes from the PRIMARY
// (BootstrapClone targets the lease holder), so it provisions its slot there -- and then
// cascade re-homes it onto an intermediate standby. The slot on the primary is dead the
// moment that re-home completes, but nothing reclaimed it: the primary's own reclaim pass
// keeps any slot whose ordinal has a live pod, precisely so it cannot delete a child's slot
// while that child is briefly disconnected. So a HEALTHY three-tier cluster accumulated one
// inactive, WAL-retaining slot per cascaded node on its primary, until
// max_slot_wal_keep_size invalidated them and PGHAReplicationSlotInvalidated paged someone.
//
// The owner cleaning up after itself is the one solution that needs no cluster-wide view:
// this node knows both upstreams, and it knows it is no longer using the old slot. The
// upstream cannot know -- it cannot distinguish "my child moved" from "my child is
// restarting", which is exactly why its own policy has to be the conservative one.
//
// Cascade-only. Without cascading, a re-home means a FAILOVER, where the old upstream is the
// demoted (often unreachable) ex-primary; standbySlotsTick already reclaims everything there,
// and reaching for a dying node here would only add a timeout to every failover.
//
// Best-effort by design: DropPhysicalSlotIfInactive refuses an active slot, an unreachable
// old upstream just logs, and either way the upstream's own bounded policy remains the
// backstop. It must never fail the Follow that already succeeded.
func (a *agent) releaseSlotOnFormerUpstream(ctx context.Context, former, target string) {
	if !a.cfg.CascadeReplication {
		return
	}
	if former == "" || former == target || former == a.cfg.PodName {
		return
	}
	name := slotNameFor(a.cfg.PodName)
	dropped, err := a.prober.DropPhysicalSlotIfInactive(ctx, a.peerConn(former), name)
	if err != nil {
		a.log.Warn("release this node's slot on the upstream it left; its own reclaim pass is the backstop",
			"slot", name, "former_upstream", former, "new_upstream", target, "err", err)
		return
	}
	if dropped {
		a.log.Info("released this node's slot on the upstream it left",
			"slot", name, "former_upstream", former, "new_upstream", target)
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
//   - It is mechanism-independent by construction. pg_stat_replication is PostgreSQL's own
//     view, so it stays readable however the standby was configured -- including a standby
//     inherited from a 1.x repmgr cluster, whose application_name is its node name.
//
// topologyTick funds itself from its OWN budget rather than the caller's fence budget (#288 review).
// wctx is fenceBudget() -- 5s on chart defaults -- and by the time topologyTick runs it has
// already paid for RegisterPrimary, slotsTick, PrimaryWALPosition, advanceMarker and
// assertPrimaryRouting. Exhausted, ReplicationTopology fails and topologyTick returns early,
// LEAVING THE GAUGES AT THEIR PREVIOUS VALUES -- which, for a node that had just demoted and
// re-promoted, meant the zeroes ClearTopology wrote on the way through Follow: a freshly
// promoted primary with healthy standbys exporting replicas_streaming = 0. Since it now runs
// from tick() rather than from inside act(), it derives its own timeout from ctx: it cannot
// starve the marker write or the routing switch, and it cannot be starved BY them either.
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
	// The expected count needs the live pod set, and that is an uncached apiserver LIST.
	// To be accurate about the cost (#288 review): it is NOT free -- livePodOrdinals issues its
	// own uncached ListPodNames, and slotsTick already made one earlier in the same tick, so a
	// primary makes two per interval. What the review fixed is where it hurt: topologyTick runs
	// OUTSIDE opMu and the fence budget, so a slow LIST cannot delay a marker write, a
	// routing switch or a lost-leadership fence. The duplicate call is a gauge's cost, paid off
	// the critical path.
	// The live pod set from the API, not NodeCount: that env var is baked in at render time and
	// is stale on every pod that has not rolled yet (see orphanSlot).
	// tctx, not ctx (#288 review, round 2). tick()'s context is the run loop's -- no deadline --
	// and this LIST is uncached, so an apiserver that blackholes it would block the reconcile
	// goroutine indefinitely: no further metr.Beat(), /healthz stale after ~3x the interval, and
	// the kubelet kills the container over a gauge. The doc above claims this cannot be starved
	// BY the leadership writes; that only holds if the whole function is bounded.
	live, liveErr := a.livePodOrdinals(tctx)
	selfOrd, selfOK := podname.Ordinal(a.cfg.PodName)
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
		if pod := a.resolveReplicaPod(r); pod != "" {
			seen[pod] = true
		} else {
			unidentified++
		}
	}
	// DISTINCT pods, not rows (#288 review, round 2). expected counts pods, so streaming has to
	// as well or the two are not comparable: one pod can hold two streaming rows for a moment --
	// an old walsender not yet reaped alongside its reconnect, or the two identity sources
	// (application_name and slot ordinal) resolving the same pod during a rolling upgrade -- and
	// a per-row tally then reports streaming > expected with nothing missing and nothing
	// unidentified, which makes any `streaming != expected` alert flap on a healthy cluster.
	streaming = int64(len(seen)) + unidentified
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
	if ord, ok := podname.Ordinal(r.AppName); ok && r.AppName == fmt.Sprintf("%s-%d", a.base, ord) {
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
// (The split was load-bearing while repmgr also owned slot lifecycle and two writers would
// have fought over the same objects; #294 left the agent as the only owner, and the
// observe/reconcile separation stays because the two have different failure modes.)
// The returned slice is what observeSlots read, so a caller needing the same list (#308's
// assertSyncStandbySlots) reuses it instead of issuing a second psql on the same fence budget.
// The bool reports whether the read SUCCEEDED, which nil cannot: a primary with no slots at
// all returns an empty slice legitimately, and #308's first reconcile of a term must still run
// on that (desired=="" is a real value, not a skip).
func (a *agent) slotsTick(ctx context.Context) ([]pg.SlotState, []string, bool) {
	slots, ok := a.observeSlots(ctx)
	if !ok {
		return nil, nil, false
	}
	// ownedOK is folded into the returned read flag on purpose: #308's reconcile needs BOTH the
	// slot list and a trustworthy owned set, and it has one skip path for "could not look".
	owned, ownedOK := a.reconcileSlots(ctx, slots)
	return slots, owned, ownedOK
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
// The reclaim policy depends on whether a standby can legitimately BE an upstream, i.e. on
// cascadingReplication (#294).
//
// With cascade OFF the policy is the simpler one, and deliberately so: a standby has no
// legitimate downstream at all. Its own slot lives on its upstream, not locally, so every
// agent-minted slot found here is a leftover, with no pod set to consult.
//
// With cascade ON that premise is false -- a standby is the upstream for its own children, and
// their slots live HERE. Reclaiming every agent-minted slot would delete exactly the slots
// cascading depends on, every tick, on the node that owns them: a child whose walreceiver
// happens to be reconnecting is inactive for that instant, and `AND NOT active` would then let
// the drop through. So the primary's own predicate is used instead, which keeps any slot whose
// ordinal still has a live pod and reclaims only the ones whose pod is gone. That needs the
// live pod set, which is an uncached apiserver LIST -- paid only when cascade is on, and only
// on a standby, where nothing else in the tick needs it.
//
// The cost of that looser predicate, stated plainly: a DEMOTED primary running with cascade on
// keeps the slots it minted for peers that now stream from the new primary, because their pods
// are still live. Those slots go inactive and hold WAL on this node -- the very leak the
// paragraph above describes. It is bounded, not unbounded: the entrypoint sets
// max_slot_wal_keep_size = 4GB at initdb, so PostgreSQL invalidates such a slot rather than
// filling the volume, and PGHAReplicationSlotInvalidated reports it. Distinguishing "this
// child is momentarily disconnected" from "this peer moved to another upstream" needs a
// cluster-wide view of who follows whom, which no single node has; given the choice, holding
// bounded WAL beats dropping a slot a returning child still needs, because that costs it a
// re-clone.
//
// A failure to read the pod set means nothing is reclaimed this tick, for the same reason.
//
// Observation runs under every mechanism (the gauges must be truthful about a standby
// holding WAL back too); mutation is native-only, same as the primary path.
func (a *agent) standbySlotsTick(ctx context.Context) {
	slots, ok := a.observeSlots(ctx)
	if !ok {
		return
	}
	var live map[int]bool
	isUpstream := false
	if a.cfg.CascadeReplication {
		var err error
		live, err = a.livePodOrdinals(ctx)
		if err != nil {
			a.log.Warn("list live pods for the standby slot reclaim; skipping it this tick (cascading replication makes this node a legitimate upstream)", "err", err)
			return
		}
		// Is anything actually streaming FROM this node right now? That single fact separates the
		// two cascade cases a legacy slot here can belong to (#290 review):
		//
		//   - A DEMOTED ex-primary: its former children have moved to the new primary, so nothing
		//     streams from it. Its repmgr_slot_* are pure residue and must be reclaimed, or they
		//     sit inactive pinning WAL on its volume forever.
		//   - A cascade UPSTREAM mid-migration: a grandchild really does stream from here, and its
		//     repmgr_slot_<id> is the slot it is using. Dropping that is the full re-clone the
		//     migration gate exists to prevent.
		//
		// An unreadable topology is treated as "might be an upstream", which is the conservative
		// direction: holding bounded WAL beats destroying a slot in use.
		rows, err := a.prober.ReplicationTopology(ctx, a.selfConn())
		if err != nil {
			a.log.Warn("read this node's downstreams for the standby slot reclaim; assuming it is an upstream", "err", err)
			isUpstream = true
		} else {
			isUpstream = len(rows) > 0
		}
	}
	migrated := migratedOrdinals(slots)
	for _, sl := range slots {
		if !a.reclaimableOnStandby(sl.Name, live, migrated, isUpstream) {
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

// reclaimableOnStandby applies the right reclaim predicate for this node's topology (#294).
// live is nil when cascading is off, in which case the ordinal carries no information and
// every agent-minted slot is a leftover.
func (a *agent) reclaimableOnStandby(name string, live map[int]bool, migrated map[int]bool, isUpstream bool) bool {
	// Without cascade this node cannot be anyone's upstream, so every agent-minted or legacy slot
	// found locally is a leftover -- its own slot lives on its upstream, not here.
	if !a.cfg.CascadeReplication {
		return leftoverStandbySlot(name)
	}
	// With cascade on, a LEGACY repmgr_slot_* here has two possible owners, and `isUpstream`
	// distinguishes them (#290 review, round 2). An earlier cut declared all of them residue,
	// which was right for a demoted ex-primary and wrong for a cascade upstream: a grandchild
	// mid-migration really is streaming through its repmgr_slot_<id> on this node, and dropping
	// it forces the re-clone the whole migration exists to avoid.
	if strings.HasPrefix(name, legacySlotPrefix) {
		if !isUpstream {
			return true // nothing streams from here: residue from a term as primary
		}
		// Something does stream from here, so fall through to the conservative per-ordinal rule
		// below -- keep it while its pod is live and unmigrated.
	}
	return orphanSlot(name, a.cfg.PodName, live, migrated)
}

// leftoverStandbySlot reports whether a slot found on a STANDBY is one this agent may
// reclaim when it cannot be anyone's upstream (#289 review): any name it can prove it
// minted, plus any legacy repmgr slot.
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
// cross-node coordination to get wrong. Callers must also skip it while paused; act() only
// reaches the primary branches when not paused.
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
func (a *agent) reconcileSlots(ctx context.Context, slots []pg.SlotState) ([]string, bool) {
	if a.cfg.NodeCount <= 0 {
		return nil, false
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

	selfOrd, selfOK := podname.Ordinal(a.cfg.PodName)
	// Under cascading replication the primary must NOT pre-create a slot per live ordinal
	// (#294). A cascaded standby streams from a PEER, so its slot belongs on that peer, and the
	// one minted here would sit inactive forever -- retaining WAL on the primary until
	// max_slot_wal_keep_size invalidates it, which is precisely the failure #289 exists to
	// prevent, and it would fire the invalidated-slot alert on a perfectly healthy cluster.
	//
	// Nothing is lost by skipping it: every slot-using path in the native mechanism (Clone,
	// Follow, RejoinForceRewind) calls ensureSlotOnUpstream first, so a follower provisions its
	// own slot on whichever upstream it actually points at, before pointing at it. Pre-creation
	// is belt-and-braces for the star topology, not the mechanism that makes slots work.
	//
	// The reclaim pass below still runs, and still governs both topologies.
	preCreate := !a.cfg.CascadeReplication
	// Names whose invalidated-slot RECYCLE failed after the drop had already taken effect, so
	// the pre-pass snapshot below over-reports them as present (#298 review; see the create
	// loop's error branch).
	recycleFailed := map[string]bool{}
	for ord := 0; preCreate && ord < a.cfg.NodeCount; ord++ {
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
		// An INVALIDATED slot counts as ABSENT here, or CreatePhysicalSlot's recycle branch
		// is unreachable from this pass (#298 review). wal_status = 'lost' means PostgreSQL
		// destroyed the reservation at max_slot_wal_keep_size, and such a slot can never be
		// acquired again -- but it is a slot, so a bare presence check skipped the create and
		// nothing else on the PRIMARY reclaimed it either: orphanSlot deliberately keeps any
		// slot whose ordinal still has a live pod, precisely so a briefly disconnected child
		// is not deprived of its slot. The dead reservation therefore stood forever, with
		// pg_ha_agent_replication_slots_invalidated (and its alert) latched on a cluster the
		// agent could have repaired in one statement.
		//
		// Only when NOT active, mirroring the SQL guard: an invalidated slot something still
		// holds is left alone, and the drop inside CreatePhysicalSlot re-checks the predicate
		// atomically anyway.
		cur, present := have[name]
		dead := present && cur.Invalidated() && !cur.Active
		if present && !dead {
			continue
		}
		if err := a.prober.CreatePhysicalSlot(ctx, self, name); err != nil {
			a.log.Warn("create replication slot", "slot", name, "err", err)
			if dead {
				// A FAILED recycle may have left the name GONE (#298 review). The drop and the
				// create are two statements in one psql call and pg_drop_replication_slot is not
				// transactional, so a statement/context deadline, a dropped connection or an
				// exhausted max_replication_slots between them removes the slot and returns an
				// error. ownedStandbySlots derives #308's synchronized_standby_slots from the
				// PRE-PASS snapshot, which still lists it -- and naming a slot that does not
				// exist is the one thing that GUC must never do: the primary then refuses to
				// release WAL and logs about it every checkpoint until the next tick re-reads
				// the real slot list. Drop it from the snapshot instead.
				recycleFailed[name] = true
			}
			continue
		}
		if dead {
			// Warn, not Info: the slot is usable again but the standby behind it still needs a
			// full re-clone, and the recycle itself clears the invalidated gauge -- so the
			// PGHAReplicationSlotInvalidated alert (for: 5m) can no longer catch this. The log
			// line is the operator's remaining trace of it.
			// Counted as well as logged (#298 review): the recycle clears the invalidated
			// GAUGE in the same tick that observed it, so PGHAReplicationSlotInvalidated's
			// `for: 5m` can never elapse for a slot the agent repairs. The counter is what
			// PGHAReplicationSlotRecycled alerts on, and it is the only durable signal that
			// the standby behind this ordinal needs a re-clone.
			a.metr.IncSlotRecycled()
			a.log.Warn("recycled an invalidated replication slot for a live standby; the standby behind it still needs a full re-clone", "slot", name)
			continue
		}
		a.log.Info("created replication slot for an expected standby", "slot", name)
	}

	if liveErr != nil {
		// No trustworthy live set: nothing is reclaimed, and #308 gets NO ANSWER rather than a
		// guessed one -- hence the false. Returning a bare nil here made "could not tell" and
		// "owns no standby slots" the same value, so one apiserver blip on a healthy 3-node
		// cluster cleared synchronized_standby_slots entirely and reinstated it the next tick:
		// the exact guarantee #308 exists for, dropped, with GUC churn per blip (#294 review).
		// The pre-#294 code failed CLOSED here; this restores that.
		return nil, false
	}
	migrated := migratedOrdinals(slots)
	for _, s := range slots {
		if !orphanSlot(s.Name, a.cfg.PodName, live, migrated) {
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
	// Filtered here rather than in ownedStandbySlots: the drop pass above still wants the full
	// observed list (a name it cannot see is a name it cannot reclaim), and assertSyncStandbySlots
	// intersects the owned set with the same snapshot -- so removing the name from `owned` is
	// enough to keep the GUC off a slot that no longer exists.
	if len(recycleFailed) > 0 {
		kept := make([]pg.SlotState, 0, len(slots))
		for _, s := range slots {
			if !recycleFailed[s.Name] {
				kept = append(kept, s)
			}
		}
		slots = kept
	}
	return a.ownedStandbySlots(slots, live), true
}

// ownedStandbySlots reduces the slots observed on this node to the ones it maintains for a
// live standby, in ordinal order -- the answer #308's synchronized_standby_slots reconcile
// needs (#294).
//
// Derived from what is ACTUALLY PRESENT rather than from the create loop's expectations, for
// two reasons. It is correct under cascading replication, where the primary pre-creates
// nothing at all and its direct children self-provision, so an expectation-driven list would
// be empty and would clear the GUC on a healthy chain. And it cannot name a slot that does not
// exist, which is the one thing synchronized_standby_slots must never do: the primary then
// refuses to release WAL and logs about it every checkpoint.
//
// Ordinal order, because the caller joins the result into a string and uses it as a
// change-detection key -- an unstable order would rewrite the GUC every tick.
func (a *agent) ownedStandbySlots(slots []pg.SlotState, live map[int]bool) []string {
	type owned struct {
		ord  int
		name string
	}
	var found []owned
	for _, s := range slots {
		// Same predicate as the reclaim pass, inverted: anything it would keep, this waits on.
		// Sharing orphanSlot is what stops the two drifting into disagreement about which
		// slots belong to a live standby.
		// migrated is irrelevant here: the legacy-slot branch it feeds cannot be reached, because
		// the slotPrefix filter below already excludes every legacy name from the waited-on set.
		if orphanSlot(s.Name, a.cfg.PodName, live, nil) {
			continue
		}
		ord, ok := slotOrdinal(s.Name)
		if !ok || !strings.HasPrefix(s.Name, slotPrefix) {
			continue // an operator's slot, or a legacy repmgr one: not ours to wait on
		}
		found = append(found, owned{ord, s.Name})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].ord < found[j].ord })
	names := make([]string, 0, len(found))
	for _, f := range found {
		names = append(names, f.name)
	}
	return names
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
		if ord, ok := podname.Ordinal(n); ok {
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
// from target's FQDN (#182). Both the standby's primary_conninfo and a.fqdn build the
// upstream host the same way (pod name + headless service), so sender_host equals
// a.fqdn(target); compared case-insensitively with any trailing dot trimmed. A probe
// error or any mismatch returns false -- the caller then runs Follow, which rewrites
// primary_conninfo and reloads (a no-op repoint if it was already attached), so a false
// negative only costs one extra follow, never a missed repoint.
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

// nodeIDBase is the repmgr node_id of ordinal 0 (node_id = nodeIDBase + ordinal), as
// init-repmgr.sh assigned them.
//
// #288's audit of this offset listed every consumer and which had to outlive
// mechanism.Repmgr. #294 settled it: nodeID(), ghostNodeIDs(), NodeIdentity.NodeID,
// Conn.NodeID and the #297 registry gate are all gone with the mechanism that read them, and
// exactly one consumer survives -- slotOrdinal()'s legacy branch, which reverses the offset to
// reclaim repmgr_slot_<node_id> orphans a repmgr-created cluster leaves behind (#292). Deleting
// the constant with the mechanism would silently strand those slots, pinning WAL forever, so it
// stays for as long as a cluster can carry one.
//
// Pod-ordinal parsing itself was never repmgr-specific; since #298 it lives in
// internal/podname, which both this file and reconcile use.
const nodeIDBase = 1000

// upstreamHost pulls the host out of a libpq conninfo string, unwrapping the single quotes
// repmgr writes around it. "" when there is none (#298).
func upstreamHost(conninfo string) string {
	for _, tok := range strings.Fields(conninfo) {
		if v, ok := strings.CutPrefix(tok, "host="); ok {
			return strings.Trim(v, "'")
		}
	}
	return ""
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
	ord, ok := podname.Ordinal(pod)
	if !ok {
		return ""
	}
	return slotPrefix + strconv.Itoa(ord)
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
func orphanSlot(name, selfPod string, liveOrdinals map[int]bool, migratedOrdinals map[int]bool) bool {
	selfOrd, selfOK := podname.Ordinal(selfPod)

	ord, ok := slotOrdinal(name)
	if !ok {
		return false // not a name this agent minted, and not a legacy repmgr slot
	}
	// This pod's own slot, either naming scheme: nobody streams from a node to itself, so it is
	// unused regardless of liveness or migration state. Checked FIRST -- putting it after the
	// legacy branch below left a migrated primary's own repmgr_slot_<self> standing forever,
	// because a primary never has an active new-scheme slot for its own ordinal to prove it.
	if selfOK && ord == selfOrd {
		return true
	}
	// A legacy repmgr_slot_<node_id> is never USED by native mode, but "unused" is not the same
	// as "safe to drop yet" (#292). During an in-place migration the standby that owns one is
	// mid-restart: the old slot has gone inactive and its new pg_ha_slot_<ordinal> may not exist
	// yet, so `AND NOT active` alone lets the drop through -- and if the primary then recycles
	// WAL before that standby reattaches, it needs exactly the full re-clone #292 exists to
	// avoid.
	//
	// An earlier round made these unconditionally reclaimable, on the grounds that scoping them
	// to departed ordinals leaves a permanent orphan per surviving node. That objection is
	// answered rather than reverted: reclaim on departed OR MIGRATED, where migrated means the
	// ordinal's new-scheme slot is present and ACTIVE -- positive proof the standby is streaming
	// through the new one and finished with the old. So the orphan is still cleaned up, just not
	// during the window where dropping it costs a re-clone.
	if strings.HasPrefix(name, legacySlotPrefix) {
		if len(liveOrdinals) > 0 && !liveOrdinals[ord] {
			return true // the pod is gone; nothing can be relying on it
		}
		return migratedOrdinals[ord]
	}
	if len(liveOrdinals) == 0 {
		return false
	}
	return !liveOrdinals[ord]
}

// migratedOrdinals returns the ordinals whose NEW-SCHEME slot is present and active, i.e. the
// standbys provably streaming through the native naming (#292). That is the only positive signal
// a primary has that a legacy repmgr slot for the same ordinal is finished with.
//
// Active, not merely present: reconcileSlots pre-creates a slot before its standby arrives, so
// presence alone would clear the legacy slot while the standby was still using it.
func migratedOrdinals(slots []pg.SlotState) map[int]bool {
	m := map[int]bool{}
	for _, s := range slots {
		if !s.Active || !strings.HasPrefix(s.Name, slotPrefix) {
			continue
		}
		if ord, ok := slotOrdinal(s.Name); ok {
			m[ord] = true
		}
	}
	return m
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
		// Bounded at BOTH ends (#298 review). The lower bound rejects a number that cannot be
		// one of this chart's node_ids at all; the upper one rejects a number that could only
		// be someone else's. The chart mints node_id = nodeIDBase + ordinal, so an ordinal is
		// only representable while it stays below nodeIDBase -- past that the ids would run
		// into a second base range and stop being unique. Without the upper bound an operator's
		// own hand-made `repmgr_slot_9999` mapped to ordinal 8999, which no live pod can ever
		// claim, so it read as a DEPARTED node and became reclaimable: the one outcome the
		// name-based gate exists to prevent. Self-healing is not good enough when the healing
		// is "the slot is already gone".
		if err != nil || n < nodeIDBase || n-nodeIDBase >= nodeIDBase {
			return 0, false
		}
		return n - nodeIDBase, true
	}
	return 0, false
}
