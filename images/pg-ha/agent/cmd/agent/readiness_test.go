package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cagriekin/pg-ha-agent/internal/observe"
	"github.com/cagriekin/pg-ha-agent/internal/process"
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

// #395: a restarted postmaster may have loaded `ssl = off`; the throttled TLS check must run
// again on the next Running tick instead of trusting the previous postmaster's answer.
func TestStartPostgresRearmsTheTLSCheck(t *testing.T) {
	ex := &tlsExec{ssl: "on"}
	a := newTLSTestAgent(ex, true)
	a.sup = process.NewSupervisor(&fakePostmaster{})
	a.verifyTLSActive(context.Background())
	ex.ssl = "off"
	if err := a.startPostgres(context.Background()); err != nil {
		t.Fatalf("startPostgres: %v", err)
	}
	a.verifyTLSActive(context.Background())
	if ex.calls != 2 || !a.tlsInactiveLatched {
		t.Errorf("after a restart: calls = %d, latched = %v; want a re-probe that latches ssl=off", ex.calls, a.tlsInactiveLatched)
	}
}

// #395: `ssl` is a sighup parameter, so the agent's own reloads (after a Follow or a
// primary_conninfo patch) must re-arm the throttled check exactly like an operator reload.
func TestReloadPostgresRearmsTheTLSCheck(t *testing.T) {
	ex := &tlsExec{ssl: "on"}
	a := newTLSTestAgent(ex, true)
	a.sup = process.NewSupervisor(&fakePostmaster{running: true})
	a.verifyTLSActive(context.Background())
	ex.ssl = "off"
	if err := a.reloadPostgres(context.Background()); err != nil {
		t.Fatalf("reloadPostgres: %v", err)
	}
	a.verifyTLSActive(context.Background())
	if ex.calls != 2 || !a.tlsInactiveLatched {
		t.Errorf("after a reload: calls = %d, latched = %v; want a re-probe that latches ssl=off", ex.calls, a.tlsInactiveLatched)
	}
}

// #395: the synchronized_standby_slots reconcile reloads over SQL, which re-reads conf.d like a
// SIGHUP does, so it must re-arm the throttled check too.
func TestSyncStandbySlotsReloadRearmsTheTLSCheck(t *testing.T) {
	ex := &scriptedExec{slots: "pg_ha_slot_1|t|0|reserved|t\n"}
	a := newNativeSyncTestAgent(t, ex)
	a.tlsCheckedAt = time.Now()
	a.assertSyncStandbySlots(context.Background(), mustSlots(t, a), []string{"pg_ha_slot_1"}, true)
	if len(ex.slotSyncSQL) != 2 {
		t.Fatalf("expected ALTER SYSTEM + reload, got %v", ex.slotSyncSQL)
	}
	if !a.tlsCheckedAt.IsZero() {
		t.Errorf("tlsCheckedAt = %v after a pg_reload_conf; want it re-armed", a.tlsCheckedAt)
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

// #395 review: a long call that leaves the postmaster alone (role provisioning) keeps the
// verdict fresh; one that promotes or replaces it (clone, rewind, promote) is NotReady from its
// first moment, for that reason rather than "stale", however long it runs.
func TestLongCallsAgeReadinessOnlyWhenThePostmasterChanges(t *testing.T) {
	const bound = 40 * time.Millisecond
	for _, tc := range []struct {
		name       string
		run        func(a *agent, fn func() error) error
		wantReady  bool
		wantReason string
	}{
		{"beatKeepingReadiness", (*agent).beatKeepingReadiness, true, "primary"},
		{"beatDuring", (*agent).beatDuring, false, "promoting, cloning, rewinding or initialising"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newFollowTestAgent(t, &scriptedExec{})
			a.metr = observe.New()
			a.cfg.ReconcileInterval = 5 * time.Millisecond
			a.metr.SetReadiness(readinessVerdict(reconcile.LocalState{Running: true, HasData: true}, nil, false))
			var during int
			_ = tc.run(a, func() error {
				during, _ = readyz(t, a.metr, bound)
				time.Sleep(3 * bound)
				return nil
			})
			if (during == http.StatusOK) != tc.wantReady {
				t.Errorf("/readyz at the start of a long %s = %d, want ready=%v", tc.name, during, tc.wantReady)
			}
			code, body := readyz(t, a.metr, bound)
			if (code == http.StatusOK) != tc.wantReady || !strings.Contains(body, tc.wantReason) {
				t.Errorf("/readyz after a long %s = %d %q, want ready=%v containing %q", tc.name, code, body, tc.wantReady, tc.wantReason)
			}
		})
	}
}
