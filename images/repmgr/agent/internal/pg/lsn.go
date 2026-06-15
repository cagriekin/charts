package pg

import (
	"strconv"
	"strings"
)

// LSN is a PostgreSQL log sequence number — the "hi/lo" pair reported by
// pg_current_wal_lsn() (e.g. "16/B374D848"). Both segments are hexadecimal.
type LSN struct {
	Hi uint64
	Lo uint64
}

// ParseLSN decodes an "X/Y" hex LSN. ok is false when the string is empty, has
// no '/', or either segment is missing/not hex.
func ParseLSN(s string) (lsn LSN, ok bool) {
	slash := strings.IndexByte(s, '/')
	if slash <= 0 || slash == len(s)-1 {
		return LSN{}, false
	}
	hi, err := strconv.ParseUint(s[:slash], 16, 64)
	if err != nil {
		return LSN{}, false
	}
	lo, err := strconv.ParseUint(s[slash+1:], 16, 64)
	if err != nil {
		return LSN{}, false
	}
	return LSN{Hi: hi, Lo: lo}, true
}

// Greater reports whether a is a strictly higher LSN than b: the hi segment
// dominates, then lo. Both compare numerically, never lexicographically (#131) —
// the segments are unpadded hex, so "10/.." must rank above "9/.." and "100/.."
// above "F2/..".
func (a LSN) Greater(b LSN) bool {
	if a.Hi != b.Hi {
		return a.Hi > b.Hi
	}
	return a.Lo > b.Lo
}

// LSNGreater compares two raw LSN strings with the exact semantics of the shell
// lsn_gt used in split-brain survivor selection: an empty or unreadable b loses to
// any a (a wins); an empty or unreadable a loses to a readable b; a malformed
// value is the loser. This keeps survivor ranking bit-identical to the bash oracle.
func LSNGreater(a, b string) bool {
	bl, bok := ParseLSN(b)
	if !bok {
		return true // nothing (or garbage) on the b side: a wins
	}
	al, aok := ParseLSN(a)
	if !aok {
		return false // a unreadable but b readable: b wins
	}
	return al.Greater(bl)
}
