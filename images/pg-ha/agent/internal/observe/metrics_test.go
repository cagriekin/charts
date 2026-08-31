package observe

import (
	"fmt"
	"strings"
	"testing"
)

// #289: the slot gauges are what turn a silent WAL-pinning orphan into an alertable
// signal, so they must actually appear in the scraped text with the right values.
func TestMetricsExportsSlotGauges(t *testing.T) {
	m := New()
	m.SetSlots(SlotStats{Total: 3, Inactive: 2, MaxRetainedWALBytes: 16 << 20})
	var b strings.Builder
	m.write(&b)
	out := b.String()
	for _, want := range []string{
		"pg_ha_agent_replication_slots 3",
		"pg_ha_agent_replication_slots_inactive 2",
		fmt.Sprintf("pg_ha_agent_replication_slot_max_retained_wal_bytes %d", 16<<20),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output missing %q:\n%s", want, out)
		}
	}
	// Each gauge needs its HELP/TYPE header or Prometheus treats it as untyped.
	for _, name := range []string{
		"pg_ha_agent_replication_slots",
		"pg_ha_agent_replication_slots_inactive",
		"pg_ha_agent_replication_slot_max_retained_wal_bytes",
	} {
		if !strings.Contains(out, "# TYPE "+name+" gauge") {
			t.Errorf("missing gauge TYPE header for %s:\n%s", name, out)
		}
	}
}

// The gauges must reflect the LATEST observation, not accumulate: a dropped orphan has to
// make the inactive count fall again or the alert never clears.
func TestMetricsSlotGaugesAreReplacedNotAccumulated(t *testing.T) {
	m := New()
	m.SetSlots(SlotStats{Total: 3, Inactive: 2, MaxRetainedWALBytes: 1 << 30})
	m.SetSlots(SlotStats{Total: 2, Inactive: 0, MaxRetainedWALBytes: 0})
	var b strings.Builder
	m.write(&b)
	out := b.String()
	for _, want := range []string{
		"pg_ha_agent_replication_slots 2",
		"pg_ha_agent_replication_slots_inactive 0",
		"pg_ha_agent_replication_slot_max_retained_wal_bytes 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("gauge not replaced, missing %q:\n%s", want, out)
		}
	}
}

// Every published metric must MOVE when its setter is called, and move
// independently of the others.
//
// This is the defect class that has bitten this branch twice: #289 shipped a slot
// alert whose gauge no code path published, and #298's review found
// pg_ha_agent_renew_failures_total with nothing incrementing it -- in both cases the
// chart's PrometheusRule was live, the scrape was green, and the alert could never
// fire. A gauge wired to the wrong atomic fails the same way and is invisible in
// review, so assert the wiring one metric at a time: set exactly one thing, and
// require exactly that line to change.
func TestEveryMetricIsWiredToItsOwnSetter(t *testing.T) {
	scrape := func(m *Metrics) string {
		var b strings.Builder
		m.write(&b)
		return b.String()
	}
	for _, c := range []struct {
		metric string
		want   string
		set    func(*Metrics)
	}{
		{"pg_ha_agent_is_leader", "1", func(m *Metrics) { m.SetLeader(true) }},
		{"pg_ha_agent_is_paused", "1", func(m *Metrics) { m.SetPaused(true) }},
		{"pg_ha_agent_tls_inactive", "1", func(m *Metrics) { m.SetTLSInactive(true) }},
		{"pg_ha_agent_renew_failures_total", "1", func(m *Metrics) { m.IncRenewFailure() }},
		{"pg_ha_agent_promotions_total", "1", func(m *Metrics) { m.IncPromotion() }},
		{"pg_ha_agent_demotes_total", "1", func(m *Metrics) { m.IncDemote() }},
		{"pg_ha_agent_fences_total", "1", func(m *Metrics) { m.IncFence() }},
		{"pg_ha_agent_reconcile_errors_total", "1", func(m *Metrics) { m.IncReconcileError() }},
		{"pg_ha_agent_recovery_starts_total", "1", func(m *Metrics) { m.IncRecoveryStart() }},
		{"pg_ha_agent_marker_tamper_suspected_total", "1", func(m *Metrics) { m.IncMarkerTamper() }},
		{"pg_ha_agent_control_requests_total", "1", func(m *Metrics) { m.IncControlRequest() }},
		{"pg_ha_agent_control_rejected_total", "1", func(m *Metrics) { m.IncControlRejected() }},
		{"pg_ha_agent_control_intents_total", "1", func(m *Metrics) { m.IncControlIntent() }},
		{"pg_ha_agent_control_restore_requests_total", "1", func(m *Metrics) { m.IncControlRestoreRequest() }},
		{"pg_ha_agent_replicas_streaming", "2", func(m *Metrics) { m.SetTopology(TopologyStats{Streaming: 2}) }},
		{"pg_ha_agent_replicas_expected", "3", func(m *Metrics) { m.SetTopology(TopologyStats{Expected: 3}) }},
		{"pg_ha_agent_replicas_unidentified", "4", func(m *Metrics) { m.SetTopology(TopologyStats{Unidentified: 4}) }},
		{"pg_ha_agent_replication_slots", "5", func(m *Metrics) { m.SetSlots(SlotStats{Total: 5}) }},
		{"pg_ha_agent_replication_slots_inactive", "6", func(m *Metrics) { m.SetSlots(SlotStats{Inactive: 6}) }},
		{"pg_ha_agent_replication_slots_invalidated", "7", func(m *Metrics) { m.SetSlots(SlotStats{Invalidated: 7}) }},
		{"pg_ha_agent_replication_slot_max_retained_wal_bytes", "8", func(m *Metrics) { m.SetSlots(SlotStats{MaxRetainedWALBytes: 8}) }},
	} {
		m := New()
		before := scrape(m)
		if !strings.Contains(before, "\n"+c.metric+" 0\n") {
			t.Errorf("%s: a fresh agent should publish it as 0:\n%s", c.metric, before)
		}
		c.set(m)
		after := scrape(m)
		if !strings.Contains(after, "\n"+c.metric+" "+c.want+"\n") {
			t.Errorf("%s: setter did not move it to %s:\n%s", c.metric, c.want, after)
		}
		// Nothing else moved: a setter storing into a neighbour's atomic would
		// otherwise pass the assertion above and silently mislabel two series.
		for _, line := range strings.Split(after, "\n") {
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			name, val, _ := strings.Cut(line, " ")
			if name != c.metric && val != "0" {
				t.Errorf("%s's setter also moved %s to %s", c.metric, name, val)
			}
		}
	}
}

// The clears exist because only the PRIMARY observes these, and the alerts aggregate
// with max() across the release: a demoted pod still exporting the figures it saw
// while primary would latch those alerts on for the rest of its process lifetime and
// keep paging after the condition moved or was resolved.
func TestClearsRetractWhatADemotedPodPublished(t *testing.T) {
	m := New()
	m.SetTopology(TopologyStats{Streaming: 2, Expected: 3, Unidentified: 1})
	m.SetSlots(SlotStats{Total: 4, Inactive: 2, Invalidated: 1, MaxRetainedWALBytes: 1 << 30})
	m.ClearTopology()
	m.ClearSlots()
	var b strings.Builder
	m.write(&b)
	for _, line := range strings.Split(b.String(), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, val, _ := strings.Cut(line, " ")
		// COUNTERS are exempt, by name (#298 review). The predicate below is a substring
		// match, so `pg_ha_agent_replication_slots_recycled_total` fell inside it -- and a
		// counter must never be retracted: rate() handles a reset as a counter reset, so
		// zeroing one on demotion loses the event the recycle alert exists to catch. The
		// exemption is asserted positively by TestSlotRecycleCounterSurvivesTheGaugeClear.
		if strings.HasSuffix(name, "_total") {
			continue
		}
		if (strings.Contains(name, "replicas_") || strings.Contains(name, "replication_slot")) && val != "0" {
			t.Errorf("%s = %s after the clear: an alert aggregating with max() would stay latched", name, val)
		}
	}
}

// The recycle COUNTER must survive ClearSlots (#298 review). Its whole reason for existing is
// that the recycle clears pg_ha_agent_replication_slots_invalidated in the same tick that
// observes it, so PGHAReplicationSlotInvalidated's `for: 5m` can never elapse -- the counter is
// the durable trace, and PGHAReplicationSlotRecycled rates it. Folding Recycled into SlotStats
// (the obvious next refactor: it sits among the slot fields and is set from the same tick) would
// have ClearSlots zero it on every demotion, and the alert would go quietly dead -- the exact
// dead-alert failure this chart has now shipped twice.
func TestSlotRecycleCounterSurvivesTheGaugeClear(t *testing.T) {
	m := New()
	m.SetSlots(SlotStats{Total: 4, Invalidated: 1})
	m.IncSlotRecycled()
	m.IncSlotRecycled()
	m.ClearSlots()
	var b strings.Builder
	m.write(&b)
	want := "pg_ha_agent_replication_slots_recycled_total 2"
	if !strings.Contains(b.String(), want+"\n") {
		t.Errorf("exposition does not carry %q after ClearSlots; a demoted pod must not retract a counter", want)
	}
	// ...and it is declared a counter, because rate() over a gauge is not a defined operation
	// and the alert would silently never fire.
	if !strings.Contains(b.String(), "# TYPE pg_ha_agent_replication_slots_recycled_total counter\n") {
		t.Error("pg_ha_agent_replication_slots_recycled_total must be TYPEd counter: PGHAReplicationSlotRecycled rates it")
	}
}

// Every metric carries its HELP and TYPE header, and every name is unique. Prometheus
// treats a header-less series as untyped, and a duplicated name makes the scrape
// itself invalid -- both are one careless copy-paste away in a flat literal list.
func TestEveryMetricHasAUniqueNameAndAHeader(t *testing.T) {
	var b strings.Builder
	New().write(&b)
	seen := map[string]bool{}
	out := b.String()
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, _, ok := strings.Cut(line, " ")
		if !ok {
			t.Errorf("sample line is not `name value`: %q", line)
			continue
		}
		if seen[name] {
			t.Errorf("duplicate metric name %s: the scrape is invalid", name)
		}
		seen[name] = true
		if !strings.Contains(out, "# HELP "+name+" ") {
			t.Errorf("%s has no HELP line", name)
		}
		if !strings.Contains(out, "# TYPE "+name+" gauge") && !strings.Contains(out, "# TYPE "+name+" counter") {
			t.Errorf("%s has no TYPE line", name)
		}
		// A _total suffix promises a counter, and Prometheus tooling relies on it.
		if strings.HasSuffix(name, "_total") && !strings.Contains(out, "# TYPE "+name+" counter") {
			t.Errorf("%s is named as a counter but not typed as one", name)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no metrics were published at all")
	}
}
