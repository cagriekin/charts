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
	recoveryStarts  atomic.Int64 // recovery-mode (read-only WAL replay) entries
	lastBeatUnixNs  atomic.Int64 // last reconcile-loop heartbeat
	// Control API (#276). Counted here so the control plane is observable from the
	// SAME read-only surface Prometheus already scrapes -- a denied or failing control
	// call must be alertable without opening the control port to the scraper.
	controlRequests        atomic.Int64
	controlRejected        atomic.Int64
	controlIntents         atomic.Int64
	controlRestoreRequests atomic.Int64
	// Physical replication slots as the primary last observed them (#289). A neglected
	// slot fails silently in one of two ways and nothing else reports either: it holds WAL
	// back until the volume fills, or -- on chart defaults, where the image caps it with
	// max_slot_wal_keep_size = 4GB -- PostgreSQL invalidates it and the standby behind it
	// can only recover by a full re-clone. Hence a metric and an alert, not just a log line.
	// Zeroed on demotion (ClearSlots): only the primary observes slots.
	slotsTotal              atomic.Int64
	slotsInactive           atomic.Int64
	slotsInvalidated        atomic.Int64
	slotMaxRetainedWALBytes atomic.Int64
	now                     func() time.Time
}

// SlotStats is the aggregate slot picture the primary publishes each tick (#289).
//
// Aggregates, not per-slot labels: this metrics surface is hand-written Prometheus text
// with no per-series lifecycle, so a label per slot would leak a stale series every time
// a slot is dropped -- and the question worth alerting on ("is some slot holding back too
// much WAL?") is answered by the maximum. Per-slot identity stays in the agent's logs and
// in pg_replication_slots.
type SlotStats struct {
	Total    int64
	Inactive int64
	// Invalidated counts slots PostgreSQL has killed for exceeding max_slot_wal_keep_size
	// (wal_status = "lost"). It needs its own gauge because retained-WAL alerting is blind
	// to it: invalidation nulls restart_lsn, so MaxRetainedWALBytes collapses to zero at the
	// exact moment the slot dies. The image sets max_slot_wal_keep_size = 4GB at initdb, so
	// on chart defaults this -- not an unbounded disk-fill -- is where a neglected slot ends
	// up, and the standby behind it then needs a full re-clone.
	Invalidated         int64
	MaxRetainedWALBytes int64
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

func (m *Metrics) SetLeader(v bool)   { m.isLeader.Store(b2i(v)) }
func (m *Metrics) SetPaused(v bool)   { m.isPaused.Store(b2i(v)) }
func (m *Metrics) IncRenewFailure()   { m.renewFailures.Add(1) }
func (m *Metrics) IncPromotion()      { m.promotions.Add(1) }
func (m *Metrics) IncDemote()         { m.demotes.Add(1) }
func (m *Metrics) IncFence()          { m.fences.Add(1) }
func (m *Metrics) IncReconcileError() { m.reconcileErrors.Add(1) }
func (m *Metrics) IncRecoveryStart()  { m.recoveryStarts.Add(1) }

// Control-API counters. IncControlRequest counts every authenticated request,
// IncControlRejected every one refused by authn/authz, IncControlIntent every
// node-local operation handed to the reconcile loop, and IncControlRestoreRequest
// every accepted restore trigger (the one verb worth alerting on by itself).
func (m *Metrics) IncControlRequest()        { m.controlRequests.Add(1) }
func (m *Metrics) IncControlRejected()       { m.controlRejected.Add(1) }
func (m *Metrics) IncControlIntent()         { m.controlIntents.Add(1) }
func (m *Metrics) IncControlRestoreRequest() { m.controlRestoreRequests.Add(1) }

// SetSlots publishes the primary's observed physical replication slots (#289).
func (m *Metrics) SetSlots(s SlotStats) {
	m.slotsTotal.Store(s.Total)
	m.slotsInactive.Store(s.Inactive)
	m.slotsInvalidated.Store(s.Invalidated)
	m.slotMaxRetainedWALBytes.Store(s.MaxRetainedWALBytes)
}

// ClearSlots zeroes the slot gauges (#289). Only the primary observes slots, so a node that
// stops being primary must retract what it published rather than leave it standing: the slot
// alerts aggregate with max() across the release, so one demoted pod still exporting the
// figures it saw while primary would latch those alerts on for the rest of its process
// lifetime and keep paging after the condition moved or was resolved.
func (m *Metrics) ClearSlots() { m.SetSlots(SlotStats{}) }

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
		{"pg_ha_agent_recovery_starts_total", "Recovery-mode (read-only WAL replay) starts at cold boot.", "counter", m.recoveryStarts.Load()},
		{"pg_ha_agent_control_requests_total", "Authenticated control-API requests.", "counter", m.controlRequests.Load()},
		{"pg_ha_agent_control_rejected_total", "Control-API requests refused by authentication or authorization.", "counter", m.controlRejected.Load()},
		{"pg_ha_agent_control_intents_total", "Node-local control-API operations handed to the reconcile loop.", "counter", m.controlIntents.Load()},
		{"pg_ha_agent_control_restore_requests_total", "Restores triggered through the control API.", "counter", m.controlRestoreRequests.Load()},
		{"pg_ha_agent_replication_slots", "Physical replication slots on this primary.", "gauge", m.slotsTotal.Load()},
		{"pg_ha_agent_replication_slots_inactive", "Physical replication slots reserving WAL with no active consumer.", "gauge", m.slotsInactive.Load()},
		{"pg_ha_agent_replication_slots_invalidated", "Physical replication slots PostgreSQL invalidated for exceeding max_slot_wal_keep_size; the standby behind each needs a full re-clone.", "gauge", m.slotsInvalidated.Load()},
		{"pg_ha_agent_replication_slot_max_retained_wal_bytes", "Largest WAL volume retained by any one physical replication slot.", "gauge", m.slotMaxRetainedWALBytes.Load()},
	} {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", x.name, x.help, x.name, x.typ, x.name, x.val)
	}
}
