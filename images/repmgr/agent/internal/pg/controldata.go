package pg

import (
	"context"
	"strconv"
	"strings"
)

// ControlInfo is the local cluster state read from pg_controldata. Unlike the SQL
// probes it needs no running server, so the agent can learn a stopped node's
// timeline and on-disk role before deciding whether to start it read-write -- the
// guard that stops a fenced ex-primary from coming up read-write and flapping.
type ControlInfo struct {
	State      string   // pg_controldata "Database cluster state"
	Timeline   Timeline // "Latest checkpoint's TimeLineID"
	TimelineOK bool
	InRecovery bool // on-disk standby state ("in archive recovery" / "shut down in recovery")
}

// ReadControlData runs `pg_controldata -D <dataDir>` and parses the cluster state
// and latest-checkpoint timeline. LC_ALL=C is forced so the field labels are not
// localized. The control file is readable whether or not the server is running.
func ReadControlData(ctx context.Context, ex Exec, pgControlDataBin, dataDir string) (ControlInfo, error) {
	out, err := ex.Run(ctx, []string{"LC_ALL=C"}, pgControlDataBin, "-D", dataDir)
	if err != nil {
		return ControlInfo{}, err
	}
	return parseControlData(out), nil
}

// parseControlData parses pg_controldata's key:value output.
//
// Correctness note: pg_controldata prints "Latest checkpoint's TimeLineID" in
// DECIMAL, unlike the HEX timeline embedded in a WAL file name (see ParseTimeline).
// It is therefore parsed base 10 here.
func parseControlData(out string) ControlInfo {
	var ci ControlInfo
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "Database cluster state":
			ci.State = val
			ci.InRecovery = isRecoveryState(val)
		case "Latest checkpoint's TimeLineID":
			if n, err := strconv.ParseUint(val, 10, 32); err == nil {
				ci.Timeline, ci.TimelineOK = Timeline(n), true
			}
		}
	}
	return ci
}

// isRecoveryState reports whether a pg_controldata cluster state means the on-disk
// data is a standby (would start up in recovery), not a primary. "in crash
// recovery" is a primary replaying its own WAL before opening read-write and is
// NOT counted as a standby.
func isRecoveryState(state string) bool {
	switch state {
	case "in archive recovery", "shut down in recovery":
		return true
	default: // "in production", "shut down", "in crash recovery", "starting up", ...
		return false
	}
}
