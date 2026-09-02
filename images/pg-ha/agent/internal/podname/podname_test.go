package podname

import "testing"

func TestOrdinal(t *testing.T) {
	for _, c := range []struct {
		pod  string
		want int
		ok   bool
		why  string
	}{
		{"pg-0", 0, true, "the ordinary case"},
		{"pg-agent-11", 11, true, "a base name containing its own separator"},
		{"my-release-pgvector-2", 2, true, "the pgvector fullname shape"},
		{"pg-", 0, false, "no ordinal at all"},
		{"-0", 0, false, "no base name: not a StatefulSet pod (cmd/agent used to accept this)"},
		{"0", 0, false, "a bare number is not a pod name"},
		{"pg", 0, false, "no separator"},
		{"pg-x", 0, false, "unparseable ordinal"},
		// Not a negative ordinal: LastIndex takes the FINAL separator, so the base is "pg-"
		// and the ordinal is 1. The n < 0 guard in Ordinal is therefore unreachable through
		// this path by construction -- kept as a cheap invariant, not as live validation.
		{"pg--1", 1, true, "the last dash is the separator, whatever precedes it"},
		{"", 0, false, "empty"},
	} {
		got, ok := Ordinal(c.pod)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("Ordinal(%q) = (%d, %v), want (%d, %v) -- %s", c.pod, got, ok, c.want, c.ok, c.why)
		}
	}
}

func TestOrdinalOrAndBase(t *testing.T) {
	if got := OrdinalOr("pg-3", -1); got != 3 {
		t.Errorf("OrdinalOr(pg-3) = %d, want 3", got)
	}
	if got := OrdinalOr("nope", -1); got != -1 {
		t.Errorf("OrdinalOr must fall back to the sentinel, got %d", got)
	}
	for pod, want := range map[string]string{
		"pg-0":          "pg",
		"pg-agent-11":   "pg-agent",
		"no-ordinal-xx": "no-ordinal-xx",
		"pg":            "pg",
	} {
		if got := Base(pod); got != want {
			t.Errorf("Base(%q) = %q, want %q", pod, got, want)
		}
	}
}
