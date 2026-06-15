// Package pg provides the typed PostgreSQL primitives the agent reasons about:
// timelines and LSNs parsed from WAL positions, plus node recovery state. These
// are the correctness core ported from the shell service-updater (tl_to_int,
// lsn_gt) and must rank identically so the agent and the bash oracle agree during
// the dual-running window.
//
// Correctness note (#168): the timeline component of a WAL file name is
// HEXADECIMAL. Parsing it as a decimal integer (the original `::int` SQL cast)
// errors at timeline 0x0A and is silently wrong from 0x10 on. Timelines here are
// always decoded base 16.
package pg

import "strconv"

// Timeline is a PostgreSQL timeline ID. In a WAL file name it is the leading
// 8 hex digits; PostgreSQL stores it as a 32-bit unsigned integer.
type Timeline uint32

// ParseTimeline decodes a hex timeline token (e.g. the first 8 chars of a WAL
// file name) into a Timeline. ok is false when s is empty or not valid hex,
// mirroring the shell tl_to_int, which yields no value for unreadable input.
func ParseTimeline(s string) (tl Timeline, ok bool) {
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, false
	}
	return Timeline(v), true
}
