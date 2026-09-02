// Package pgbackrest is the agent's read-only window onto the pgBackRest
// repository and onto the outcome of the chart's restore Job (#276). It runs the
// pgbackrest CLI already present in the image and parses the artefacts the chart's
// restore.sh leaves behind; it never restores anything itself -- restoring over a
// live PGDATA is the Job's job, and doing it in-process while the agent supervises
// a running postmaster is explicitly out of scope.
package pgbackrest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/cagriekin/pg-ha-agent/internal/atomicfile"
)

// Runner runs an external command and returns its trimmed stdout. Structurally
// satisfied by pg.OSExec, so production wiring passes that and tests pass a fake
// without this package depending on the pg package.
type Runner interface {
	Run(ctx context.Context, env []string, name string, args ...string) (string, error)
}

// Client reads pgBackRest state for one stanza.
type Client struct {
	Exec   Runner
	Bin    string // pgbackrest binary; "pgbackrest" in the image
	Stanza string
	// StatusPath is the restore-outcome file restore.sh writes on the data volume.
	// It lives beside PGDATA (not inside it) so a restore never has to write into the
	// directory it is restoring, and so it survives on the PVC after the Job is gone.
	StatusPath string
}

// Info returns `pgbackrest info --output=json` verbatim: backup sets, WAL archive
// horizon, repository status. Passed through to the API rather than re-modelled --
// pgBackRest's own schema is the documented contract, and re-shaping it here would
// silently drop fields on a pgBackRest upgrade.
//
// Logging is turned fully off for this call. The mounted pgbackrest.conf sets
// log-level-console=info, whose output would otherwise interleave with the JSON on
// stdout, and file logging would need a writable log path this call has no business
// depending on. Failures still surface: a non-zero exit carries stderr in the error.
func (c Client) Info(ctx context.Context) (json.RawMessage, error) {
	out, err := c.Exec.Run(ctx, nil, c.bin(),
		"--stanza="+c.Stanza, "--output=json",
		"--log-level-console=off", "--log-level-file=off", "info")
	if err != nil {
		return nil, fmt.Errorf("pgbackrest info: %w", err)
	}
	doc := extractJSON(out)
	if doc == "" || !json.Valid([]byte(doc)) {
		// Never forward unvalidated bytes as JSON to a client: output we cannot find a
		// JSON document in must read as a server error, not a corrupt body.
		return nil, fmt.Errorf("pgbackrest info returned %d bytes with no valid JSON document", len(out))
	}
	return json.RawMessage(doc), nil
}

// extractJSON returns the JSON document in pgbackrest's stdout, skipping any leading
// non-JSON lines. It returns "" when there is none.
//
// This exists because pgBackRest writes some diagnostics to STDOUT before logging is even
// configured, so --log-level-console=off cannot suppress them. The concrete case: the
// chart puts PGBACKREST_ENABLED on the container env as a feature flag, and pgBackRest
// treats every PGBACKREST_* variable as an option, so it prepends
// "P00   WARN: environment contains invalid option 'enabled'" to the payload. (That
// warning is pre-existing and harmless everywhere else -- nothing else parses this
// stdout -- and only an unset, not an empty value, silences it.) Rather than depend on
// the exact set of warnings pgBackRest may emit, find where the document starts.
func extractJSON(out string) string {
	lines := strings.Split(out, "\n")
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") || strings.HasPrefix(t, "{") {
			return strings.Join(lines[i:], "\n")
		}
	}
	return ""
}

func (c Client) bin() string {
	if c.Bin == "" {
		return "pgbackrest"
	}
	return c.Bin
}

// RestoreRecord is the outcome of the last restore that ran on this data volume,
// read from the status file restore.sh writes. It is how the control API answers
// "what happened to my restore?" after the operator has scaled the StatefulSet back
// up: by then the Job may be gone (history limits) and its logs with it, but the
// volume still carries this. It is also data-directory provenance -- which backup
// set and which point in time this PGDATA came from -- which nothing else records.
type RestoreRecord struct {
	Present    bool   `json:"present"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
	// RestoredAt is the finishedAt of the last SUCCESSFUL restore on this volume, carried
	// across later failed attempts by write_status (#288 review). Prefer it over
	// FinishedAt+Succeeded() when asking "where did this data come from".
	RestoredAt string `json:"restoredAt,omitempty"`
	Stanza     string `json:"stanza,omitempty"`
	TargetType string `json:"targetType,omitempty"`
	Target     string `json:"target,omitempty"`
	BackupSet  string `json:"backupSet,omitempty"`
	// ExitCode is restore.sh's own exit status; 0 means pgbackrest reported success.
	// Absent (nil) when the file was written by a version that did not record it, or
	// when the restore died before writing the outcome.
	ExitCode *int `json:"exitCode,omitempty"`
	// ClusterState and Checkpoint are the post-restore pg_controldata readings --
	// the first thing to look at when a restored cluster does not come up.
	ClusterState string `json:"clusterState,omitempty"`
	Checkpoint   string `json:"checkpoint,omitempty"`
	// AdoptedAt is stamped by the AGENT, not by restore.sh, when this volume's restored
	// history became the cluster's own -- i.e. when this node promoted on it (#288 review,
	// round 2). It EXPIRES THE ELECTION CLAIM without erasing the record: the restored
	// timeline is now what the highwater marker records, so later elections are decided by
	// position alone, while GET /v1/status keeps reporting where this PGDATA came from. An
	// earlier revision unlinked the file instead, which expired the claim by destroying the
	// provenance #276 exists to preserve -- permanently, on the very restore that succeeded.
	AdoptedAt string `json:"adoptedAt,omitempty"`
	// RequestedBy is set when the restore was triggered through the control API,
	// carrying the client identity that asked for it.
	RequestedBy string `json:"requestedBy,omitempty"`
	// Attempted* describe a FAILED attempt's requested recovery point. On failure the
	// descriptive fields above keep the previous (successful) restore's values, because
	// many failures copy nothing and must not erase this directory's provenance -- so the
	// attempt is reported here instead. Empty for a successful restore.
	AttemptedTargetType string `json:"attemptedTargetType,omitempty"`
	AttemptedTarget     string `json:"attemptedTarget,omitempty"`
	AttemptedBackupSet  string `json:"attemptedBackupSet,omitempty"`
}

// Succeeded reports a restore that completed with exit code 0. An absent record or
// a missing exit code is NOT success -- an interrupted restore must never read as a
// clean one.
func (r RestoreRecord) Succeeded() bool {
	return r.Present && r.ExitCode != nil && *r.ExitCode == 0
}

// LastRestore reads the restore status file. A missing file is (zero, nil): no
// restore has run on this volume, which is the normal case and not an error.
// Unknown keys are ignored so a future restore.sh can add fields without breaking
// an older agent mid-upgrade.
func (c Client) LastRestore() (RestoreRecord, error) {
	if c.StatusPath == "" {
		return RestoreRecord{}, nil
	}
	// Bounded read: this file is a handful of key=value lines, and it lives on a
	// volume the agent does not exclusively own.
	b, err := readFileLimit(c.StatusPath, 16<<10)
	if os.IsNotExist(err) {
		return RestoreRecord{}, nil
	}
	if err != nil {
		return RestoreRecord{}, fmt.Errorf("read restore status %s: %w", c.StatusPath, err)
	}
	r := RestoreRecord{Present: true}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "startedAt":
			r.StartedAt = v
		case "finishedAt":
			r.FinishedAt = v
		case "restoredAt":
			r.RestoredAt = v
		case "adoptedAt":
			r.AdoptedAt = v
		case "stanza":
			r.Stanza = v
		case "targetType":
			r.TargetType = v
		case "target":
			r.Target = v
		case "backupSet":
			r.BackupSet = v
		case "exitCode":
			if n, cerr := strconv.Atoi(v); cerr == nil {
				r.ExitCode = &n
			}
		case "clusterState":
			r.ClusterState = v
		case "checkpoint":
			r.Checkpoint = v
		case "requestedBy":
			r.RequestedBy = v
		case "attemptedTargetType":
			r.AttemptedTargetType = v
		case "attemptedTarget":
			r.AttemptedTarget = v
		case "attemptedBackupSet":
			r.AttemptedBackupSet = v
		}
	}
	return r, nil
}

// MarkAdopted stamps adoptedAt on the restore record, expiring its election claim while
// KEEPING every other field (#288 review, round 2).
//
// Rewrites the raw bytes rather than re-serialising the parsed struct: LastRestore ignores
// unknown keys on purpose, so a restore.sh newer than this agent can add fields, and a
// round-trip through RestoreRecord would silently drop them. Atomic, because this file is the
// only record of the volume's provenance and a torn write loses it as surely as an unlink.
// A missing file is not an error: nothing to adopt.
func (c Client) MarkAdopted(ts string) error {
	if c.StatusPath == "" {
		return nil
	}
	// UNBOUNDED read, unlike LastRestore's (#298): readFileLimit truncates silently, which is
	// fine for a reader but not for this read-MODIFY-write -- the rewrite below would persist
	// the truncation and destroy the provenance record this function exists to preserve. The
	// file is a handful of key=value lines on a volume the agent owns.
	b, err := os.ReadFile(c.StatusPath) //nolint:gosec // path is agent config, not request input
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read restore status %s: %w", c.StatusPath, err)
	}
	kept := make([]string, 0)
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if k, _, ok := strings.Cut(strings.TrimSpace(line), "="); ok && strings.TrimSpace(k) == "adoptedAt" {
			continue // idempotent: the newest stamp replaces an older one
		}
		kept = append(kept, line)
	}
	kept = append(kept, "adoptedAt="+ts)
	// atomicfile, not a fourth hand-rolled temp+fsync+rename (#298).
	if err := atomicfile.WriteString(c.StatusPath, strings.Join(kept, "\n")+"\n", 0o600); err != nil {
		return fmt.Errorf("write restore status %s: %w", c.StatusPath, err)
	}
	return nil
}

func readFileLimit(path string, max int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // path is agent config, not request input
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, max))
}

// progressRe matches the cumulative percentage pgBackRest puts on each per-file
// restore line, e.g.
//
//	P01 DETAIL: restore file /pgdata/base/1/2657 (8KB, 12.5%) checksum e5f...
//
// Those lines are DETAIL level, so they appear only when the restore runs with a
// detail console log level -- which is why live progress is opt-in and the agent
// raises the level on the Job it creates. The pattern is deliberately loose (it
// anchors on the ", N%)" tail, not the whole line) so a wording change upstream
// degrades to "no percentage available" rather than a wrong number.
var progressRe = regexp.MustCompile(`,\s*([0-9]+(?:\.[0-9]+)?)%\)`)

// restoreFileRe counts per-file restore lines, the fallback progress signal when no
// percentage is present in the output at all.
var restoreFileRe = regexp.MustCompile(`(?m)^.*restore file `)

// Progress is what could be read out of a restore log.
type Progress struct {
	// Percent is pgBackRest's own cumulative figure from the most recent file line.
	Percent   float64 `json:"percent"`
	PercentOK bool    `json:"percentKnown"`
	FilesSeen int     `json:"filesSeen"`
}

// ParseProgress extracts progress from a restore log. It takes the LAST percentage
// in the stream (the figure is cumulative, so the last line is the current state)
// and always reports how many file lines were seen, so a caller has something to
// show even when the log carries no percentages.
func ParseProgress(log string) Progress {
	p := Progress{FilesSeen: len(restoreFileRe.FindAllString(log, -1))}
	if m := progressRe.FindAllStringSubmatch(log, -1); len(m) > 0 {
		if v, err := strconv.ParseFloat(m[len(m)-1][1], 64); err == nil {
			p.Percent, p.PercentOK = v, true
		}
	}
	return p
}
