package control

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// handlerFunc is a route body: it writes the response and returns the status plus an
// audit detail string, so guard can audit the outcome without inspecting the body.
type handlerFunc func(http.ResponseWriter, *http.Request) (status int, detail string)

// route is one endpoint. The table is the single source of truth for the mux, for the
// 405 Allow header, and for the route list a typo gets back -- three things that
// silently drift apart when written out separately.
type route struct {
	method string
	path   string
	verb   Verb
	// featureGated routes answer "not configured" before "not authorized": the restore
	// allowlist is empty unless the feature was turned on, so without the gate every
	// disabled release would report an authorization problem instead.
	featureGated bool
	h            handlerFunc
}

func (s *Server) routeTable() []route {
	return []route{
		{http.MethodGet, "/v1/status", VerbObserve, false, s.handleStatus},
		{http.MethodGet, "/v1/cluster", VerbObserve, false, s.handleCluster},

		{http.MethodPost, "/v1/pause", VerbControl, false, s.handlePause(true)},
		{http.MethodPost, "/v1/resume", VerbControl, false, s.handlePause(false)},
		{http.MethodPost, "/v1/switchover", VerbControl, false, s.handleSwitchover},
		{http.MethodDelete, "/v1/switchover", VerbControl, false, s.handleSwitchoverCancel},
		{http.MethodPost, "/v1/restart", VerbControl, false, s.handleIntent(IntentRestart)},
		{http.MethodPost, "/v1/reload", VerbControl, false, s.handleIntent(IntentReload)},
		{http.MethodPost, "/v1/reinitialize", VerbControl, false, s.handleReinitialize},

		{http.MethodGet, "/v1/backups", VerbObserve, false, s.handleBackups},
		// Feature-gated like the mutating routes: reading the Job needs the `get jobs`
		// grant that rbac.yaml renders only under control.restore.enabled, so without the
		// gate this answers 502 Forbidden (reading as a broken Role) instead of 501.
		{http.MethodGet, "/v1/restore", VerbObserve, true, s.handleRestoreStatus},
		{http.MethodPost, "/v1/restore", VerbRestore, true, s.handleRestore},
		{http.MethodDelete, "/v1/restore", VerbRestore, true, s.handleRestoreDelete},
	}
}

// routes builds the mux from the table.
//
// The catch-all is why the table exists: registering "/" makes Go's ServeMux prefer
// it over the built-in 405 for a known path with the wrong method, so a GET on
// /v1/pause would be answered 404 by the fallback. The fallback therefore computes
// the 405 (with an Allow header) itself.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	table := s.routeTable()
	for _, rt := range table {
		h := s.guard(rt.verb, rt.h)
		if rt.featureGated {
			h = s.featureGate(h)
		}
		mux.HandleFunc(rt.method+" "+rt.path, h)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var allowed []string
		for _, rt := range table {
			if rt.path == r.URL.Path {
				allowed = append(allowed, rt.method)
			}
		}
		if len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeErr(w, http.StatusMethodNotAllowed,
				fmt.Sprintf("%s is not allowed on %s", r.Method, r.URL.Path),
				"allowed: "+strings.Join(allowed, ", "))
			return
		}
		list := make([]string, 0, len(table))
		for _, rt := range table {
			list = append(list, rt.method+" "+rt.path)
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such route", "routes": list})
	})
	return mux
}

// StatusResponse is GET /v1/status: this node, plus the cluster intents read fresh.
type StatusResponse struct {
	Node       string       `json:"node"`
	Cluster    string       `json:"cluster"`
	PGMajor    string       `json:"pgMajor"`
	HoldsLease bool         `json:"holdsLease"`
	Leader     string       `json:"leader,omitempty"`
	Local      MemberState  `json:"local"`
	Recovery   RecoveryView `json:"recovery"`
	Intents    IntentsView  `json:"intents"`
	// LastRestore is data-directory provenance: which backup set and point in time
	// this PGDATA was restored from, if it ever was.
	LastRestore *RestoreRecordView `json:"lastRestore,omitempty"`
	ObservedAt  time.Time          `json:"observedAt"`
	AgeSeconds  float64            `json:"observationAgeSeconds"`
	Warning     string             `json:"warning,omitempty"`
}

// IntentsView is the operator-set state on the marker ConfigMap: what has been asked
// for, and by whom. Read fresh on every request -- these are the fields an operator
// checks right after setting them, so serving a cached copy would be actively
// misleading.
type IntentsView struct {
	Paused           bool   `json:"paused"`
	PausedBy         string `json:"pausedBy,omitempty"`
	SwitchoverTarget string `json:"switchoverTarget,omitempty"`
	MarkerPresent    bool   `json:"markerPresent"`
	MarkerPrimary    string `json:"markerPrimary,omitempty"`
}

// RestoreRecordView mirrors pgbackrest.RestoreRecord in the API surface.
type RestoreRecordView struct {
	StartedAt    string `json:"startedAt,omitempty"`
	FinishedAt   string `json:"finishedAt,omitempty"`
	TargetType   string `json:"targetType,omitempty"`
	Target       string `json:"target,omitempty"`
	BackupSet    string `json:"backupSet,omitempty"`
	ExitCode     *int   `json:"exitCode,omitempty"`
	Succeeded    bool   `json:"succeeded"`
	ClusterState string `json:"clusterState,omitempty"`
	Checkpoint   string `json:"checkpoint,omitempty"`
	RequestedBy  string `json:"requestedBy,omitempty"`
}

// ClusterResponse is GET /v1/cluster.
type ClusterResponse struct {
	Node    string `json:"node"`
	Cluster string `json:"cluster"`
	Leader  string `json:"leader,omitempty"`
	// Members is THIS node's view: its own state from a local probe, and each peer's
	// from a cross-pod probe or, when unreachable, from gossip. Two members can
	// legitimately disagree mid-failover.
	Members []MemberState `json:"members"`
	Intents IntentsView   `json:"intents"`
	// Decision is the reconcile loop's latest decision on this node. Diagnostic
	// output, not a stable contract: action names and reasons change without notice.
	Decision   DecisionView `json:"lastDecision"`
	ObservedAt time.Time    `json:"observedAt"`
	AgeSeconds float64      `json:"observationAgeSeconds"`
	Warning    string       `json:"warning,omitempty"`
}

// intents reads the marker fresh. On failure it falls back to the snapshot's copy and
// returns a warning, so a transient apiserver blip degrades the answer instead of
// failing the request -- but never silently.
func (s *Server) intents(r *http.Request, snap Snapshot) (IntentsView, string) {
	m, err := s.o.Cluster.Marker(r.Context())
	if err != nil {
		return IntentsView{
			Paused:           snap.Paused,
			SwitchoverTarget: snap.SwitchoverTarget,
			MarkerPresent:    snap.MarkerPresent,
			MarkerPrimary:    snap.MarkerPrimary,
		}, "marker could not be read; pause/switchover shown from the last reconcile tick: " + err.Error()
	}
	return IntentsView{
		Paused:           m.Paused,
		PausedBy:         m.PausedBy,
		SwitchoverTarget: m.SwitchoverTarget,
		MarkerPresent:    m.Present,
		MarkerPrimary:    m.Primary,
	}, ""
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) (int, string) {
	snap := s.o.Node.Snapshot()
	iv, warn := s.intents(r, snap)
	resp := StatusResponse{
		Node:       snap.Node,
		Cluster:    s.o.ClusterName,
		PGMajor:    snap.PGMajor,
		HoldsLease: snap.HoldsLease,
		Leader:     snap.LeaseHolder,
		Local:      snap.Local,
		Recovery:   snap.Recovery,
		Intents:    iv,
		ObservedAt: snap.ObservedAt,
		AgeSeconds: age(snap.ObservedAt),
		Warning:    warn,
	}
	if snap.LastRestore.Present {
		lr := snap.LastRestore
		resp.LastRestore = &RestoreRecordView{
			StartedAt: lr.StartedAt, FinishedAt: lr.FinishedAt,
			TargetType: lr.TargetType, Target: lr.Target, BackupSet: lr.BackupSet,
			ExitCode: lr.ExitCode, Succeeded: lr.Succeeded(),
			ClusterState: lr.ClusterState, Checkpoint: lr.Checkpoint, RequestedBy: lr.RequestedBy,
		}
	}
	writeJSON(w, http.StatusOK, resp)
	return http.StatusOK, ""
}

func (s *Server) handleCluster(w http.ResponseWriter, r *http.Request) (int, string) {
	snap := s.o.Node.Snapshot()
	iv, warn := s.intents(r, snap)
	members := make([]MemberState, 0, len(snap.Peers)+1)
	members = append(members, snap.Local)
	members = append(members, snap.Peers...)
	writeJSON(w, http.StatusOK, ClusterResponse{
		Node:       snap.Node,
		Cluster:    s.o.ClusterName,
		Leader:     snap.LeaseHolder,
		Members:    members,
		Intents:    iv,
		Decision:   snap.Decision,
		ObservedAt: snap.ObservedAt,
		AgeSeconds: age(snap.ObservedAt),
		Warning:    warn,
	})
	return http.StatusOK, ""
}

func age(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return time.Since(t).Round(time.Millisecond).Seconds()
}

// actionResponse is the shape every mutating route returns.
type actionResponse struct {
	Result  string   `json:"result"`
	Node    string   `json:"node,omitempty"`
	Cluster string   `json:"cluster,omitempty"`
	Target  string   `json:"target,omitempty"`
	Message string   `json:"message,omitempty"`
	Warning string   `json:"warning,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

// handlePause sets or clears maintenance mode. Idempotent, and it warns about the
// thing operators most often get wrong: while paused, a REAL failure does not fail
// over either.
func (s *Server) handlePause(on bool) func(http.ResponseWriter, *http.Request) (int, string) {
	return func(w http.ResponseWriter, r *http.Request) (int, string) {
		var body struct{}
		if err := decodeBody(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error(), "pause and resume take no body")
			return http.StatusBadRequest, "bad body"
		}
		id := identityFrom(r.Context())
		m, err := s.o.Cluster.Marker(r.Context())
		if err != nil {
			writeErr(w, http.StatusBadGateway, "could not read the marker: "+err.Error(), "")
			return http.StatusBadGateway, "marker read failed"
		}
		if !m.Present {
			writeErr(w, http.StatusConflict,
				"no marker ConfigMap yet: this cluster has not recorded a primary",
				"there is nothing to pause until the cluster has elected a primary")
			return http.StatusConflict, "no marker"
		}
		if m.Paused == on {
			result := "already-paused"
			if !on {
				result = "already-running"
			}
			writeJSON(w, http.StatusOK, actionResponse{
				Result: result, Cluster: s.o.ClusterName,
				Message: fmt.Sprintf("no change; paused=%t", m.Paused),
				Warning: pauseWarning(on, s.o.Node.Snapshot()),
			})
			return http.StatusOK, "no change"
		}
		// Resuming while a restore Job is still copying is the sharpest edge in this API:
		// the loop would start PostgreSQL on a data directory pgbackrest is rewriting. The
		// 202 from POST /v1/restore hands the operator a runbook that ENDS in this call, so
		// the ordering mistake is one an attentive operator makes.
		if !on {
			if status, detail, ok := s.checkNoRestoreInFlight(w, r, "resume"); !ok {
				return status, detail
			}
		}
		if err := s.o.Cluster.SetPause(r.Context(), on, id.CN); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error(), "")
			return http.StatusBadGateway, "marker write failed"
		}
		result := "paused"
		if !on {
			result = "resumed"
		}
		writeJSON(w, http.StatusOK, actionResponse{
			Result: result, Cluster: s.o.ClusterName,
			Warning: pauseWarning(on, s.o.Node.Snapshot()),
		})
		return http.StatusOK, result
	}
}

// pauseWarning states the consequence of the pause, and escalates it when the
// cluster is ALREADY degraded -- pausing a cluster with no reachable leader suspends
// the recovery that would have fixed it.
func pauseWarning(on bool, snap Snapshot) string {
	if !on {
		return ""
	}
	base := "while paused, automatic failover is suspended: a genuine primary failure will NOT fail over until you resume"
	if snap.LeaseHolder == "" {
		return base + ". NOTE: this cluster currently has no lease holder, so pausing suspends the recovery that would elect one"
	}
	for _, p := range snap.Peers {
		if !p.Reachable {
			return base + fmt.Sprintf(". NOTE: peer %s is not reachable right now", p.Name)
		}
	}
	return base
}

type switchoverRequest struct {
	// Leader, when set, must be the current lease holder. It is an optimistic
	// concurrency check: it fails the request if leadership moved between the
	// operator's last look and this call, instead of switching away from a primary
	// they did not mean.
	Leader    string `json:"leader"`
	Candidate string `json:"candidate"`
}

// handleSwitchover records a handoff request after checking it would actually be
// acted on. The loop remains the authority for WHEN (it re-verifies the candidate is
// caught up at the moment it steps down); this refuses the requests the loop would
// silently sit on -- which is the whole reason to prefer the API over annotating.
func (s *Server) handleSwitchover(w http.ResponseWriter, r *http.Request) (int, string) {
	var req switchoverRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), `body: {"candidate":"<pod>","leader":"<pod>"}`)
		return http.StatusBadRequest, "bad body"
	}
	if req.Candidate == "" {
		writeErr(w, http.StatusBadRequest, "candidate is required", "name the pod that should become primary")
		return http.StatusBadRequest, "no candidate"
	}
	snap := s.o.Node.Snapshot()
	m, err := s.o.Cluster.Marker(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "could not read the marker: "+err.Error(), "")
		return http.StatusBadGateway, "marker read failed"
	}
	if req.Leader != "" && req.Leader != snap.LeaseHolder {
		writeErr(w, http.StatusConflict,
			fmt.Sprintf("leader mismatch: you named %q but the lease is held by %q", req.Leader, snap.LeaseHolder),
			"re-read GET /v1/status and retry if the handoff is still what you want")
		return http.StatusConflict, "leader mismatch"
	}
	if req.Candidate == snap.LeaseHolder {
		writeErr(w, http.StatusConflict,
			fmt.Sprintf("%s already holds the lease", req.Candidate), "nothing to switch over")
		return http.StatusConflict, "candidate is leader"
	}
	// A paused loop takes no action at all, so a request recorded now would sit
	// unnoticed until someone resumed. Refuse instead of accepting a no-op.
	if m.Paused {
		writeErr(w, http.StatusConflict, "cluster is paused: a switchover would not be acted on",
			"POST /v1/resume first (maintenance mode suspends every automatic action)")
		return http.StatusConflict, "paused"
	}
	cand, found := findMember(snap, req.Candidate)
	if !found {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("%s is not a member of this cluster", req.Candidate),
			"members are listed by GET /v1/cluster")
		return http.StatusBadRequest, "unknown candidate"
	}
	if snap.LeaseHolder == "" {
		writeErr(w, http.StatusConflict,
			"this cluster currently has no lease holder, so there is no primary to hand the role over from",
			"wait for a leader (GET /v1/cluster), or check whether the cluster is paused")
		return http.StatusConflict, "no leader"
	}
	ref, refOK := switchoverReference(snap)
	if !refOK {
		writeErr(w, http.StatusConflict,
			fmt.Sprintf("the lease holder %q is not in this pod's view of the cluster, so the candidate cannot be checked against the primary's timeline", snap.LeaseHolder),
			"re-read GET /v1/cluster, or address this call to a pod that can see the primary")
		return http.StatusConflict, "leader not visible"
	}
	if reason, ok := candidateUnready(ref, cand); !ok {
		writeErr(w, http.StatusConflict,
			fmt.Sprintf("%s cannot take over: %s", req.Candidate, reason),
			"the primary only steps down for a reachable, same-timeline standby that is caught up")
		return http.StatusConflict, "candidate not ready"
	}
	id := identityFrom(r.Context())
	if err := s.o.Cluster.SetSwitchoverTarget(r.Context(), req.Candidate, id.CN); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error(), "")
		return http.StatusBadGateway, "marker write failed"
	}
	writeJSON(w, http.StatusAccepted, actionResponse{
		Result: "switchover-requested", Cluster: s.o.ClusterName, Target: req.Candidate,
		Message: "the serving primary steps down once the candidate is verified caught up; the request is one-shot",
		Notes: []string{
			"watch GET /v1/cluster for the leader to change",
			"DELETE /v1/switchover cancels a request that has not been acted on",
		},
	})
	return http.StatusAccepted, "target=" + req.Candidate
}

func findMember(snap Snapshot, name string) (MemberState, bool) {
	if snap.Local.Name == name {
		return snap.Local, true
	}
	for _, p := range snap.Peers {
		if p.Name == name {
			return p, true
		}
	}
	return MemberState{}, false
}

// switchoverReference returns the member a candidate must be measured against: the
// current lease holder, i.e. the node that will actually step down.
//
// It is deliberately NOT this pod. The loop's own switchover gate compares against the
// primary because there the local node IS the lease holder, but this API can be
// addressed to any member -- and a standby still on the previous timeline would both
// refuse valid candidates ("it is on timeline N but this node is on N-1") and admit
// divergent ones, which is exactly the silent no-op the preflight exists to prevent.
func switchoverReference(snap Snapshot) (MemberState, bool) {
	if snap.LeaseHolder == "" {
		return MemberState{}, false
	}
	return findMember(snap, snap.LeaseHolder)
}

// candidateUnready mirrors the loop's switchover gate: a reachable standby on the
// primary's timeline. ref is the current lease holder (see switchoverReference); it
// reports the FIRST failing condition in the operator's terms.
//
// It deliberately checks position but not an exact LSN equality: replication is
// moving, so a byte-exact comparison here would be stale by the time the primary
// acts. The loop re-checks at the moment of handoff; this catches the cases that
// cannot resolve on their own (wrong role, unreachable, divergent timeline).
func candidateUnready(ref, cand MemberState) (string, bool) {
	switch {
	case cand.Gossip || !cand.Reachable:
		return "it is not reachable from this pod", false
	case cand.Role != "standby":
		return fmt.Sprintf("its role is %q, not standby", cand.Role), false
	case !cand.TimelineOK:
		return "its timeline could not be read", false
	case !ref.TimelineOK:
		// Refuse rather than skip the comparison: without the primary's timeline a
		// divergent candidate cannot be ruled out, and accepting one yields a 202 the
		// loop then silently sits on.
		return fmt.Sprintf("the timeline of the primary %s could not be read, so a divergent candidate cannot be ruled out", ref.Name), false
	case cand.Timeline != ref.Timeline:
		return fmt.Sprintf("it is on timeline %d but the primary %s is on %d", cand.Timeline, ref.Name, ref.Timeline), false
	}
	return "", true
}

func (s *Server) handleSwitchoverCancel(w http.ResponseWriter, r *http.Request) (int, string) {
	if err := s.o.Cluster.ClearSwitchoverTarget(r.Context()); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error(), "")
		return http.StatusBadGateway, "marker write failed"
	}
	writeJSON(w, http.StatusOK, actionResponse{
		Result: "switchover-cleared", Cluster: s.o.ClusterName,
		Message: "any pending handoff request is removed; a switchover already in progress is not rolled back",
	})
	return http.StatusOK, "cleared"
}

type nodeRequest struct {
	// Node must name the pod being addressed. Node-local verbs act on whichever pod
	// answered the connection, so requiring the caller to say which pod they think
	// that is turns a misrouted request into a 409 instead of an action on the wrong
	// database.
	Node  string `json:"node"`
	Force bool   `json:"force"`
}

// handleIntent serves the node-local verbs by handing the operation to the reconcile
// loop, which owns the postmaster. Blocking (rather than 202) is deliberate: a
// restart or reload finishes in seconds, and a synchronous answer is the point of
// having an API.
func (s *Server) handleIntent(kind IntentKind) func(http.ResponseWriter, *http.Request) (int, string) {
	return func(w http.ResponseWriter, r *http.Request) (int, string) {
		var req nodeRequest
		if err := decodeBody(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error(), `body: {"node":"<this pod>"}`)
			return http.StatusBadRequest, "bad body"
		}
		if status, detail, ok := s.checkNode(w, req.Node); !ok {
			return status, detail
		}
		snap := s.o.Node.Snapshot()
		// Restarting the serving primary is a write outage. Require an explicit force
		// so it cannot be the result of a fat-fingered pod name plus a default.
		// The lease is read LIVE, and while it is held the gate FAILS CLOSED: force is
		// required unless the snapshot POSITIVELY shows a running standby ("standby" is
		// the one role localRole only reports for a running non-primary). Keying the
		// gate on Running && Role == "primary" instead let a single failed local probe
		// on the serving primary -- a probe timeout under load, max_connections
		// exhausted; postgres still accepting writes -- publish Running=false /
		// Role="unknown" and wave an unforced restart through (#298 review). The
		// pre-first-tick all-zero snapshot (ObservedAt zero, Role "") fails the same
		// test, which is exactly right: ambiguity is not a positive showing.
		if kind == IntentRestart && !req.Force && s.o.Node.HoldsLeaseNow() && snap.Local.Role != "standby" {
			hint := `pass {"force":true} to proceed, and consider POST /v1/pause first so the restart is not read as a failure`
			var msg string
			switch {
			case snap.ObservedAt.IsZero():
				msg = "this pod holds the leader lease and has not completed its first reconcile tick, so it may be the serving primary"
			case snap.Local.Running && snap.Local.Role == "primary":
				msg = "this pod is the serving primary: restarting it interrupts writes"
			default:
				msg = "this pod holds the leader lease and its last local probe failed, so it may still be the serving primary"
			}
			writeErr(w, http.StatusConflict, msg, hint)
			return http.StatusConflict, "primary without force"
		}
		ctx, cancel := contextWithTimeout(r, s.o.IntentTimeout)
		defer cancel()
		s.o.Metrics.IncControlIntent()
		if err := s.o.Node.Submit(ctx, kind); err != nil {
			if ctx.Err() != nil {
				writeErr(w, http.StatusGatewayTimeout,
					fmt.Sprintf("%s did not complete within %s", kind, s.o.IntentTimeout),
					"the operation may still be in progress; check GET /v1/status")
				return http.StatusGatewayTimeout, kind.String() + " timed out"
			}
			writeErr(w, http.StatusInternalServerError, fmt.Sprintf("%s failed: %v", kind, err), "")
			return http.StatusInternalServerError, kind.String() + " failed"
		}
		writeJSON(w, http.StatusOK, actionResponse{
			Result: kind.String() + "ed", Node: s.o.PodName,
			Message: "state in GET /v1/status refreshes on the next reconcile tick",
		})
		return http.StatusOK, kind.String()
	}
}

// checkNoRestoreInFlight refuses an operation that would write to, or start PostgreSQL
// on, a data directory a restore Job is currently rewriting. A Job that is pending or
// running is in flight; succeeded/failed/none are not.
//
// It fails CLOSED on a read error: not being able to tell whether a restore is running is
// not evidence that none is.
func (s *Server) checkNoRestoreInFlight(w http.ResponseWriter, r *http.Request, what string) (int, string, bool) {
	// Only meaningful when restore triggering is ON: the Job this looks for is the one the
	// API creates, and the `get jobs` grant that reads it is rendered only under
	// control.restore.enabled. Calling it otherwise would fail closed on an RBAC error and
	// break resume outright for every release that has pgbackrest enabled but restore off.
	if s.o.Backups == nil || !s.o.Backups.RestoreEnabled() {
		return 0, "", true
	}
	v, err := s.o.Backups.RestoreStatus(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway,
			fmt.Sprintf("could not determine whether a restore is in flight, so %s was refused: %v", what, err),
			"retry, or check the restore Job directly")
		return http.StatusBadGateway, "restore status unreadable", false
	}
	if v.Phase == "pending" || v.Phase == "running" {
		writeErr(w, http.StatusConflict,
			fmt.Sprintf("restore Job %s is %s: %s would write to the data directory it is rewriting", v.JobName, v.Phase, what),
			"wait for GET /v1/restore to report succeeded or failed, or abandon the restore with DELETE /v1/restore first")
		return http.StatusConflict, "restore in flight", false
	}
	return 0, "", true
}

// checkNode enforces the addressed-pod interlock.
func (s *Server) checkNode(w http.ResponseWriter, node string) (int, string, bool) {
	if node == "" {
		writeErr(w, http.StatusBadRequest, "node is required",
			fmt.Sprintf("this endpoint acts on the pod that answers it; pass {\"node\":%q}", s.o.PodName))
		return http.StatusBadRequest, "no node", false
	}
	if node != s.o.PodName {
		writeErr(w, http.StatusConflict,
			fmt.Sprintf("you addressed %q but this is %q", node, s.o.PodName),
			"node-local operations are per-pod: connect to the pod you mean (there is deliberately no load-balanced control Service)")
		return http.StatusConflict, "node mismatch", false
	}
	return 0, "", true
}

// handleReinitialize discards this replica's data directory so the reconcile loop rebuilds
// it from the lease holder. It is the recovery path for a standby that cannot rejoin on its
// own -- a diverged timeline, a corrupt local copy -- and it replaces the manual "delete the
// PVC and the pod" runbook.
//
// Three things make it safe to expose:
//
// REPLICA ONLY. It refuses on the lease holder. Wiping the primary's data would discard the
// cluster's only copy of committed writes; that is what POST /v1/restore is for, and it has
// its own confirmations and its own authz verb. This node's own view is not trusted for the
// check -- the marker/lease are read fresh -- because a stale snapshot claiming "I am a
// standby" is exactly the case that would be catastrophic.
//
// NOT WHILE PAUSED. Maintenance mode makes the loop a no-op, so a wipe would leave the
// replica empty and stopped with nothing to rebuild it, until someone resumed. Refusing is
// better than a cluster that quietly lost a replica.
//
// FORCE REQUIRED, and the pod must be named -- it destroys data, even if it is data that
// can be rebuilt from the primary.
//
// It returns 202: the wipe is immediate but the clone that follows takes as long as the
// database is big, and it is the loop that performs it. Progress is GET /v1/status
// (local.hasData, then local.role) and GET /v1/cluster.
func (s *Server) handleReinitialize(w http.ResponseWriter, r *http.Request) (int, string) {
	var req nodeRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), `body: {"node":"<this pod>","force":true}`)
		return http.StatusBadRequest, "bad body"
	}
	if status, detail, ok := s.checkNode(w, req.Node); !ok {
		return status, detail
	}
	if !req.Force {
		writeErr(w, http.StatusBadRequest, "force must be true",
			fmt.Sprintf("this DISCARDS the data directory on %s and re-clones it from the primary; there is no dry-run", s.o.PodName))
		return http.StatusBadRequest, "force not set"
	}

	// Fresh reads for both gates: a cached "I am a standby" is the one error that would
	// make this destroy the cluster's only copy of its data.
	m, err := s.o.Cluster.Marker(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "could not read the marker: "+err.Error(),
			"the replica-only and pause checks cannot be verified, so nothing was changed")
		return http.StatusBadGateway, "marker read failed"
	}
	if m.Paused {
		writeErr(w, http.StatusConflict, "cluster is paused: the re-clone would not start",
			"while paused the reconcile loop takes no action, so this would leave the replica empty and stopped. POST /v1/resume first")
		return http.StatusConflict, "paused"
	}
	// The replica-only gate is read LIVE from the DCS, never from the snapshot. The
	// snapshot is all-zero until the first tick publishes, and the control listener starts
	// before boot() and before that first tick -- so a cached HoldsLease=false would admit
	// this wipe on the actual primary during startup (unrecoverable on a single-replica
	// release, since the marker then names an empty node and the loop waits forever).
	if s.o.Node.HoldsLeaseNow() {
		writeErr(w, http.StatusConflict,
			fmt.Sprintf("%s holds the leader lease: reinitialize is replica-only", s.o.PodName),
			"wiping the primary would discard the cluster's only copy of committed data. Hand the primary role to another pod with POST /v1/switchover first, or use POST /v1/restore to recover the primary from a backup")
		return http.StatusConflict, "holds lease"
	}
	// Second, independent check against the durable marker: if it records THIS pod as the
	// primary, refuse even if the lease read said otherwise (a lease we have just lost, or
	// a DCS blip). Two disagreeing sources both have to clear it.
	if m.Primary == s.o.PodName {
		writeErr(w, http.StatusConflict,
			fmt.Sprintf("the primary marker records %s as the primary: reinitialize is replica-only", s.o.PodName),
			"if this node really is a stale ex-primary, let the reconcile loop resolve it first (GET /v1/cluster shows the current holder)")
		return http.StatusConflict, "marker names this pod primary"
	}
	snap := s.o.Node.Snapshot()
	// Before the first reconcile tick nothing below is meaningful, and the role field is
	// "" rather than a role. Refuse rather than infer safety from unpopulated state.
	if snap.ObservedAt.IsZero() {
		writeErr(w, http.StatusConflict,
			"the agent has not completed its first reconcile tick yet",
			"this node's role is not established, so a destructive rebuild cannot be authorised; retry once GET /v1/status reports a role")
		return http.StatusConflict, "no observation yet"
	}
	if snap.Local.Role == "primary" {
		// Running read-write without the lease is a fenced/split-brain shape the loop is
		// about to resolve; do not race it with a destructive local action.
		writeErr(w, http.StatusConflict,
			"this node is running read-write without holding the lease",
			"the reconcile loop is about to demote or fence it; retry once GET /v1/status reports a standby role")
		return http.StatusConflict, "read-write without lease"
	}
	// A restore Job writing to this same volume must not be raced by a wipe.
	if status, detail, ok := s.checkNoRestoreInFlight(w, r, "reinitialize"); !ok {
		return status, detail
	}

	id := identityFrom(r.Context())
	ictx, cancel := contextWithTimeout(r, s.o.IntentTimeout)
	defer cancel()
	s.o.Metrics.IncControlIntent()
	if err := s.o.Node.Submit(ictx, IntentReinitialize); err != nil {
		if ictx.Err() != nil {
			writeErr(w, http.StatusGatewayTimeout,
				fmt.Sprintf("reinitialize did not complete within %s", s.o.IntentTimeout),
				"the data directory may already have been discarded; check GET /v1/status")
			return http.StatusGatewayTimeout, "reinitialize timed out"
		}
		writeErr(w, http.StatusInternalServerError, "reinitialize failed: "+err.Error(),
			"the data directory was left as it was unless the error says otherwise")
		return http.StatusInternalServerError, "reinitialize failed"
	}
	s.o.Log.Warn("control: replica data directory discarded for a re-clone",
		"node", s.o.PodName, "client_cn", id.CN, "client_fingerprint", id.Fingerprint)
	writeJSON(w, http.StatusAccepted, actionResponse{
		Result: "reinitializing", Node: s.o.PodName,
		Message: "the data directory is discarded; the reconcile loop re-clones from the lease holder on its next tick",
		Notes: []string{
			"watch GET /v1/status: local.hasData goes true when the clone lands, then local.role becomes standby",
			"the clone takes as long as the database is large; nothing further is needed from you",
		},
	})
	return http.StatusAccepted, "reinitialize"
}
