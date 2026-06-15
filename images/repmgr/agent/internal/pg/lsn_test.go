package pg

import "testing"

// Parity with the shell lsn_gt unit tests (pg/tests/test-template.sh): segments
// are unpadded hex and must compare numerically, not lexicographically (#131).
func TestLSNGreater(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"10/00000001", "9/2B3C4D50", true},   // hi 0x10 > 0x9 (lexicographic would say false)
		{"100/0", "F2/FFFFFFFF", true},        // hi 0x100 > 0xF2 (lexicographic '1' < 'F' would say false)
		{"9/2B3C4D50", "10/00000001", false},  // reverse of the first case
		{"5/100", "5/FF", true},               // equal hi, lo 0x100 > 0xFF
		{"5/FF", "5/100", false},              // equal hi, lo 0xFF < 0x100
		{"5/0", "5/0", false},                 // strictly greater, so equal is false
		{"5/0", "", true},                     // empty b: a wins
		{"", "5/0", false},                    // empty a: b wins
		{"", "", true},                        // both empty: b-empty checked first, a wins (bash parity)
		{"garbage", "5/0", false},             // a malformed: loses
		{"5/0", "garbage", true},              // b malformed: a wins
	}
	for _, c := range cases {
		if got := LSNGreater(c.a, c.b); got != c.want {
			t.Errorf("LSNGreater(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestParseLSN(t *testing.T) {
	if l, ok := ParseLSN("16/B374D848"); !ok || l.Hi != 0x16 || l.Lo != 0xB374D848 {
		t.Errorf("ParseLSN(16/B374D848) = (%+v, %v)", l, ok)
	}
	for _, bad := range []string{"", "16", "/5", "16/", "x/5", "16/zz"} {
		if _, ok := ParseLSN(bad); ok {
			t.Errorf("ParseLSN(%q) unexpectedly ok", bad)
		}
	}
}
