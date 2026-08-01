package pg

import "testing"

// Parity with the shell tl_to_int unit tests (images/repmgr/test/scripts-test.sh):
// the timeline is hex; a decimal parse errors at 0x0A and is wrong from 0x10.
func TestParseTimeline(t *testing.T) {
	cases := []struct {
		in   string
		want Timeline
		ok   bool
	}{
		{"00000001", 1, true},
		{"00000009", 9, true},     // last timeline where hex == decimal
		{"0000000A", 10, true},    // a ::int cast ERRORS here (#168)
		{"00000010", 16, true},    // a ::int cast yields 10 here (#168)
		{"000000FF", 255, true},
		{"0000ABCD", 43981, true},
		{"FFFFFFFF", 4294967295, true}, // max 32-bit timeline
		{"", 0, false},                 // unreadable
		{"0000000G", 0, false},         // non-hex
		{"100000000", 0, false},        // overflows 32 bits
	}
	for _, c := range cases {
		got, ok := ParseTimeline(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseTimeline(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
