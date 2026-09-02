package pg

import "testing"

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

// LSN segments are UNPADDED hex, so a lexicographic comparison ranks "9/.." above
// "10/.." and "F2/.." above "100/..". Survivor ranking picks the standby with the
// highest LSN, so getting this backwards promotes the node that is FURTHEST BEHIND and
// silently discards the writes the others had (#131).
func TestLSNGreaterComparesNumericallyNotLexicographically(t *testing.T) {
	mk := func(s string) LSN {
		l, ok := ParseLSN(s)
		if !ok {
			t.Fatalf("ParseLSN(%q) failed", s)
		}
		return l
	}
	for _, c := range []struct{ hi, lo string }{
		// The lexicographic traps: "10" < "9" and "100" < "F2" as strings.
		{"10/0", "9/FFFFFFFF"},
		{"100/0", "F2/FFFFFFFF"},
		// hi dominates lo outright.
		{"17/0", "16/FFFFFFFF"},
		// equal hi: lo decides, and it is unpadded too.
		{"16/10000000", "16/9000000"},
	} {
		high, low := mk(c.hi), mk(c.lo)
		if !high.Greater(low) {
			t.Errorf("%s must rank above %s", c.hi, c.lo)
		}
		if low.Greater(high) {
			t.Errorf("%s must NOT rank above %s", c.lo, c.hi)
		}
	}
	// Strictly greater: an equal position is not ahead, or two caught-up standbys
	// would each consider itself the better survivor.
	same := mk("16/B374D848")
	if same.Greater(mk("16/B374D848")) {
		t.Error("Greater must be strict: an equal LSN is not ahead")
	}
}

// Role is what the decision engine branches on and what the pg-role label carries, so
// the three strings are a contract, not a debug convenience. "unknown" in particular is
// load-bearing: the restart interlock fails closed on anything that is not "standby".
func TestRoleStringsAreTheContract(t *testing.T) {
	if got := RolePrimary.String(); got != "primary" {
		t.Errorf("RolePrimary = %q, want primary", got)
	}
	if got := RoleStandby.String(); got != "standby" {
		t.Errorf("RoleStandby = %q, want standby", got)
	}
	// Every other value, including the zero one, is "unknown" rather than a number:
	// an unrecognised role must never be mistaken for a real one.
	if got := Role(0).String(); got != "unknown" {
		t.Errorf("the zero Role = %q, want unknown", got)
	}
	if got := Role(99).String(); got != "unknown" {
		t.Errorf("an out-of-range Role = %q, want unknown", got)
	}
}
