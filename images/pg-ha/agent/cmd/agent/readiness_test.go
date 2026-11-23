package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cagriekin/pg-ha-agent/internal/observe"
	"github.com/cagriekin/pg-ha-agent/internal/reconcile"
)

func readyz(t *testing.T, m *observe.Metrics, maxAge time.Duration) (int, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	m.Handler(maxAge).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rr.Code, rr.Body.String()
}

// #395: every state the exec readiness probe distinguished, through the agent's HTTP handler.
func TestReadyzStates(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name        string
		local       reconcile.LocalState
		streaming   *bool
		tlsInactive bool
		wantCode    int
		wantReason  string
	}{
		{"primary", reconcile.LocalState{Running: true, HasData: true}, nil, false, http.StatusOK, "primary"},
		{"standby streaming", reconcile.LocalState{Running: true, InRecovery: true, HasData: true}, &yes, false, http.StatusOK, "standby streaming"},
		{"standby catching up (no walreceiver)", reconcile.LocalState{Running: true, InRecovery: true, HasData: true}, &no, false, http.StatusServiceUnavailable, "not streaming"},
		{"standby walreceiver unreadable", reconcile.LocalState{Running: true, InRecovery: true, HasData: true}, nil, false, http.StatusServiceUnavailable, "could not be read"},
		{"standby cloning (no postmaster)", reconcile.LocalState{}, nil, false, http.StatusServiceUnavailable, "not accepting connections on 127.0.0.1"},
		// The bootstrap's transient postmaster listens on the socket only (#350): the agent's
		// loopback probe does not reach it, so Running is false while the process is alive.
		{"transient bootstrap postmaster", reconcile.LocalState{HasData: true}, nil, false, http.StatusServiceUnavailable, "not accepting connections on 127.0.0.1"},
		{"TLS requested but ssl=off, primary", reconcile.LocalState{Running: true, HasData: true}, nil, true, http.StatusServiceUnavailable, "ssl=off"},
		{"TLS requested but ssl=off, streaming standby", reconcile.LocalState{Running: true, InRecovery: true, HasData: true}, &yes, true, http.StatusServiceUnavailable, "ssl=off"},
		{"saturated", reconcile.LocalState{Saturated: true, HasData: true}, nil, false, http.StatusServiceUnavailable, "max_connections exhausted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := observe.New()
			m.SetReadiness(readinessVerdict(tc.local, tc.streaming, tc.tlsInactive))
			code, body := readyz(t, m, time.Minute)
			if code != tc.wantCode || !strings.Contains(body, tc.wantReason) {
				t.Errorf("/readyz = %d %q, want %d containing %q", code, body, tc.wantCode, tc.wantReason)
			}
		})
	}
}

// #395: a wedged reconcile loop must not keep serving its last good verdict.
func TestReadyzStaleLoop(t *testing.T) {
	m := observe.New()
	m.SetReadiness(readinessVerdict(reconcile.LocalState{Running: true, HasData: true}, nil, false))
	time.Sleep(2 * time.Millisecond)
	if code, body := readyz(t, m, time.Millisecond); code != http.StatusServiceUnavailable || !strings.Contains(body, "stale") {
		t.Errorf("/readyz on a stale verdict = %d %q, want 503 stale", code, body)
	}
}

// #395: the stall check's walreceiver read is the one readiness uses, so a standby's verdict
// needs no extra query; it must record both answers.
func TestObserveStandbyStallRecordsStreamingForReadiness(t *testing.T) {
	ex := &scriptedExec{walRcv: "pg-0.h|streaming"}
	a := newFollowTestAgent(t, ex)
	o := reconcile.Observation{Local: reconcile.LocalState{Running: true, InRecovery: true, HasData: true}}
	a.observeStandbyStall(context.Background(), &o)
	if a.localStreaming == nil || !*a.localStreaming {
		t.Fatalf("streaming walreceiver not recorded: %v", a.localStreaming)
	}
	ex.walRcv = ""
	a.localStreaming = nil
	a.observeStandbyStall(context.Background(), &o)
	if a.localStreaming == nil || *a.localStreaming {
		t.Fatalf("absent walreceiver not recorded as not streaming: %v", a.localStreaming)
	}
}
