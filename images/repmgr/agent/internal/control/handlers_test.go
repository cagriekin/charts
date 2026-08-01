package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cagriekin/pg-ha-agent/internal/pgbackrest"
)

// --- fakes ---

type pauseCall struct {
	on bool
	by string
}

type fakeCluster struct {
	marker    MarkerView
	markerErr error

	pauses       []pauseCall
	switchTarget string
	switchBy     string
	cleared      int

	setPauseErr  error
	setSwitchErr error
	clearErr     error

	order *[]string
}

func (f *fakeCluster) Marker(context.Context) (MarkerView, error) {
	return f.marker, f.markerErr
}

func (f *fakeCluster) SetPause(_ context.Context, on bool, by string) error {
	if f.setPauseErr != nil {
		return f.setPauseErr
	}
	f.pauses = append(f.pauses, pauseCall{on: on, by: by})
	f.marker.Paused = on
	return nil
}

func (f *fakeCluster) SetSwitchoverTarget(_ context.Context, target, by string) error {
	if f.setSwitchErr != nil {
		return f.setSwitchErr
	}
	f.switchTarget, f.switchBy = target, by
	return nil
}

func (f *fakeCluster) ClearSwitchoverTarget(context.Context) error {
	if f.clearErr != nil {
		return f.clearErr
	}
	f.cleared++
	return nil
}

type fakeNode struct {
	snap      Snapshot
	submitted []IntentKind
	submitErr error
	// hang makes Submit block until the caller's context expires, standing in for a
	// reconcile loop that is busy with a long action.
	hang  bool
	order *[]string
}

func (f *fakeNode) Snapshot() Snapshot { return f.snap }

func (f *fakeNode) Submit(ctx context.Context, kind IntentKind) error {
	if f.hang {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.submitErr != nil {
		return f.submitErr
	}
	f.submitted = append(f.submitted, kind)
	if f.order != nil {
		*f.order = append(*f.order, "intent:"+kind.String())
	}
	return nil
}

type fakeBackups struct {
	info      json.RawMessage
	infoErr   error
	status    RestoreView
	statusErr error
	created   *RestoreRequest
	createErr error
	deletes   int
	enabled   bool
	order     *[]string
}

func (f *fakeBackups) Info(context.Context) (json.RawMessage, error) { return f.info, f.infoErr }

func (f *fakeBackups) RestoreStatus(context.Context) (RestoreView, error) {
	return f.status, f.statusErr
}

func (f *fakeBackups) Restore(_ context.Context, req RestoreRequest, id Identity) (RestoreView, error) {
	if f.createErr != nil {
		return RestoreView{}, f.createErr
	}
	f.created = &req
	if f.order != nil {
		*f.order = append(*f.order, "createJob")
	}
	return RestoreView{Phase: "pending", JobName: "pg-pgbackrest-restore-api", RequestedBy: id.CN}, nil
}

func (f *fakeBackups) DeleteRestore(context.Context) error {
	f.deletes++
	if f.order != nil {
		*f.order = append(*f.order, "deleteJob")
	}
	return nil
}

func (f *fakeBackups) RestoreEnabled() bool { return f.enabled }

// --- harness ---

func healthySnapshot() Snapshot {
	return Snapshot{
		Node:        "pg-0",
		PGMajor:     "18",
		HoldsLease:  true,
		LeaseHolder: "pg-0",
		Local: MemberState{
			Name: "pg-0", Self: true, Role: "primary", Reachable: true, Running: true,
			HasData: true, Timeline: 7, TimelineOK: true, LSN: "16/B3000000",
		},
		Peers: []MemberState{{
			Name: "pg-1", Role: "standby", Reachable: true, Running: true,
			HasData: true, Timeline: 7, TimelineOK: true, LSN: "16/B2FF0000",
		}},
		Decision:   DecisionView{Action: "StayPrimary", Reason: "holds lease and is a current primary", At: time.Now()},
		Recovery:   RecoveryView{InRecovery: false},
		ObservedAt: time.Now(),
	}
}

type harness struct {
	srv *Server
	cl  *fakeCluster
	nd  *fakeNode
	bk  *fakeBackups
	// order records the sequence of side effects across fakes, for the restore
	// ordering assertions.
	order []string
}

func newHarness(t *testing.T, tweak func(*Options, *harness)) *harness {
	t.Helper()
	h := &harness{}
	h.cl = &fakeCluster{marker: MarkerView{Present: true, Primary: "pg-0"}, order: &h.order}
	h.nd = &fakeNode{snap: healthySnapshot(), order: &h.order}
	h.bk = &fakeBackups{info: json.RawMessage(`[{"name":"db"}]`), status: RestoreView{Phase: "none"}, enabled: true, order: &h.order}
	o := Options{
		Addr:              ":9201",
		ClusterName:       "pg",
		PodName:           "pg-0",
		Namespace:         "db",
		RestoreTargetPod:  "pg-0",
		RestorePodOrdinal: 0,
		RestoreJobName:    "pg-pgbackrest-restore-api",
		RestoreAllowedCNs: []string{"dba"},
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		Cluster:           h.cl,
		Node:              h.nd,
		Backups:           h.bk,
		Metrics:           Nop{},
		RequestTimeout:    5 * time.Second,
		IntentTimeout:     200 * time.Millisecond,
	}
	if tweak != nil {
		tweak(&o, h)
	}
	// Bypass New(): it loads TLS material from disk, which the handler-logic tests do
	// not exercise (the mTLS path has its own tests against a real TLS listener).
	h.srv = &Server{o: o, sem: make(chan struct{}, maxConcurrent)}
	return h
}

// do issues a request through the full chain except authn, injecting an already
// authenticated identity the way the TLS middleware would.
func (h *harness) do(method, path, body, cn string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req = req.WithContext(withIdentity(req.Context(), Identity{CN: cn, Fingerprint: "ab12", Serial: "1"}))
	rec := httptest.NewRecorder()
	h.srv.recoverMW(h.srv.limitMW(h.srv.routes())).ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("response is not JSON (%d): %s", rec.Code, rec.Body.String())
	}
	return v
}

func wantCode(t *testing.T, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, code, rec.Body.String())
	}
}

// --- status / cluster ---

func TestStatusReportsFreshIntents(t *testing.T) {
	h := newHarness(t, nil)
	h.cl.marker = MarkerView{Present: true, Paused: true, PausedBy: "ops-admin", SwitchoverTarget: "pg-1", Primary: "pg-0"}
	rec := h.do("GET", "/v1/status", "", "ops-admin")
	wantCode(t, rec, 200)
	resp := decode[StatusResponse](t, rec)
	if resp.Node != "pg-0" || resp.Cluster != "pg" || resp.PGMajor != "18" {
		t.Errorf("bad identity fields: %+v", resp)
	}
	// The intents must come from the FRESH marker read, not the (unpaused) snapshot --
	// an operator checks these right after setting them.
	if !resp.Intents.Paused || resp.Intents.PausedBy != "ops-admin" || resp.Intents.SwitchoverTarget != "pg-1" {
		t.Errorf("intents should come from the marker: %+v", resp.Intents)
	}
	if !resp.HoldsLease || resp.Local.Role != "primary" {
		t.Errorf("local state missing: %+v", resp)
	}
	if resp.Warning != "" {
		t.Errorf("unexpected warning: %q", resp.Warning)
	}
}

// A marker read failure must degrade to the cached values WITH a warning, never fail
// the whole status call and never silently present stale data as fresh.
func TestStatusDegradesOnMarkerError(t *testing.T) {
	h := newHarness(t, nil)
	h.cl.markerErr = errors.New("apiserver unreachable")
	h.nd.snap.Paused = true
	rec := h.do("GET", "/v1/status", "", "ops")
	wantCode(t, rec, 200)
	resp := decode[StatusResponse](t, rec)
	if !resp.Intents.Paused {
		t.Error("should fall back to the snapshot's pause state")
	}
	if !strings.Contains(resp.Warning, "last reconcile tick") {
		t.Errorf("the fallback must be disclosed: %q", resp.Warning)
	}
}

func TestStatusIncludesRestoreProvenance(t *testing.T) {
	h := newHarness(t, nil)
	zero := 0
	h.nd.snap.LastRestore = pgbackrest.RestoreRecord{
		Present: true, TargetType: "time", Target: "2026-08-01 09:55:00+00",
		BackupSet: "20260801-090002F", ExitCode: &zero, RequestedBy: "dba",
	}
	resp := decode[StatusResponse](t, h.do("GET", "/v1/status", "", "ops"))
	if resp.LastRestore == nil {
		t.Fatal("lastRestore should be reported: it is the only record of where this PGDATA came from")
	}
	if !resp.LastRestore.Succeeded || resp.LastRestore.BackupSet != "20260801-090002F" {
		t.Errorf("bad record: %+v", resp.LastRestore)
	}
}

func TestClusterListsMembersAndDecision(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.do("GET", "/v1/cluster", "", "ops")
	wantCode(t, rec, 200)
	resp := decode[ClusterResponse](t, rec)
	if len(resp.Members) != 2 {
		t.Fatalf("want self + 1 peer, got %d: %+v", len(resp.Members), resp.Members)
	}
	if !resp.Members[0].Self || resp.Members[0].Name != "pg-0" {
		t.Errorf("self should be first and flagged: %+v", resp.Members[0])
	}
	// The reconcile decision is the whole point of this endpoint: it answers "why is
	// nothing happening" without reading logs.
	if resp.Decision.Action != "StayPrimary" || resp.Decision.Reason == "" {
		t.Errorf("decision missing: %+v", resp.Decision)
	}
}

// --- pause ---

func TestPauseWritesMarkerWithRequesterAndWarns(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.do("POST", "/v1/pause", "", "ops-admin")
	wantCode(t, rec, 200)
	resp := decode[actionResponse](t, rec)
	if resp.Result != "paused" {
		t.Errorf("result = %q", resp.Result)
	}
	if len(h.cl.pauses) != 1 || !h.cl.pauses[0].on || h.cl.pauses[0].by != "ops-admin" {
		t.Errorf("pause calls = %+v; the client identity must be recorded", h.cl.pauses)
	}
	// The consequence has to be stated: this is the step that suspends failover.
	if !strings.Contains(resp.Warning, "will NOT fail over") {
		t.Errorf("warning should state the consequence: %q", resp.Warning)
	}
}

func TestPauseIsIdempotent(t *testing.T) {
	h := newHarness(t, nil)
	h.cl.marker.Paused = true
	rec := h.do("POST", "/v1/pause", "", "ops")
	wantCode(t, rec, 200)
	if got := decode[actionResponse](t, rec).Result; got != "already-paused" {
		t.Errorf("result = %q, want already-paused", got)
	}
	if len(h.cl.pauses) != 0 {
		t.Error("no write should be issued when the state already matches")
	}
}

func TestResumeClearsPause(t *testing.T) {
	h := newHarness(t, nil)
	h.cl.marker.Paused = true
	rec := h.do("POST", "/v1/resume", "", "ops")
	wantCode(t, rec, 200)
	if got := decode[actionResponse](t, rec).Result; got != "resumed" {
		t.Errorf("result = %q", got)
	}
	if len(h.cl.pauses) != 1 || h.cl.pauses[0].on {
		t.Errorf("pause calls = %+v, want one clear", h.cl.pauses)
	}
	// A resume carries no scare warning.
	if w := decode[actionResponse](t, rec).Warning; w != "" {
		t.Errorf("resume should not warn: %q", w)
	}
}

// Pausing a cluster that is already leaderless suspends the recovery that would fix
// it -- the warning must escalate rather than read as routine.
func TestPauseEscalatesWarningWhenLeaderless(t *testing.T) {
	h := newHarness(t, nil)
	h.nd.snap.LeaseHolder = ""
	resp := decode[actionResponse](t, h.do("POST", "/v1/pause", "", "ops"))
	if !strings.Contains(resp.Warning, "no lease holder") {
		t.Errorf("warning should escalate: %q", resp.Warning)
	}
}

func TestPauseWithoutMarkerIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	h.cl.marker = MarkerView{Present: false}
	rec := h.do("POST", "/v1/pause", "", "ops")
	wantCode(t, rec, 409)
	if len(h.cl.pauses) != 0 {
		t.Error("nothing should be written without a marker")
	}
}

func TestPauseRejectsBody(t *testing.T) {
	// DisallowUnknownFields means a hopeful `{"reason":"..."}` fails loudly instead of
	// being silently dropped.
	rec := newHarness(t, nil).do("POST", "/v1/pause", `{"reason":"maintenance"}`, "ops")
	wantCode(t, rec, 400)
}

// --- switchover ---

func TestSwitchoverHappyPath(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.do("POST", "/v1/switchover", `{"leader":"pg-0","candidate":"pg-1"}`, "ops-admin")
	wantCode(t, rec, 202)
	if h.cl.switchTarget != "pg-1" || h.cl.switchBy != "ops-admin" {
		t.Errorf("target=%q by=%q", h.cl.switchTarget, h.cl.switchBy)
	}
	resp := decode[actionResponse](t, rec)
	if resp.Result != "switchover-requested" || resp.Target != "pg-1" {
		t.Errorf("bad response: %+v", resp)
	}
}

func TestSwitchoverRequiresCandidate(t *testing.T) {
	h := newHarness(t, nil)
	wantCode(t, h.do("POST", "/v1/switchover", `{}`, "ops"), 400)
	if h.cl.switchTarget != "" {
		t.Error("nothing should be written")
	}
}

// The leader field is an optimistic concurrency check: if leadership moved since the
// operator looked, the request must fail rather than switch away from a primary they
// did not mean to touch.
func TestSwitchoverLeaderMismatch(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.do("POST", "/v1/switchover", `{"leader":"pg-1","candidate":"pg-0"}`, "ops")
	wantCode(t, rec, 409)
	if !strings.Contains(rec.Body.String(), "leader mismatch") {
		t.Errorf("body should name the problem: %s", rec.Body.String())
	}
	if h.cl.switchTarget != "" {
		t.Error("nothing should be written")
	}
}

func TestSwitchoverToCurrentLeaderIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	wantCode(t, h.do("POST", "/v1/switchover", `{"candidate":"pg-0"}`, "ops"), 409)
}

func TestSwitchoverUnknownCandidate(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.do("POST", "/v1/switchover", `{"candidate":"pg-9"}`, "ops")
	wantCode(t, rec, 400)
	if !strings.Contains(rec.Body.String(), "not a member") {
		t.Errorf("body: %s", rec.Body.String())
	}
}

// A paused loop takes no action, so recording a request would leave it silently
// pending -- exactly the failure mode the API exists to prevent.
func TestSwitchoverRefusedWhilePaused(t *testing.T) {
	h := newHarness(t, nil)
	h.cl.marker.Paused = true
	rec := h.do("POST", "/v1/switchover", `{"candidate":"pg-1"}`, "ops")
	wantCode(t, rec, 409)
	if !strings.Contains(rec.Body.String(), "paused") {
		t.Errorf("body: %s", rec.Body.String())
	}
	if h.cl.switchTarget != "" {
		t.Error("nothing should be written")
	}
}

// These are the cases plain `kubectl annotate` accepts and the loop then sits on.
func TestSwitchoverRejectsUnreadyCandidates(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Snapshot)
		want   string
	}{
		{"unreachable", func(s *Snapshot) { s.Peers[0].Reachable = false }, "not reachable"},
		{"gossip only", func(s *Snapshot) { s.Peers[0].Gossip = true }, "not reachable"},
		{"not a standby", func(s *Snapshot) { s.Peers[0].Role = "primary" }, "not standby"},
		{"timeline unknown", func(s *Snapshot) { s.Peers[0].TimelineOK = false }, "timeline could not be read"},
		{"divergent timeline", func(s *Snapshot) { s.Peers[0].Timeline = 6 }, "timeline 6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			tc.mutate(&h.nd.snap)
			rec := h.do("POST", "/v1/switchover", `{"candidate":"pg-1"}`, "ops")
			wantCode(t, rec, 409)
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("body should explain %q: %s", tc.want, rec.Body.String())
			}
			if h.cl.switchTarget != "" {
				t.Error("nothing should be written for an unready candidate")
			}
		})
	}
}

func TestSwitchoverCancel(t *testing.T) {
	h := newHarness(t, nil)
	wantCode(t, h.do("DELETE", "/v1/switchover", "", "ops"), 200)
	if h.cl.cleared != 1 {
		t.Errorf("cleared = %d, want 1", h.cl.cleared)
	}
	// Idempotent: clearing twice is not an error.
	wantCode(t, h.do("DELETE", "/v1/switchover", "", "ops"), 200)
}

// --- node-local intents ---

func TestRestartRequiresAddressedNode(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.do("POST", "/v1/restart", `{}`, "ops")
	wantCode(t, rec, 400)
	if len(h.nd.submitted) != 0 {
		t.Error("no intent should be submitted")
	}
}

// The interlock that makes a misrouted request harmless.
func TestRestartRejectsWrongNode(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.do("POST", "/v1/restart", `{"node":"pg-1"}`, "ops")
	wantCode(t, rec, 409)
	if !strings.Contains(rec.Body.String(), "pg-0") {
		t.Errorf("the error should name the pod actually serving: %s", rec.Body.String())
	}
	if len(h.nd.submitted) != 0 {
		t.Error("no intent should be submitted for the wrong node")
	}
}

func TestRestartOfServingPrimaryNeedsForce(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.do("POST", "/v1/restart", `{"node":"pg-0"}`, "ops")
	wantCode(t, rec, 409)
	if !strings.Contains(rec.Body.String(), "force") {
		t.Errorf("the error should name the escape hatch: %s", rec.Body.String())
	}
	if len(h.nd.submitted) != 0 {
		t.Error("no restart without force on the serving primary")
	}
	// With force it proceeds.
	wantCode(t, h.do("POST", "/v1/restart", `{"node":"pg-0","force":true}`, "ops"), 200)
	if len(h.nd.submitted) != 1 || h.nd.submitted[0] != IntentRestart {
		t.Errorf("submitted = %v", h.nd.submitted)
	}
}

// A standby needs no force: restarting it costs no writes.
func TestRestartStandbyNeedsNoForce(t *testing.T) {
	h := newHarness(t, nil)
	h.nd.snap.HoldsLease = false
	h.nd.snap.Local.Role = "standby"
	wantCode(t, h.do("POST", "/v1/restart", `{"node":"pg-0"}`, "ops"), 200)
	if len(h.nd.submitted) != 1 {
		t.Errorf("submitted = %v", h.nd.submitted)
	}
}

func TestReloadSubmitsIntent(t *testing.T) {
	h := newHarness(t, nil)
	wantCode(t, h.do("POST", "/v1/reload", `{"node":"pg-0"}`, "ops"), 200)
	if len(h.nd.submitted) != 1 || h.nd.submitted[0] != IntentReload {
		t.Errorf("submitted = %v", h.nd.submitted)
	}
}

// A busy reconcile loop must produce a timeout that says the state is still
// observable -- never a 200 for something that may not have happened.
func TestIntentTimeoutIs504(t *testing.T) {
	h := newHarness(t, nil)
	h.nd.hang = true
	rec := h.do("POST", "/v1/reload", `{"node":"pg-0"}`, "ops")
	wantCode(t, rec, 504)
	if !strings.Contains(rec.Body.String(), "GET /v1/status") {
		t.Errorf("the timeout should point at how to check: %s", rec.Body.String())
	}
}

func TestIntentFailureIs500(t *testing.T) {
	h := newHarness(t, nil)
	h.nd.submitErr = errors.New("pg_ctl: could not send reload signal")
	rec := h.do("POST", "/v1/reload", `{"node":"pg-0"}`, "ops")
	wantCode(t, rec, 500)
}

// --- routing, bodies, panics ---

func TestUnknownRouteListsRoutes(t *testing.T) {
	rec := newHarness(t, nil).do("GET", "/v1/nope", "", "ops")
	wantCode(t, rec, 404)
	if !strings.Contains(rec.Body.String(), "/v1/status") {
		t.Errorf("a typo should be self-correcting: %s", rec.Body.String())
	}
}

// A GET on a mutating path must not be answered as a read.
func TestWrongMethodIs405(t *testing.T) {
	rec := newHarness(t, nil).do("GET", "/v1/pause", "", "ops")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// A typo in a destructive request must fail rather than default the field it meant.
func TestUnknownBodyFieldIsRejected(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.do("POST", "/v1/restart", `{"node":"pg-0","forced":true}`, "ops")
	wantCode(t, rec, 400)
	if len(h.nd.submitted) != 0 {
		t.Error("no intent from a malformed request")
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	h := newHarness(t, nil)
	big := `{"node":"` + strings.Repeat("x", maxBodyBytes+1024) + `"}`
	rec := h.do("POST", "/v1/restart", big, "ops")
	if rec.Code != 400 && rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want a 4xx rejection; body: %s", rec.Code, rec.Body.String())
	}
}

// The server shares a process with PID 1: a handler panic must become a 500, not a
// dead postmaster parent.
func TestPanicIsContainedAsA500(t *testing.T) {
	h := newHarness(t, nil)
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	rec := httptest.NewRecorder()
	h.srv.recoverMW(panicking).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/status", nil))
	wantCode(t, rec, 500)
	if strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("the panic value must not reach the client: %s", rec.Body.String())
	}
}

func TestConcurrencyLimitReturns503(t *testing.T) {
	h := newHarness(t, nil)
	for i := 0; i < maxConcurrent; i++ {
		h.srv.sem <- struct{}{}
	}
	rec := h.do("GET", "/v1/status", "", "ops")
	wantCode(t, rec, 503)
}

func TestReadResponsesAreNotCached(t *testing.T) {
	rec := newHarness(t, nil).do("GET", "/v1/status", "", "ops")
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

var _ = io.Discard

// --- reinitialize (#276 phase 2) ---

// The gate that matters: wiping the primary would discard the cluster's only copy of
// committed data. reinitialize is replica-only, and the check reads the lease state rather
// than trusting a role string.
func TestReinitializeRefusedOnTheLeaseHolder(t *testing.T) {
	h := newHarness(t, nil) // healthySnapshot holds the lease and is primary
	rec := h.do("POST", "/v1/reinitialize", `{"node":"pg-0","force":true}`, "ops")
	wantCode(t, rec, 409)
	if !strings.Contains(rec.Body.String(), "replica-only") {
		t.Errorf("the error should say why: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/v1/switchover") || !strings.Contains(rec.Body.String(), "/v1/restore") {
		t.Errorf("it should point at the two legitimate alternatives: %s", rec.Body.String())
	}
	if len(h.nd.submitted) != 0 {
		t.Error("nothing may be submitted for the primary")
	}
}

// Running read-write without the lease is a fenced/split-brain shape the loop is about to
// resolve; a destructive local action must not race it.
func TestReinitializeRefusedWhenReadWriteWithoutLease(t *testing.T) {
	h := newHarness(t, nil)
	h.nd.snap.HoldsLease = false // still Role == "primary"
	rec := h.do("POST", "/v1/reinitialize", `{"node":"pg-0","force":true}`, "ops")
	wantCode(t, rec, 409)
	if len(h.nd.submitted) != 0 {
		t.Error("nothing may be submitted")
	}
}

// A paused loop performs no clone, so a wipe would leave the replica empty and stopped.
func TestReinitializeRefusedWhilePaused(t *testing.T) {
	h := newHarness(t, nil)
	h.nd.snap.HoldsLease = false
	h.nd.snap.Local.Role = "standby"
	h.cl.marker.Paused = true
	rec := h.do("POST", "/v1/reinitialize", `{"node":"pg-0","force":true}`, "ops")
	wantCode(t, rec, 409)
	if !strings.Contains(rec.Body.String(), "/v1/resume") {
		t.Errorf("the error should name the fix: %s", rec.Body.String())
	}
	if len(h.nd.submitted) != 0 {
		t.Error("nothing may be submitted while paused")
	}
}

func TestReinitializeRequiresForceAndNode(t *testing.T) {
	standby := func() *harness {
		h := newHarness(t, nil)
		h.nd.snap.HoldsLease = false
		h.nd.snap.Local.Role = "standby"
		return h
	}
	h := standby()
	wantCode(t, h.do("POST", "/v1/reinitialize", `{"node":"pg-0"}`, "ops"), 400)
	wantCode(t, standby().do("POST", "/v1/reinitialize", `{"force":true}`, "ops"), 400)
	// Addressed to another pod: refused, like every other node-local verb.
	wantCode(t, standby().do("POST", "/v1/reinitialize", `{"node":"pg-1","force":true}`, "ops"), 409)
	if len(h.nd.submitted) != 0 {
		t.Error("nothing may be submitted")
	}
}

func TestReinitializeOnAStandbySubmitsTheIntent(t *testing.T) {
	h := newHarness(t, nil)
	h.nd.snap.HoldsLease = false
	h.nd.snap.Local.Role = "standby"
	rec := h.do("POST", "/v1/reinitialize", `{"node":"pg-0","force":true}`, "ops")
	wantCode(t, rec, 202)
	if len(h.nd.submitted) != 1 || h.nd.submitted[0] != IntentReinitialize {
		t.Fatalf("submitted = %v, want one IntentReinitialize", h.nd.submitted)
	}
	resp := decode[actionResponse](t, rec)
	if resp.Result != "reinitializing" || resp.Node != "pg-0" {
		t.Errorf("bad response: %+v", resp)
	}
	// The clone is the loop's job and takes as long as the data is big, so the response
	// must say how to watch it rather than implying completion.
	if len(resp.Notes) == 0 || !strings.Contains(strings.Join(resp.Notes, " "), "hasData") {
		t.Errorf("notes should explain how to watch the re-clone: %v", resp.Notes)
	}
}

// A marker read failure must not be treated as "not paused, not the leader".
func TestReinitializeFailsClosedWhenTheMarkerIsUnreadable(t *testing.T) {
	h := newHarness(t, nil)
	h.nd.snap.HoldsLease = false
	h.nd.snap.Local.Role = "standby"
	h.cl.markerErr = errors.New("apiserver unreachable")
	rec := h.do("POST", "/v1/reinitialize", `{"node":"pg-0","force":true}`, "ops")
	wantCode(t, rec, 502)
	if len(h.nd.submitted) != 0 {
		t.Error("an unverifiable precondition must not proceed")
	}
}

func TestReinitializeIsListedAndMethodGuarded(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.do("GET", "/v1/nope", "", "ops")
	if !strings.Contains(rec.Body.String(), "/v1/reinitialize") {
		t.Errorf("the route list should include reinitialize: %s", rec.Body.String())
	}
	if got := h.do("GET", "/v1/reinitialize", "", "ops").Code; got != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/reinitialize = %d, want 405", got)
	}
}

// hasData/running are what a client polls while a reinitialize re-clone runs, so `false`
// must be REPORTED, not omitted -- otherwise "no data yet" and "field absent" look alike.
func TestMemberBooleansAreAlwaysReported(t *testing.T) {
	h := newHarness(t, nil)
	h.nd.snap.Local.HasData = false
	h.nd.snap.Local.Running = false
	h.nd.snap.Local.Reachable = false
	body := h.do("GET", "/v1/status", "", "ops").Body.String()
	for _, want := range []string{`"hasData": false`, `"running": false`, `"reachable": false`} {
		if !strings.Contains(body, want) {
			t.Errorf("status body should contain %s:\n%s", want, body)
		}
	}
}
