package pg

import (
	"context"
	"strings"
)

// ReplayProgress is a recovering node's WAL-replay position. It exists for the
// control API's restore/PITR story (#276): after a pgBackRest restore the operator
// scales the StatefulSet back up and then waits, and the question that matters is
// not "how many files were copied" (that Job is already gone) but "how far has
// recovery got, and when can I use this database". Replay position answers it, and
// for a point-in-time restore LastReplayTime answers it in the units the operator
// actually specified the target in.
//
// Every field is best-effort: an unreachable or not-yet-ready node reports
// Reachable=false with the rest zero, so a caller can never mistake "unknown" for
// "caught up".
type ReplayProgress struct {
	Reachable  bool
	InRecovery bool
	// ReceiveLSN is the furthest WAL received from the archive/upstream, ReplayLSN
	// the furthest actually replayed. Their gap is the remaining work. Both are NULL
	// on a node that never entered recovery, hence the separate OK flags.
	ReceiveLSN LSN
	ReceiveOK  bool
	ReplayLSN  LSN
	ReplayOK   bool
	// LastReplayTime is pg_last_xact_replay_timestamp() as PostgreSQL renders it --
	// the commit timestamp of the last replayed transaction. For a `targetType: time`
	// restore this is directly comparable to the requested target, which is what makes
	// it the most useful single progress signal during a PITR.
	LastReplayTime string
}

// ReplayLagBytes is the WAL still to replay (received minus replayed). ok is false
// unless both positions are readable, so an unknown gap is never reported as zero
// -- "0 bytes behind" and "no idea" must not look alike to an operator deciding
// whether recovery has finished.
func (r ReplayProgress) ReplayLagBytes() (lag uint64, ok bool) {
	if !r.ReceiveOK || !r.ReplayOK {
		return 0, false
	}
	recv, replay := r.ReceiveLSN.Uint64(), r.ReplayLSN.Uint64()
	if replay >= recv {
		return 0, true
	}
	return recv - replay, true
}

// ReplayProgress reads the node's recovery position in one round trip. An error is
// returned only for a failed query; a reachable node outside recovery is a normal
// result with InRecovery=false.
func (p *Prober) ReplayProgress(ctx context.Context, ci ConnInfo) (ReplayProgress, error) {
	// ::text rather than a to_char format: this value is passed through to the API
	// response for a human to read, and PostgreSQL's own rendering already carries
	// the session time zone offset.
	out, err := p.psql(ctx, ci, "SELECT pg_is_in_recovery(), "+
		"COALESCE(pg_last_wal_receive_lsn()::text, ''), "+
		"COALESCE(pg_last_wal_replay_lsn()::text, ''), "+
		"COALESCE(pg_last_xact_replay_timestamp()::text, '');")
	if err != nil {
		return ReplayProgress{}, err
	}
	f := strings.Split(out, "|")
	if len(f) != 4 {
		return ReplayProgress{}, nil // unexpected shape: report unknown, never a guess
	}
	r := ReplayProgress{Reachable: true, InRecovery: strings.TrimSpace(f[0]) == "t"}
	r.ReceiveLSN, r.ReceiveOK = ParseLSN(strings.TrimSpace(f[1]))
	r.ReplayLSN, r.ReplayOK = ParseLSN(strings.TrimSpace(f[2]))
	r.LastReplayTime = strings.TrimSpace(f[3])
	return r, nil
}
