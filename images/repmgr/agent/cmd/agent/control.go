package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cagriekin/pg-ha-agent/internal/control"
	"github.com/cagriekin/pg-ha-agent/internal/k8s"
	"github.com/cagriekin/pg-ha-agent/internal/pg"
	"github.com/cagriekin/pg-ha-agent/internal/pgbackrest"
	"github.com/cagriekin/pg-ha-agent/internal/process"
	"github.com/cagriekin/pg-ha-agent/internal/reconcile"
)

// Control-API wiring (#276). The API is a facade over state the reconcile loop
// already computes and over intents the loop already honours, so everything here is
// adaptation: publish a snapshot per tick, and hand node-local operations to the loop
// instead of performing them on the HTTP goroutine.

// restoreLogTailLines / restoreLogMaxBytes bound the opt-in progress log read.
const (
	restoreLogTailLines = 400
	restoreLogMaxBytes  = 256 << 10
)

// intentRequest is a control-API operation waiting for the reconcile loop. done is
// buffered so the loop never blocks on a caller that has already given up.
type intentRequest struct {
	kind control.IntentKind
	done chan error
}

// runIntent performs a node-local operation. Only ever called from the reconcile
// goroutine (the run loop selects on the intent channel), so it cannot interleave
// with a tick; opMu still serialises it against the leadership OnLost fence, which
// fires on its own goroutine.
func (a *agent) runIntent(ctx context.Context, kind control.IntentKind) error {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	switch kind {
	case control.IntentReload:
		return a.sup.Reload(ctx)
	case control.IntentRestart:
		// Stop and start explicitly rather than stopping and letting the loop start it
		// back up: under maintenance mode the loop is a no-op, so relying on it would
		// leave Postgres down.
		if err := a.sup.Demote(ctx, false); err != nil {
			return fmt.Errorf("stop postgres: %w", err)
		}
		if err := a.sup.Start(ctx); err != nil {
			return fmt.Errorf("start postgres: %w", err)
		}
		a.log.Info("control: restarted postgres")
		return nil
	case control.IntentStop:
		// Leave it stopped: the caller is about to restore over this data directory. Safe
		// only while paused, which the handler verified against a fresh marker read --
		// an active loop would start it again on the next tick.
		if err := a.sup.Demote(ctx, false); err != nil {
			return fmt.Errorf("stop postgres: %w", err)
		}
		a.log.Warn("control: stopped postgres for a restore")
		return nil
	case control.IntentReinitialize:
		// Stop, then discard the data directory. The reconcile loop takes it from here:
		// an empty PGDATA on a node that is not the chosen primary is exactly the
		// BootstrapClone case, so the rebuild runs through the same path a brand-new
		// replica uses. WipeDataDir carries its own interlocks (initialized data
		// directory, no postmaster.pid, not near the filesystem root).
		if err := a.sup.Demote(ctx, false); err != nil {
			return fmt.Errorf("stop postgres: %w", err)
		}
		if err := process.WipeDataDir(a.cfg.PGDATA); err != nil {
			return err
		}
		a.log.Warn("control: discarded the data directory; the reconcile loop will re-clone from the lease holder",
			"pgdata", a.cfg.PGDATA)
		return nil
	default:
		return fmt.Errorf("unknown intent %v", kind)
	}
}

// --- control.Node ---

type nodeAPI struct{ a *agent }

func (n nodeAPI) Snapshot() control.Snapshot {
	if s := n.a.snap.Load(); s != nil {
		return *s
	}
	// Before the first tick has published anything, report identity only. Every
	// position field stays zero and ObservedAt stays zero, which the API surfaces as a
	// zero age rather than inventing state.
	return control.Snapshot{Node: n.a.cfg.PodName, PGMajor: n.a.cfg.PGMajor}
}

// Submit enqueues an intent and waits for the loop to run it.
//
// If ctx expires after the intent was accepted, the loop still performs it -- which is
// why the timeout response says the operation may be in progress rather than claiming
// it did not happen.
func (n nodeAPI) Submit(ctx context.Context, kind control.IntentKind) error {
	req := intentRequest{kind: kind, done: make(chan error, 1)}
	select {
	case n.a.intents <- req:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// --- control.Cluster ---

type clusterAPI struct{ a *agent }

func (c clusterAPI) Marker(ctx context.Context) (control.MarkerView, error) {
	m, err := c.a.kube.ReadMarker(ctx, c.a.cfg.MarkerName)
	if err != nil {
		return control.MarkerView{}, err
	}
	return control.MarkerView{
		Present:          m.Present,
		Paused:           m.Paused,
		PausedBy:         m.PausedBy,
		SwitchoverTarget: m.SwitchoverTarget,
		Primary:          m.Primary,
	}, nil
}

func (c clusterAPI) SetPause(ctx context.Context, on bool, requestedBy string) error {
	return c.a.kube.SetPause(ctx, c.a.cfg.MarkerName, on, requestedBy)
}

func (c clusterAPI) SetSwitchoverTarget(ctx context.Context, target, requestedBy string) error {
	return c.a.kube.SetSwitchoverTarget(ctx, c.a.cfg.MarkerName, target, requestedBy)
}

func (c clusterAPI) ClearSwitchoverTarget(ctx context.Context) error {
	return c.a.kube.ClearSwitchoverTarget(ctx, c.a.cfg.MarkerName)
}

// --- control.Backups ---

type backupAPI struct{ a *agent }

func (b backupAPI) RestoreEnabled() bool { return b.a.cfg.ControlRestoreEnabled }

func (b backupAPI) Info(ctx context.Context) (json.RawMessage, error) {
	return b.a.pgbr.Info(ctx)
}

// restoreEnvOverrides is the ONLY part of the cloned Job the agent changes. Each key
// is a plain literal env the chart's restore.sh already reads.
//
// FORCE is deliberately absent. That flag bypasses pgBackRest's postmaster.pid
// interlock -- the last guard against restoring over a live volume -- so the API
// neither sets nor clears it: whatever pgbackrest.restore.force renders (a reviewed,
// git-visible decision) is what applies. An HTTP request cannot reach for it.
func (b backupAPI) restoreEnvOverrides(req control.RestoreRequest) map[string]string {
	ov := map[string]string{
		"TARGET_TYPE": req.TargetType,
		"TARGET":      req.Target,
		"BACKUP_SET":  req.BackupSet,
	}
	if b.a.cfg.ControlRestoreReadPodLogs {
		// pgBackRest logs its per-file lines (which carry the cumulative percentage) at
		// detail level only. Raising it here rather than in the chart keeps the verbose
		// output confined to API-triggered restores.
		ov["PGBACKREST_LOG_LEVEL_CONSOLE"] = "detail"
	}
	return ov
}

func (b backupAPI) Restore(ctx context.Context, req control.RestoreRequest, id control.Identity) (control.RestoreView, error) {
	jv, err := b.a.kube.CloneCronJobToJob(ctx,
		b.a.cfg.ControlRestoreCronJob, b.a.cfg.ControlRestoreJobName,
		b.restoreEnvOverrides(req), id.CN, time.Now())
	if err != nil {
		return control.RestoreView{}, err
	}
	b.a.log.Warn("control: created restore Job over the live data directory",
		"job", jv.Name, "client_cn", id.CN, "client_fingerprint", id.Fingerprint,
		"targetType", req.TargetType, "backupSet", req.BackupSet)
	return b.view(ctx, jv, k8s.PodView{})
}

func (b backupAPI) RestoreStatus(ctx context.Context) (control.RestoreView, error) {
	jv, err := b.a.kube.GetJob(ctx, b.a.cfg.ControlRestoreJobName)
	if err != nil {
		return control.RestoreView{}, err
	}
	var pod k8s.PodView
	if jv.Present {
		p, perr := b.a.kube.JobPod(ctx, jv.Name)
		if perr != nil {
			// The Job's own status is still worth returning; note the gap rather than
			// failing the whole call.
			b.a.log.Warn("control: could not read the restore pod", "err", perr)
		} else {
			pod = p
		}
	}
	return b.view(ctx, jv, pod)
}

// view assembles the API's restore state, including the last recorded outcome (which
// outlives the Job) and, when opted in, live progress from the pod's log.
func (b backupAPI) view(ctx context.Context, jv k8s.JobView, pod k8s.PodView) (control.RestoreView, error) {
	v := control.RestoreView{
		Phase:            restorePhase(jv, pod),
		JobName:          jv.Name,
		CreatedAt:        jv.CreatedAt,
		StartedAt:        jv.StartTime,
		CompletedAt:      jv.CompletionTime,
		Active:           jv.Active,
		Succeeded:        jv.Succeeded,
		Failed:           jv.Failed,
		RequestedBy:      jv.RequestedBy,
		RequestedAt:      jv.RequestedAt,
		PodName:          pod.Name,
		PodPhase:         pod.Phase,
		WaitingReason:    pod.WaitingReason,
		WaitingMessage:   pod.WaitingMessage,
		ContainerStarted: pod.ContainerStarted,
	}
	if rec, err := b.a.pgbr.LastRestore(); err != nil {
		b.a.log.Warn("control: could not read the restore status file", "err", err)
	} else if rec.Present {
		v.LastRestore = &rec
	}
	if b.a.cfg.ControlRestoreReadPodLogs && v.Phase == "running" && pod.Present {
		// Container name is left empty: the clone requires exactly one container, so
		// there is nothing to disambiguate and no chart-side name to hardcode.
		if log, err := b.a.kube.PodLogTail(ctx, pod.Name, "", restoreLogTailLines, restoreLogMaxBytes); err != nil {
			b.a.log.Warn("control: could not read restore progress from the pod log", "err", err)
		} else {
			p := pgbackrest.ParseProgress(log)
			v.Progress = &p
		}
	}
	return v, nil
}

func (b backupAPI) DeleteRestore(ctx context.Context) error {
	return b.a.kube.DeleteJob(ctx, b.a.cfg.ControlRestoreJobName)
}

// restorePhase collapses Job status plus pod state into one word. A Job whose pod has
// not started is "pending" -- the state a restore sits in until the StatefulSet is
// scaled down and the data volume can attach.
func restorePhase(jv k8s.JobView, pod k8s.PodView) string {
	switch {
	case !jv.Present:
		return "none"
	case jv.Succeeded > 0:
		return "succeeded"
	case jv.Failed > 0 && jv.Active == 0:
		return "failed"
	case pod.ContainerStarted:
		return "running"
	default:
		return "pending"
	}
}

// --- snapshot publication ---

// publishSnapshot records what the API serves, once per tick. Called only when the
// control API is enabled, so a release without it does exactly the work it did
// before -- including no extra probe for replay progress.
func (a *agent) publishSnapshot(ctx context.Context, obs reconcile.Observation, dec reconcile.Decision) {
	snap := control.Snapshot{
		Node:             a.cfg.PodName,
		PGMajor:          a.cfg.PGMajor,
		HoldsLease:       obs.HoldLease,
		LeaseHolder:      obs.LeaderIdentity,
		Paused:           obs.Paused,
		SwitchoverTarget: obs.SwitchoverTarget,
		MarkerPresent:    obs.Marker.Present,
		MarkerPrimary:    obs.Marker.Primary,
		Local: control.MemberState{
			Name:       a.cfg.PodName,
			Self:       true,
			Role:       localRole(obs.Local),
			Reachable:  obs.Local.Running,
			Running:    obs.Local.Running,
			HasData:    obs.Local.HasData,
			Timeline:   uint32(obs.Local.Timeline),
			TimelineOK: obs.Local.TimelineOK,
			LSN:        lsnString(obs.Local.LSN, obs.Local.LSNOK),
		},
		Decision: control.DecisionView{
			Action: dec.Action.String(), Target: dec.Target, Reason: dec.Reason, At: time.Now(),
		},
		ObservedAt: time.Now(),
	}
	for _, p := range obs.Peers {
		snap.Peers = append(snap.Peers, control.MemberState{
			Name:       p.Name,
			Role:       p.Role.String(),
			Reachable:  p.Reachable,
			Running:    p.Reachable,
			Timeline:   uint32(p.Timeline),
			TimelineOK: p.TimelineOK,
			LSN:        lsnString(p.LSN, p.LSNOK),
			Gossip:     p.Gossip,
		})
	}
	// Replay progress is the answer to "when can I use this database" after a restore,
	// and it is only meaningful while recovering -- so the extra probe is spent only
	// then.
	if obs.Local.Running && obs.Local.InRecovery {
		if rp, err := a.prober.ReplayProgress(ctx, a.selfConn()); err != nil {
			a.log.Warn("control: read replay progress", "err", err)
		} else {
			snap.Recovery = control.RecoveryView{
				InRecovery:     rp.InRecovery,
				ReceiveLSN:     lsnString(rp.ReceiveLSN, rp.ReceiveOK),
				ReplayLSN:      lsnString(rp.ReplayLSN, rp.ReplayOK),
				LastReplayTime: rp.LastReplayTime,
			}
			if lag, ok := rp.ReplayLagBytes(); ok {
				snap.Recovery.ReplayLagBytes = &lag
			}
		}
	}
	if a.cfg.PGBackrestEnabled {
		if rec, err := a.pgbr.LastRestore(); err != nil {
			a.log.Warn("control: read restore status file", "err", err)
		} else {
			snap.LastRestore = rec
		}
	}
	a.snap.Store(&snap)
}

// localRole names this node's role in the API's vocabulary. A stopped node reports
// "unknown" rather than guessing from its on-disk state, which is exactly the
// ambiguity the reconcile loop exists to resolve.
func localRole(ls reconcile.LocalState) string {
	switch {
	case !ls.Running:
		return "unknown"
	case ls.InRecovery:
		return pg.RoleStandby.String()
	default:
		return pg.RolePrimary.String()
	}
}

func lsnString(l pg.LSN, ok bool) string {
	if !ok {
		return ""
	}
	return fmt.Sprintf("%X/%X", l.Hi, l.Lo)
}

// startControl builds and runs the control API. Any failure here is fatal by design:
// an operator who enabled the API and got a running agent without it would believe a
// port is protected when it is simply absent.
func (a *agent) startControl(ctx context.Context) error {
	var backups control.Backups
	if a.cfg.PGBackrestEnabled {
		backups = backupAPI{a: a}
	}
	srv, err := control.New(control.Options{
		Addr:              a.cfg.ControlAddr,
		CertFile:          a.cfg.ControlCertFile,
		KeyFile:           a.cfg.ControlKeyFile,
		CAFile:            a.cfg.ControlCAFile,
		AllowedCNs:        a.cfg.ControlAllowedCNs,
		RestoreAllowedCNs: a.cfg.ControlRestoreAllowedCNs,
		ClusterName:       a.base,
		PodName:           a.cfg.PodName,
		Namespace:         a.cfg.Namespace,
		RestoreTargetPod:  fmt.Sprintf("%s-%d", a.base, a.cfg.ControlRestorePodOrdinal),
		RestorePodOrdinal: a.cfg.ControlRestorePodOrdinal,
		RestoreJobName:    a.cfg.ControlRestoreJobName,
		Log:               a.log,
		Cluster:           clusterAPI{a: a},
		Node:              nodeAPI{a: a},
		Backups:           backups,
		Metrics:           a.metr,
		// A node-local intent waits at most one reconcile interval to be picked up plus
		// the operation itself; scale the budget with the interval so a slow (cloud
		// preset) cluster does not report spurious timeouts.
		IntentTimeout: intentBudget(a.cfg.ReconcileInterval),
	})
	if err != nil {
		return err
	}
	go func() {
		if serr := srv.Serve(ctx); serr != nil {
			// Losing the control listener leaves HA intact (the reconcile loop is
			// untouched), so this is loud but not fatal to the database.
			a.log.Error("control API stopped", "err", serr)
		}
	}()
	return nil
}

// intentBudget is how long a control request waits for the reconcile loop.
func intentBudget(reconcileInterval time.Duration) time.Duration {
	const floor = 30 * time.Second
	if b := 4 * reconcileInterval; b > floor {
		return b
	}
	return floor
}

// newPgbackrest builds the pgBackRest client for the control API's read-only backup
// routes. Safe to construct unconditionally: nothing runs until a route is called.
func newPgbackrest(stanza, statusPath string) pgbackrest.Client {
	return pgbackrest.Client{Exec: pg.OSExec{}, Stanza: stanza, StatusPath: statusPath}
}
