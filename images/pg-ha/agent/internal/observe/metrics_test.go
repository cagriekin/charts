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
