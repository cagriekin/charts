package observe

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsExposition(t *testing.T) {
	m := New()
	m.SetLeader(true)
	m.IncPromotion()
	m.IncPromotion()
	m.IncFence()
	m.IncRecoveryStart()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	m.Handler(time.Minute).ServeHTTP(rr, req)

	body := rr.Body.String()
	for _, want := range []string{
		"pg_ha_agent_is_leader 1",
		"pg_ha_agent_promotions_total 2",
		"pg_ha_agent_fences_total 1",
		"pg_ha_agent_recovery_starts_total 1",
		"# TYPE pg_ha_agent_is_leader gauge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q in:\n%s", want, body)
		}
	}
}

func TestLivenessReflectsHeartbeat(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	m := New()
	m.now = func() time.Time { return now }
	m.Beat() // heartbeat at "now"

	get := func() int {
		rr := httptest.NewRecorder()
		m.Handler(10 * time.Second).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return rr.Code
	}
	if code := get(); code != http.StatusOK {
		t.Errorf("fresh heartbeat: /healthz = %d, want 200", code)
	}
	now = now.Add(30 * time.Second) // loop wedged: no Beat for 30s > 10s maxAge
	if code := get(); code != http.StatusServiceUnavailable {
		t.Errorf("stale heartbeat: /healthz = %d, want 503", code)
	}
}

func TestAuditWritesStructuredReason(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))
	Audit(l, false, "DemoteFence", "", "read-write without the lease; demote now (soft fence)")
	out := buf.String()
	if !strings.Contains(out, "action=DemoteFence") || !strings.Contains(out, "hold_lease=false") {
		t.Errorf("audit line missing fields: %s", out)
	}
}

// The read-only surface must stay read-only. The control API (#276) lives on its own
// port precisely so a NetworkPolicy can admit Prometheus here and nobody there; a
// mutating route appearing on 9200 would silently undo that separation.
func TestMetricsHandlerServesNoControlRoutes(t *testing.T) {
	h := New().Handler(time.Minute)
	for _, path := range []string{
		"/v1/status", "/v1/cluster", "/v1/pause", "/v1/resume",
		"/v1/switchover", "/v1/restart", "/v1/reload", "/v1/backups", "/v1/restore",
	} {
		for _, method := range []string{"GET", "POST", "DELETE"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s on the metrics port = %d, want 404", method, path, rec.Code)
			}
		}
	}
}

// Only these three routes exist on the metrics port.
func TestMetricsHandlerRoutes(t *testing.T) {
	h := New().Handler(time.Minute)
	for _, path := range []string{"/metrics", "/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
}

// The control counters are exported so a denied control call is alertable without
// opening the control port to the scraper.
func TestControlCountersAreExported(t *testing.T) {
	m := New()
	m.IncControlRequest()
	m.IncControlRejected()
	m.IncControlIntent()
	m.IncControlRestoreRequest()
	var sb strings.Builder
	m.write(&sb)
	for _, want := range []string{
		"pg_ha_agent_control_requests_total 1",
		"pg_ha_agent_control_rejected_total 1",
		"pg_ha_agent_control_intents_total 1",
		"pg_ha_agent_control_restore_requests_total 1",
	} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("metrics output should contain %q:\n%s", want, sb.String())
		}
	}
}
