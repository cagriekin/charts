package k8s

import (
	"testing"
	"time"
)

func TestParseHandoff(t *testing.T) {
	start := time.Date(2026, 11, 12, 10, 0, 0, 123, time.UTC)
	if h := ParseHandoff(FormatHandoff("pg-1", start)); !h.OK || !h.Present || h.Target != "pg-1" || !h.Start.Equal(start) {
		t.Fatalf("round trip: %+v", h)
	}
	if h := ParseHandoff(""); h.Present || h.OK {
		t.Fatalf("empty: %+v", h)
	}
	for _, v := range []string{"pg-1", "@2026-11-12T10:00:00Z", "pg-1@yesterday"} {
		if h := ParseHandoff(v); !h.Present || h.OK {
			t.Errorf("%q: %+v, want present and not OK", v, h)
		}
	}
}
