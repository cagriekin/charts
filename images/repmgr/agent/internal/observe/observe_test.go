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
