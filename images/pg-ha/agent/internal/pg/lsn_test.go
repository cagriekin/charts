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
