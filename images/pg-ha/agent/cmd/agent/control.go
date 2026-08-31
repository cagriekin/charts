package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	// How long to wait for a deleted restore Job to disappear. Its pods get the default
	// 30s termination grace, so this allows for that plus apiserver latency.
	restoreDeleteTimeout = 90 * time.Second
	// minIntentStopBudget is the smallest graceful-shutdown window runIntent will give the
	// postmaster, however stale the request deadline it inherited is (#298 review). See the
	// floor in runIntent for why a raw past deadline is a zero-grace SIGKILL.
	minIntentStopBudget = 15 * time.Second
)

// intentRequest is a control-API operation waiting for the reconcile loop. done is
// buffered so the loop never blocks on a caller that has already given up.
//
// deadline carries the REQUEST's deadline into the loop. Without it the operation would
// run under the process-lifetime root context, so a postmaster that ignores SIGINT would
// hold opMu until shutdown: the loop stops ticking (peer gossip goes stale, the highwater
// marker stops advancing) and the leadership OnLost fence blocks behind the same mutex, so
// a node that comes back read-write is never demoted. A deadline makes the stop escalate
// to SIGKILL exactly as the fence path does. Zero means "no deadline" (the client sent no
// timeout), which is the caller's choice to make.
type intentRequest struct {
	kind     control.IntentKind
	deadline time.Time
	done     chan error
}

// runIntent performs a node-local operation. Only ever called from the reconcile
// goroutine (the run loop selects on the intent channel), so it cannot interleave
// with a tick; opMu still serialises it against the leadership OnLost fence, which
// fires on its own goroutine.
//
// parent is the loop's root context: it bounds the operation by process lifetime, while
// req.deadline bounds the part that can wedge (stopping the postmaster).
func (a *agent) runIntent(parent context.Context, req intentRequest) error {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	kind := req.kind
	// Only the stop is deadline-bounded. Start and Reload do not block on the child, and
	// bounding them would add nothing but a way to fail.
	stopCtx, cancelStop := parent, context.CancelFunc(func() {})
	if !req.deadline.IsZero() {
		// FLOORED, not used raw (#298 review). req.deadline is copied from the HTTP request
		// context and the request deliberately stays queued after the caller gives up ("if
		// ctx expires after the intent was accepted, the loop still performs it") -- so by
		// the time the loop dequeues it, the deadline is routinely in the PAST. A past
		// deadline makes context.WithDeadline return an already-cancelled context, and
		// ChildPostmaster.Stop then takes its `case <-ctx.Done()` arm on the first select:
		// SIGKILL immediately after the SIGINT, with zero grace. An operator POSTing
		// /v1/node/restart while act() sits in a 60s pg_ctl promote therefore got a 504 and,
		// a moment later, a SIGKILLed read-write primary forced into crash recovery -- on the
		// restart they asked to be graceful. IntentStop likewise reported a failure nobody
		// saw. The floor keeps the wedge bounded (which is all the deadline is for) while
		// guaranteeing a real shutdown attempt.
		budget := time.Until(req.deadline)
		if budget < minIntentStopBudget {
			budget = minIntentStopBudget
		}
		stopCtx, cancelStop = context.WithTimeout(parent, budget)
	}
	defer cancelStop()
	switch kind {
	case control.IntentReload:
		return a.sup.Reload(parent)
	case control.IntentRestart:
		// Stop and start explicitly rather than stopping and letting the loop start it
		// back up: under maintenance mode the loop is a no-op, so relying on it would
		// leave Postgres down.
		if err := a.sup.Demote(stopCtx, false); err != nil {
			// A deadline-forced SIGKILL still leaves the postmaster dead and reaped, so
			// the restart must go on and bring it back up -- returning here would leave
			// the node down for a wedge this deliberately escalated past. Any other
			// failure, or a child still alive, is fatal to the operation.
			// stopProvedDead is that test, shared with the fence, the planned shutdown and
			// RestartLocal so the four cannot drift (#298 review).
			if !a.stopProvedDead(err) {
				return fmt.Errorf("stop postgres: %w", err)
			}
			a.log.Warn("control: postmaster did not exit before the request deadline and was killed; starting it back up",
				"deadline", req.deadline)
		}
		// ASSERT the on-disk role before starting (#298 review). sup.Start is a bare
		// `postgres -D PGDATA`: it neither creates nor removes standby.signal, so this was
		// the one path that started a postmaster in whatever role the directory happened to
		// carry -- while every start the reconcile loop performs goes through StartLocal
		// (which asserts the signal for standby-state data) or StartRecovery (which asserts
		// it for a NON-HOLDER's primary-state data, "so its true position is observable
		// without risking a second writer").
		//
		// The gap is reachable through the documented path, not by misuse. A fenced
		// ex-primary is stopped with primary-state data and no standby.signal, does not hold
		// the lease, and publishes Role "unknown" -- so handleIntent's force gate (which only
		// bites while HoldsLeaseNow()) waves an unforced restart straight through, and the
		// node came up READ-WRITE on the old timeline beside the real primary until the next
		// tick's DemoteFence: a two-writer window of up to one reconcile interval. The
		// restore runbook's own hint ("POST /v1/restart to bring it back") points at it, and
		// the same shape is reachable on a directory whose signal was lost inside
		// RejoinForceRewind's window.
		if err := a.assertRestartRecoverySignal(parent); err != nil {
			return err
		}
		if err := a.sup.Start(parent); err != nil {
			return fmt.Errorf("start postgres: %w", err)
		}
		a.log.Info("control: restarted postgres")
		return nil
	case control.IntentStop:
		// Leave it stopped: the caller is about to restore over this data directory. Safe
		// only while paused, which the handler verified against a fresh marker read --
		// an active loop would start it again on the next tick.
		//
		// Unlike restart, a deadline-forced kill is reported as a FAILURE here: SIGKILL
		// leaves postmaster.pid behind, and that file is what stops the restore Job from
		// writing over a directory something may still own. The caller must see the stop
		// did not complete cleanly rather than have a Job created behind it.
		if err := a.sup.Demote(stopCtx, false); err != nil {
			return fmt.Errorf("stop postgres: %w", err)
		}
		a.log.Warn("control: stopped postgres for a restore")
		return nil
	case control.IntentReinitialize:
		// RE-ASSERT the replica-only gate here, in the reconcile goroutine (#298 review). handleReinitialize
		// checks it carefully -- live lease read, durable marker, snapshot role -- but every one
		// of those runs on the HTTP goroutine BEFORE Submit queues the intent, and the loop's
		// select may serve a tick first. The request also deliberately stays queued after the
		// caller's context expires, so the gap is not bounded by the HTTP timeout.
		//
		// The scenario is the ordinary failover: an operator reinitializes a healthy standby, the
		// primary dies in that window, this node wins the lease and the next tick promotes it --
		// then runIntent dequeues and wipes the data directory of the cluster's new primary.
		// WipeDataDir's postmaster.pid interlock cannot help, because the Demote below has just
		// removed it. The marker then names an empty node, which handleReinitialize's own comment
		// identifies as unrecoverable.
		//
		// Here the check IS atomic against the promote it guards, for TWO independent reasons
		// (#298 review, round 5 -- an earlier revision of this comment claimed runIntent does not
		// take opMu, which is wrong: it takes it at the top of this function). First, runIntent
		// runs in the reconcile loop's own select, i.e. the same goroutine as tick(), so no tick
		// can interleave between this gate and the wipe and nothing can promote in between.
		// Second, opMu is held for the whole call, which is what also excludes the OnLost fence.
		// The goroutine argument is the one that matters here, and it is also why reading
		// a.lastMarker unsynchronised is safe: observe() is its only writer and runs on this
		// goroutine. The lease is read live; the marker is the loop's own last successful read
		// (the handler already paid for a fresh one, and this asks the cheaper question of
		// whether anything changed since). servingRW is atomic, and is the one field OnLost --
		// which does run elsewhere, under opMu -- can also touch.
		if a.dcs.IsLeader() {
			return fmt.Errorf("refusing to discard %s: this node acquired the leader lease after the request was accepted, so it is now the cluster's primary (reinitialize is replica-only)", a.cfg.PGDATA)
		}
		if a.servingRW.Load() {
			return fmt.Errorf("refusing to discard %s: this node is marked read-write, so a live primary may be serving from it (reinitialize is replica-only)", a.cfg.PGDATA)
		}
		if a.lastMarkerOK && a.lastMarker.Primary == a.cfg.PodName {
			return fmt.Errorf("refusing to discard %s: the primary marker now records %s as the primary (reinitialize is replica-only)", a.cfg.PGDATA, a.cfg.PodName)
		}
		// Stop, then discard the data directory. The reconcile loop takes it from here:
		// an empty PGDATA on a node that is not the chosen primary is exactly the
		// BootstrapClone case, so the rebuild runs through the same path a brand-new
		// replica uses. WipeDataDir carries its own interlocks (initialized data
		// directory, no postmaster.pid, not near the filesystem root).
		if err := a.sup.Demote(stopCtx, false); err != nil {
			return fmt.Errorf("stop postgres: %w", err)
		}
		if err := process.WipeDataDir(a.cfg.PGDATA); err != nil {
			return err
		}
		a.dropRestoreRecord("the data directory was discarded for a rebuild")
		a.log.Warn("control: discarded the data directory; the reconcile loop will re-clone from the lease holder",
			"pgdata", a.cfg.PGDATA)
		return nil
	default:
		return fmt.Errorf("unknown intent %v", kind)
	}
}

// assertRestartRecoverySignal makes the on-disk role explicit before the control-API restart
// starts the postmaster, mirroring act()'s StartLocal / StartRecovery arms (#298 review).
//
// It only ever ADDS standby.signal, never removes one. Asserting the file is what closes the
// two-writer window described at the call site; REMOVING it would be a new way to bring a node
// up read-write, and this path has none of the guards act() applies before it does that (the
// #125 highwater check in particular). So a holder with primary-state data is left exactly as it
// is -- byte-identical to the previous behaviour -- and every other shape is pinned read-only.
//
// Best-effort on an unreadable control file, in the SAFE direction: if pg_controldata cannot be
// read the role is unknown, and a standby that merely waits for WAL is recoverable while a second
// writer is not.
func (a *agent) assertRestartRecoverySignal(ctx context.Context) error {
	if !process.HasData(a.cfg.PGDATA) {
		return nil // nothing to start in a role; the loop will bootstrap or clone
	}
	holdsLease := a.dcs.IsLeader()
	cd, err := a.readControlData(ctx)
	switch {
	case err != nil:
		a.log.Warn("control restart: could not read pg_controldata; starting read-only (standby.signal) rather than risk a second writer", "err", err)
	case cd.InRecovery:
		// standby-state data: assert what the control file already says, exactly as StartLocal
		// does. InRecovery comes from pg_control, never from the file, so the two can disagree.
	case holdsLease:
		// primary-state data on the lease holder: act()'s StartLocal would clear the signal and
		// come up read-write here, but only after unsafeToServe. Leave the directory untouched.
		return nil
	default:
		a.log.Warn("control restart: primary-state data on a node that does not hold the lease; starting read-only (standby.signal) so it cannot become a second writer",
			"node", a.cfg.PodName)
	}
	// SAY SO when a pgBackRest restore's recovery.signal is also present (#298 review). When
	// both files exist PostgreSQL takes STANDBY mode -- standby.signal wins -- so the restore's
	// recovery_target/--target-action=promote is not honoured and the node sits in recovery
	// instead of coming up as a primary at the target. That is still the right answer here (a
	// non-holder that promoted itself at the target would be a second writer, which is the whole
	// point of this function), but it is the single most confusing shape an operator can hit on
	// the documented restore-then-POST-/v1/restart path, so it must not be silent: the fix is to
	// restart on the pod that holds the lease, which this function leaves untouched.
	if _, serr := os.Stat(filepath.Join(a.cfg.PGDATA, "recovery.signal")); serr == nil {
		a.log.Warn("control restart: recovery.signal is present, so this data directory came from a restore -- adding standby.signal makes PostgreSQL take STANDBY mode and the restore's recovery target will NOT promote. Restart on the pod that holds the leader lease if you meant to bring the restored data up read-write",
			"node", a.cfg.PodName, "pgdata", a.cfg.PGDATA)
	}
	if err := process.SetRecoverySignal(a.cfg.PGDATA); err != nil {
		return fmt.Errorf("control restart: assert standby.signal before starting: %w", err)
	}
	return nil
}

// dropRestoreRecord invalidates the restore-outcome record when this volume's contents
// stop being what that record describes.
//
// The record lives BESIDE PGDATA (it has to outlive the Job that wrote it and the
// directory that Job rewrites), so neither WipeDataDir nor a clone touches it. Left in
// place it would make GET /v1/status report a backup set and recovery target as the
// provenance of a data directory that was rebuilt from the primary -- and the field is
// documented as exactly that provenance, so anything reasoning or alerting on it would be
// wrong.
//
// Best effort by design: the rebuild has already happened by the time this runs, and a
// misleading record is not worth failing a completed clone over. The warning names the
// path so an operator can remove it by hand.
func (a *agent) dropRestoreRecord(why string) {
	p := a.cfg.RestoreStatusPath()
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			return
		}
		a.log.Warn("could not remove the stale restore record; GET /v1/status may report it as this volume's provenance",
			"path", p, "reason", why, "err", err)
		return
	}
	a.log.Info("removed the restore record: it no longer describes this data directory",
		"path", p, "reason", why)
}

// adoptRestoreIfServing stamps the restore record as adopted when this node is serving as
// primary and still carries an unexpired claim (#288 review, round 4).
//
// obs.Local.RestoredAt IS the claim -- localRestoredAt returns "" once adoptedAt is set -- so it
// both decides whether there is anything to do and makes the write happen once per restore
// rather than on every primary tick.
//
// Serving as primary is the adoption event: the cluster is running on this volume's history,
// which is what the claim existed to argue for. Deliberately not on Follow -- a restored pod
// following another primary is still WAITING for the handoff the claim wins, and expiring it
// there loses the restore (see the Promote path).
func (a *agent) adoptRestoreIfServing(obs reconcile.Observation) {
	if obs.Local.RestoredAt == "" {
		return
	}
	a.markRestoreAdopted()
}

// markRestoreAdopted expires the restore record's ELECTION claim while keeping the record
// itself, for a volume whose restored history the cluster has adopted. Best-effort: a failure
// only leaves the claim standing, which the position ranking then has to out-argue -- strictly
// safer than losing the provenance.
func (a *agent) markRestoreAdopted() {
	ts := time.Now().UTC().Format(time.RFC3339)
	if err := a.pgbr.MarkAdopted(ts); err != nil {
		a.log.Warn("could not stamp the restore record as adopted; its election claim stays in force",
			"path", a.cfg.RestoreStatusPath(), "err", err)
		return
	}
	a.log.Info("stamped the restore record as adopted: this node is serving on the restored history",
		"path", a.cfg.RestoreStatusPath(), "adoptedAt", ts)
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

// HoldsLeaseNow reports the LIVE leadership state from the DCS. Destructive gates use
// this rather than the snapshot, which is unpopulated until the first tick publishes --
// and the control listener is already accepting requests by then.
func (n nodeAPI) HoldsLeaseNow() bool { return n.a.dcs.IsLeader() }

// Submit enqueues an intent and waits for the loop to run it.
//
// If ctx expires after the intent was accepted, the loop still performs it -- which is
// why the timeout response says the operation may be in progress rather than claiming
// it did not happen. The deadline travels WITH the request so "still performs it" stays
// bounded: without it a wedged postmaster would hold the loop (and the leadership fence
// behind the same mutex) for the life of the process.
func (n nodeAPI) Submit(ctx context.Context, kind control.IntentKind) error {
	deadline, _ := ctx.Deadline() // zero when the caller set none
	req := intentRequest{kind: kind, deadline: deadline, done: make(chan error, 1)}
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
	ov := map[string]string{}
	// Override ONLY what the request specified. Unconditionally writing these would blank
	// a recovery point the release pinned in values when a request omits it -- restoring
	// the latest backup and replaying all WAL instead of stopping at the reviewed point in
	// time, over the live data directory. Values supply the default; the request overrides
	// it; the response reports whichever is in effect.
	if req.TargetType != "" {
		// targetType and target are validated to arrive together, so setting both here is
		// never a half-override.
		ov["TARGET_TYPE"] = req.TargetType
		ov["TARGET"] = req.Target
	}
	if req.BackupSet != "" {
		ov["BACKUP_SET"] = req.BackupSet
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
		"effectiveTargetType", jv.TargetType, "effectiveTarget", jv.Target,
		"effectiveBackupSet", jv.BackupSet)
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
		Phase:       restorePhase(jv, pod),
		JobName:     jv.Name,
		CreatedAt:   jv.CreatedAt,
		StartedAt:   jv.StartTime,
		CompletedAt: jv.CompletionTime,
		Active:      jv.Active,
		Succeeded:   jv.Succeeded,
		Failed:      jv.Failed,
		RequestedBy: jv.RequestedBy,
		RequestedAt: jv.RequestedAt,
		// Whatever the created Job really carries, request-supplied or values-pinned.
		EffectiveTargetType: jv.TargetType,
		EffectiveTarget:     jv.Target,
		EffectiveBackupSet:  jv.BackupSet,
		PodName:             pod.Name,
		PodPhase:            pod.Phase,
		WaitingReason:       pod.WaitingReason,
		WaitingMessage:      pod.WaitingMessage,
		ContainerStarted:    pod.ContainerStarted,
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

// DeleteRestore removes the restore Job and waits for it to be GONE. Foreground
// propagation returns as soon as the finalizer is set, so without the wait a replace
// (delete-then-create of the same deterministic name) hits AlreadyExists -- after the
// handler has already stopped PostgreSQL, leaving the operator with a stopped database
// and a 502 mid-incident.
// DETACHED from the caller's context and given its own budget (#298 review), the same way
// handleRestore's own point-of-no-return calls are. Both callers pass r.Context(), which
// control.limitMW has already capped at RequestTimeout -- 60s on chart defaults, since the
// IntentTimeout+15s floor is 45s there -- so the 90s wait below could never run to completion:
// WaitJobGone took its `case <-ctx.Done()` arm at ~60s and returned a bare
// "context deadline exceeded", discarding the real reason. That made a slow-terminating Job an
// intermittent 502 on DELETE /v1/restore and on {"replace":true}, and made WaitJobGone's own
// "still failing after ...: %w" message (which exists to name a permanent failure such as
// missing RBAC) unreachable on this path.
func (b backupAPI) DeleteRestore(ctx context.Context) error {
	// +15s so the budget covers the delete call itself, not only the wait.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreDeleteTimeout+15*time.Second)
	defer cancel()
	if err := b.a.kube.DeleteJob(dctx, b.a.cfg.ControlRestoreJobName); err != nil {
		return err
	}
	return b.a.kube.WaitJobGone(dctx, b.a.cfg.ControlRestoreJobName, restoreDeleteTimeout)
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
	// Only this node's data-directory state is observable; peers' MemberState.HasData
	// stays nil (absent), because a cross-pod probe does not report it and emitting false
	// would misreport every peer -- including a primary plainly holding data.
	hasData := obs.Local.HasData
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
			HasData:    &hasData,
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
		// Set explicitly rather than left to defaultRequestTO (#298 review). DeleteRestore
		// is the widest operation the API performs -- restoreDeleteTimeout (90s) for the
		// deleted Job's pods to finish their 30s termination grace, plus apiserver latency --
		// and the whole-request cap has to be wider than it or the wait is cut short and the
		// real error replaced by "context deadline exceeded". It also sets the response-write
		// budget (Server.writeTimeout = this + 30s), so the reply to a genuinely slow delete
		// still reaches the client. maxConcurrent (16) is what bounds concurrency; this is
		// only a per-request ceiling on an mTLS, CN-allowlisted surface.
		RequestTimeout: restoreDeleteTimeout + 30*time.Second,
		// The detached delete leg, so writeTimeout can include a budget RequestTimeout does
		// not bound (#298 review). Same expression DeleteRestore builds its context from.
		DetachedTimeout: restoreDeleteTimeout + 15*time.Second,
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
