// Package observe is the agent's observability: Prometheus metrics, a liveness
// endpoint that reflects reconcile-loop progress (not just process-up), and a
// structured decision audit trail. The HTTP surface is strictly read-only — no
// route can change role or release the lease (security review H4).
package observe

import (
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// Metrics holds the agent's counters/gauges. All access is atomic so the metrics
// goroutine and the reconcile loop are race-free.
type Metrics struct {
	isLeader        atomic.Int64 // 0/1 gauge
	isPaused        atomic.Int64 // 0/1 gauge (maintenance mode, Part H1)
	renewFailures   atomic.Int64
	promotions      atomic.Int64
	demotes         atomic.Int64
	fences          atomic.Int64
	reconcileErrors atomic.Int64
	lastBeatUnixNs  atomic.Int64 // last reconcile-loop heartbeat
	now             func() time.Time
}

// New returns Metrics with the heartbeat primed so the agent is live at startup.
func New() *Metrics {
	m := &Metrics{now: time.Now}
	m.Beat()
	return m
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func (m *Metrics) SetLeader(v bool)     { m.isLeader.Store(b2i(v)) }
func (m *Metrics) SetPaused(v bool)     { m.isPaused.Store(b2i(v)) }
func (m *Metrics) IncRenewFailure()     { m.renewFailures.Add(1) }
func (m *Metrics) IncPromotion()        { m.promotions.Add(1) }
func (m *Metrics) IncDemote()           { m.demotes.Add(1) }
func (m *Metrics) IncFence()            { m.fences.Add(1) }
func (m *Metrics) IncReconcileError()   { m.reconcileErrors.Add(1) }

// Beat records that the reconcile loop ran; call it each tick.
func (m *Metrics) Beat() { m.lastBeatUnixNs.Store(m.now().UnixNano()) }

// Alive reports whether the reconcile loop has beaten within maxAge. This is what
// the liveness probe checks: a deadlocked agent (HTTP still up, loop wedged) is
// reported NOT alive so the kubelet restarts it.
func (m *Metrics) Alive(maxAge time.Duration) bool {
	last := time.Unix(0, m.lastBeatUnixNs.Load())
	return m.now().Sub(last) < maxAge
}

// Handler returns the read-only HTTP surface: /metrics (Prometheus text),
// /healthz (reconcile-loop liveness), /readyz (process up).
func (m *Metrics) Handler(livenessMaxAge time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		m.write(w)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if m.Alive(livenessMaxAge) {
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "ok")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "stale: reconcile loop has not progressed")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})
	return mux
}

func (m *Metrics) write(w io.Writer) {
	type metric struct {
		name, help, typ string
		val             int64
	}
	for _, x := range []metric{
		{"pg_ha_agent_is_leader", "Whether this agent currently holds the Lease.", "gauge", m.isLeader.Load()},
		{"pg_ha_agent_is_paused", "Whether maintenance mode is active (automatic failover suspended).", "gauge", m.isPaused.Load()},
		{"pg_ha_agent_renew_failures_total", "Lease renew failures.", "counter", m.renewFailures.Load()},
		{"pg_ha_agent_promotions_total", "Promotions performed.", "counter", m.promotions.Load()},
		{"pg_ha_agent_demotes_total", "Demotions performed.", "counter", m.demotes.Load()},
		{"pg_ha_agent_fences_total", "Soft fences performed.", "counter", m.fences.Load()},
		{"pg_ha_agent_reconcile_errors_total", "Reconcile-loop errors.", "counter", m.reconcileErrors.Load()},
	} {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", x.name, x.help, x.name, x.typ, x.name, x.val)
	}
}
