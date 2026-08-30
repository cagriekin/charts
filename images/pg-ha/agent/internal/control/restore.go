package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// featureGate answers "not configured" before "not authorized" for the restore
// routes. Without it, a release that never enabled restore would answer 403 (the
// restore allowlist is empty by construction), sending the operator to look for a
// certificate problem instead of a values flag.
// It sits OUTSIDE guard, so it short-circuits the authorization check -- deliberately, so
// an operator who simply forgot the values flag gets an actionable 501 instead of a 403 for
// an allowlist that is empty by construction while the feature is off. Because guard is
// therefore skipped, this must emit the audit line and the rejection counter itself:
// otherwise probing of the most destructive routes would leave no trace in either the audit
// log or pg_ha_agent_control_rejected_total.
func (s *Server) featureGate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		refuse := func(msg, hint, detail string) {
			s.o.Metrics.IncControlRejected()
			s.audit(r, identityFrom(r.Context()), VerbRestore, "not-configured", detail)
			writeErr(w, http.StatusNotImplemented, msg, hint)
		}
		if s.o.Backups == nil {
			refuse("pgBackRest is not enabled for this release",
				"set pgbackrest.enabled=true to use the backup and restore endpoints",
				"pgbackrest disabled")
			return
		}
		if !s.o.Backups.RestoreEnabled() {
			refuse("restore triggering is not enabled",
				"set ha.agent.control.restore.enabled=true (it is a separate opt-in from the control API: it grants the pods Job-creation RBAC)",
				"restore triggering disabled")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) (int, string) {
	if s.o.Backups == nil {
		writeErr(w, http.StatusNotImplemented, "pgBackRest is not enabled for this release",
			"set pgbackrest.enabled=true to use the backup endpoints")
		return http.StatusNotImplemented, "pgbackrest disabled"
	}
	info, err := s.o.Backups.Info(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "pgbackrest info failed: "+err.Error(),
			"the repository may be unreachable; check the pgbackrest S3 configuration and credentials")
		return http.StatusBadGateway, "info failed"
	}
	// pgBackRest's own JSON, forwarded unmodelled: its schema is the documented
	// contract, and re-shaping it here would drop fields on a pgBackRest upgrade.
	writeJSON(w, http.StatusOK, map[string]json.RawMessage{"info": info})
	return http.StatusOK, ""
}

func (s *Server) handleRestoreStatus(w http.ResponseWriter, r *http.Request) (int, string) {
	if s.o.Backups == nil {
		writeErr(w, http.StatusNotImplemented, "pgBackRest is not enabled for this release",
			"set pgbackrest.enabled=true to use the restore endpoints")
		return http.StatusNotImplemented, "pgbackrest disabled"
	}
	v, err := s.o.Backups.RestoreStatus(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error(), "")
		return http.StatusBadGateway, "restore status failed"
	}
	s.annotateRestoreView(&v)
	writeJSON(w, http.StatusOK, v)
	return http.StatusOK, ""
}

// annotateRestoreView adds the operator-facing explanation and the runbook steps the
// API deliberately does not perform.
//
// The Pending hint is derived locally rather than from Kubernetes Events: the agent
// answering this request is a running pod of the StatefulSet that holds the target
// volume, so a Job that cannot start is very likely waiting on that volume. It needs no
// extra RBAC and covers the most common failure ("forgot to scale down").
//
// It is only a HINT, and only when the Job is actually Pending, because a restore Job
// scheduled onto the SAME NODE as the volume holder starts immediately: ReadWriteOnce
// binds a volume to one node, not one pod. In that case the restore runs while the
// StatefulSet is still scaled up -- which is safe here only because this flow stopped the
// postmaster and required the cluster to be paused first. That pair, not the scale-down,
// is what keeps a restore off a live data directory.
func (s *Server) annotateRestoreView(v *RestoreView) {
	if v.Phase == "pending" && !v.ContainerStarted && unexplainedWait(v.WaitingReason) {
		if s.o.PodName == s.o.RestoreTargetPod {
			// This pod owns the target volume and is answering the request, so "that pod
			// still has it mounted" is a fact -- but whether that is why the Job has not
			// started is not, so the causation stays conditional.
			v.Hint = fmt.Sprintf(
				"the restore Job has not started yet. This request was answered by %s, so that pod still has %s mounted -- if the Job stays Pending, that is why. Scale the StatefulSet to 0 to free it.",
				s.o.RestoreTargetPod, s.dataPVC())
		} else {
			// Answered by a different member, which says nothing about the target pod.
			v.Hint = fmt.Sprintf(
				"the restore Job has not started yet. If it stays Pending, the usual cause is that %s still has %s mounted. Scale the StatefulSet to 0 to free it.",
				s.o.RestoreTargetPod, s.dataPVC())
		}
	}
	if v.Phase == "pending" || v.Phase == "none" {
		v.NextSteps = s.restoreNextSteps()
	}
	if v.Phase == "failed" {
		// A failed restore leaves PGDATA half-written and the Job is not retried
		// automatically (backoffLimit 0), so the operator's next move is to read the log
		// before doing anything else -- and the Job has to go before another attempt,
		// because Jobs are immutable.
		v.Hint = fmt.Sprintf(
			"the restore failed and PGDATA on %s may be half-written; read `kubectl logs -n %s job/%s` before retrying, then DELETE /v1/restore (or pass replace:true) to re-create it",
			s.o.RestoreTargetPod, s.o.Namespace, v.JobName)
	}
}

// unexplainedWait reports whether a not-yet-started container's waiting reason explains
// itself. "ContainerCreating" and "PodInitializing" do NOT: they are exactly what the
// kubelet reports while a pod waits for a volume it cannot attach, which is the case the
// volume hint exists for. Reasons like ImagePullBackOff or CreateContainerConfigError DO
// explain themselves and are already in the response, so the hint stays out of the way.
func unexplainedWait(reason string) bool {
	switch reason {
	case "", "ContainerCreating", "PodInitializing":
		return true
	default:
		return false
	}
}

func (s *Server) dataPVC() string {
	return fmt.Sprintf("data-%s", s.o.RestoreTargetPod)
}

// restoreNextSteps is the remainder of the runbook. The API cannot scale the StatefulSet:
// scaling to 0 deletes every agent, including the one that would report progress, so
// scale-down/up stays an operator action by design.
//
// Whether a scale-down is needed AT ALL depends on scheduling. ReadWriteOnce binds a
// volume to a NODE, not a pod, so a restore Job placed on the same node as the volume
// holder starts straight away; one placed elsewhere waits for the volume. Both cases are
// covered here, in the order an operator meets them: watch the Job first, and free the
// volume only if it actually sits Pending.
func (s *Server) restoreNextSteps() []string {
	ns := s.o.Namespace
	return []string{
		fmt.Sprintf("kubectl logs -n %s -l batch.kubernetes.io/job-name=%s -f   # the restore runs here", ns, s.o.RestoreJobName),
		fmt.Sprintf("# if the Job stays Pending it cannot attach %s -- free the volume:", s.dataPVC()),
		fmt.Sprintf("kubectl scale statefulset %s -n %s --replicas=0", s.o.ClusterName, ns),
		fmt.Sprintf("kubectl wait --for=delete pod/%s -n %s --timeout=5m", s.o.RestoreTargetPod, ns),
		fmt.Sprintf("kubectl scale statefulset %s -n %s --replicas=<original replicas>", s.o.ClusterName, ns),
		"# with standbys, scale down regardless: they must not stream from a primary being rewritten underneath them, and they re-clone onto the new timeline afterwards",
		// This step is easy to forget and the cluster stays DOWN without it: maintenance
		// mode makes the reconcile loop a no-op, so a scaled-back-up pod never starts
		// PostgreSQL and never goes Ready. It is listed as an explicit step for that
		// reason -- the API will not clear an operator's pause on its own.
		"POST /v1/resume   # REQUIRED: while paused the loop is a no-op, so the restored node will not start",
		"then GET /v1/status: recovery.replayLsn / recovery.lastReplayTime show WAL replay progress, and lastRestore carries the outcome",
	}
}

// handleRestore triggers a restore over the live data directory of THIS pod.
//
// The sequence is one flow rather than three calls, so an operator cannot get the
// order wrong: verify the cluster is paused, verify the destructive confirmations,
// stop the local postmaster, then create the Job. Pausing first is load-bearing --
// a running reconcile loop would restart the postmaster the moment it stopped, and
// pgBackRest refuses to restore over a live PGDATA.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) (int, string) {
	var req RestoreRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(),
			`body: {"node":"<pod>","confirm":"<cluster>","force":true,"targetType":"time","target":"..."}`)
		return http.StatusBadRequest, "bad body"
	}
	if status, detail, ok := s.checkNode(w, req.Node); !ok {
		return status, detail
	}
	// The rendered Job's volume is data-<cluster>-<ordinal>; only the pod that owns
	// that volume can stop the postmaster holding it, so the request must be addressed
	// there.
	if s.o.PodName != s.o.RestoreTargetPod {
		writeErr(w, http.StatusConflict,
			fmt.Sprintf("this release's restore targets %s (%s), but you are talking to %s",
				s.o.RestoreTargetPod, s.dataPVC(), s.o.PodName),
			"address the pod that owns the target volume, or change pgbackrest.restore.podOrdinal")
		return http.StatusConflict, "wrong pod for target volume"
	}
	if req.Confirm != s.o.ClusterName {
		writeErr(w, http.StatusBadRequest,
			"confirm does not match this cluster",
			fmt.Sprintf("this restore OVERWRITES %s; set confirm to %q to proceed", s.dataPVC(), s.o.ClusterName))
		return http.StatusBadRequest, "confirm mismatch"
	}
	if !req.Force {
		writeErr(w, http.StatusBadRequest, "force must be true",
			fmt.Sprintf("this restores over the live data directory on %s; there is no dry-run", s.o.PodName))
		return http.StatusBadRequest, "force not set"
	}
	// podOrdinal is confirm-only: the volume is baked into the rendered Job, and which
	// volume a restore overwrites stays a values decision, not an HTTP-body one.
	if req.PodOrdinal != nil && *req.PodOrdinal != s.o.RestorePodOrdinal {
		writeErr(w, http.StatusConflict,
			fmt.Sprintf("podOrdinal %d does not match the rendered restore target (%d)", *req.PodOrdinal, s.o.RestorePodOrdinal),
			"the target volume comes from pgbackrest.restore.podOrdinal in values, so change it there and helm upgrade")
		return http.StatusConflict, "ordinal mismatch"
	}
	if (req.Target != "" && req.TargetType == "") || (req.TargetType != "" && req.Target == "") {
		writeErr(w, http.StatusBadRequest, "targetType and target must be set together",
			`omit both to restore the latest backup and replay all archived WAL, or set e.g. {"targetType":"time","target":"2026-08-01 09:55:00+00"}`)
		return http.StatusBadRequest, "incomplete target"
	}

	m, err := s.o.Cluster.Marker(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "could not read the marker: "+err.Error(), "")
		return http.StatusBadGateway, "marker read failed"
	}
	if !m.Paused {
		writeErr(w, http.StatusConflict, "cluster is not paused",
			"POST /v1/pause first: an active reconcile loop would restart the postmaster this restore needs stopped")
		return http.StatusConflict, "not paused"
	}

	existing, err := s.o.Backups.RestoreStatus(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error(), "")
		return http.StatusBadGateway, "restore status failed"
	}
	if existing.Phase != "none" {
		if !req.Replace {
			writeErr(w, http.StatusConflict,
				fmt.Sprintf("restore Job %s already exists (phase %s)", existing.JobName, existing.Phase),
				`Jobs are immutable and this one is the record of the last restore: pass {"replace":true} to delete and re-create it, or DELETE /v1/restore`)
			return http.StatusConflict, "job exists"
		}
		if err := s.o.Backups.DeleteRestore(r.Context()); err != nil {
			writeErr(w, http.StatusBadGateway, "could not delete the previous restore Job: "+err.Error(), "")
			return http.StatusBadGateway, "delete failed"
		}
	}

	// Stop the local postmaster through the loop before creating the Job, so no
	// postmaster.pid remains for pgBackRest's interlock to trip over. The API never
	// sets the Job's FORCE flag, so that interlock stays armed: if a pid file survives
	// this stop, something else is on the volume and the restore must refuse.
	// Detached from the client for the SAME reason the Job creation below is (#298 review),
	// and it has to start HERE rather than after the stop. Submit's own contract is that "if
	// ctx expires after the intent was accepted, the loop still performs it" -- so a
	// port-forward dropping while the postmaster is shutting down cancels only the WAIT, not
	// the stop. Bound to r.Context() this handler then returned "could not stop postgres:
	// context canceled" with the hint "no restore Job was created", while the loop went on to
	// stop PostgreSQL anyway: exactly the paused-cluster-down-with-no-Job outcome the detach
	// below exists to prevent, reached one step earlier.
	ictx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.o.IntentTimeout)
	defer cancel()
	s.o.Metrics.IncControlIntent()
	if err := s.o.Node.Submit(ictx, IntentStop); err != nil {
		if ictx.Err() != nil {
			writeErr(w, http.StatusGatewayTimeout,
				fmt.Sprintf("postgres did not stop within %s", s.o.IntentTimeout),
				"no restore Job was created; check GET /v1/status and retry")
			return http.StatusGatewayTimeout, "stop timed out"
		}
		writeErr(w, http.StatusInternalServerError, "could not stop postgres: "+err.Error(),
			"no restore Job was created")
		return http.StatusInternalServerError, "stop failed"
	}

	id := identityFrom(r.Context())
	s.o.Metrics.IncControlRestoreRequest()
	// PAST THE POINT OF NO RETURN, so this half must not die with the client (#298 review).
	// The Submit above has already stopped the postmaster; creating the Job is what makes that
	// recoverable. r.Context() is cancelled on client disconnect -- a port-forward dropping
	// mid-incident is the ordinary case, and a clean stop can legitimately take tens of
	// seconds -- and the cancellation would surface as `context canceled` from
	// CloneCronJobToJob, leaving the cluster paused, PostgreSQL down and NO Job running, with
	// the operator's only signal a client-side connection error. Detach from cancellation but
	// keep a bound, so the flow either completes or reports honestly.
	jctx, jcancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.o.IntentTimeout)
	defer jcancel()
	v, err := s.o.Backups.Restore(jctx, req, id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "could not create the restore Job: "+err.Error(),
			"postgres on this pod has already been STOPPED; POST /v1/restart to bring it back, or retry the restore")
		return http.StatusBadGateway, "create failed"
	}
	s.annotateRestoreView(&v)
	writeJSON(w, http.StatusAccepted, v)
	return http.StatusAccepted, fmt.Sprintf("job=%s targetType=%s backupSet=%s", v.JobName, req.TargetType, req.BackupSet)
}

func (s *Server) handleRestoreDelete(w http.ResponseWriter, r *http.Request) (int, string) {
	if err := s.o.Backups.DeleteRestore(r.Context()); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error(), "")
		return http.StatusBadGateway, "delete failed"
	}
	writeJSON(w, http.StatusOK, actionResponse{
		Result: "restore-job-deleted", Cluster: s.o.ClusterName,
		Message: "the Job and its pods are removed; the restore outcome recorded on the data volume is untouched",
	})
	return http.StatusOK, "deleted"
}
