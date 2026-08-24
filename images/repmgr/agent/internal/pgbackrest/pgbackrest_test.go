package pgbackrest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRunner struct {
	out  string
	err  error
	args []string
}

func (f *fakeRunner) Run(_ context.Context, _ []string, _ string, args ...string) (string, error) {
	f.args = args
	return f.out, f.err
}

func TestInfoPassesThroughJSON(t *testing.T) {
	r := &fakeRunner{out: `[{"name":"db","backup":[]}]`}
	c := Client{Exec: r, Stanza: "db"}
	got, err := c.Info(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != r.out {
		t.Errorf("Info() = %s, want verbatim %s", got, r.out)
	}
	joined := strings.Join(r.args, " ")
	// Logging must be off on both sinks: console output would interleave with the
	// JSON on stdout, and file logging would need a writable log path.
	for _, want := range []string{"--stanza=db", "--output=json", "--log-level-console=off", "--log-level-file=off", "info"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q should contain %q", joined, want)
		}
	}
}

// pgBackRest writes some diagnostics to STDOUT before logging is configured, so
// --log-level-console=off cannot suppress them. The real one seen in a live cluster: the
// chart's PGBACKREST_ENABLED feature flag looks like a (bogus) pgBackRest option, so a
// WARN is prepended to the payload. The document must still be found.
func TestInfoSkipsLeadingDiagnostics(t *testing.T) {
	const real = "P00   WARN: environment contains invalid option 'enabled'\n" +
		`[{"archive":[],"backup":[],"name":"db","status":{"code":0}}]`
	c := Client{Exec: &fakeRunner{out: real}, Stanza: "db"}
	got, err := c.Info(context.Background())
	if err != nil {
		t.Fatalf("a leading WARN must not fail the call: %v", err)
	}
	if strings.Contains(string(got), "WARN") {
		t.Errorf("the diagnostic must not be forwarded: %s", got)
	}
	var v []map[string]any
	if err := json.Unmarshal(got, &v); err != nil || len(v) != 1 || v[0]["name"] != "db" {
		t.Errorf("payload did not survive extraction: %s", got)
	}
}

// Output with no JSON document at all is still a server error.
func TestInfoRejectsNonJSON(t *testing.T) {
	c := Client{Exec: &fakeRunner{out: "ERROR: unable to open the repository\nP00 INFO: exiting"}, Stanza: "db"}
	if _, err := c.Info(context.Background()); err == nil {
		t.Fatal("output with no JSON document must be rejected")
	}
	// An object-shaped document (not only an array) is accepted too.
	c2 := Client{Exec: &fakeRunner{out: "noise\n{\"a\":1}"}, Stanza: "db"}
	if _, err := c2.Info(context.Background()); err != nil {
		t.Errorf("a JSON object payload must be accepted: %v", err)
	}
}

func TestInfoSurfacesExecError(t *testing.T) {
	c := Client{Exec: &fakeRunner{err: errors.New("exit status 1")}, Stanza: "db"}
	if _, err := c.Info(context.Background()); err == nil {
		t.Fatal("a failed pgbackrest must surface an error")
	}
}

func writeStatus(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pgbackrest-restore.status")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLastRestoreParses(t *testing.T) {
	p := writeStatus(t, strings.Join([]string{
		"startedAt=2026-08-01T10:00:00Z",
		"finishedAt=2026-08-01T10:12:31Z",
		"stanza=db",
		"targetType=time",
		"target=2026-08-01 09:55:00+00",
		"backupSet=20260801-090002F",
		"exitCode=0",
		"clusterState=in archive recovery",
		"checkpoint=16/B3000028",
		"requestedBy=dba-break-glass",
		"unknownFutureKey=ignored",
		"",
	}, "\n"))
	c := Client{StatusPath: p}
	r, err := c.LastRestore()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.Present || !r.Succeeded() {
		t.Fatalf("want a present, successful record: %+v", r)
	}
	if r.TargetType != "time" || r.BackupSet != "20260801-090002F" || r.RequestedBy != "dba-break-glass" {
		t.Errorf("bad parse: %+v", r)
	}
	if r.ClusterState != "in archive recovery" {
		t.Errorf("clusterState should keep its spaces: %q", r.ClusterState)
	}
}

// The common case: nothing has ever restored onto this volume. That is not an error.
func TestLastRestoreMissingFile(t *testing.T) {
	c := Client{StatusPath: filepath.Join(t.TempDir(), "absent")}
	r, err := c.LastRestore()
	if err != nil {
		t.Fatalf("a missing status file must not be an error: %v", err)
	}
	if r.Present || r.Succeeded() {
		t.Errorf("want an absent record: %+v", r)
	}
}

// A restore that died before writing its outcome must not read as a clean one.
func TestLastRestoreWithoutExitCodeIsNotSuccess(t *testing.T) {
	c := Client{StatusPath: writeStatus(t, "startedAt=2026-08-01T10:00:00Z\nstanza=db\n")}
	r, err := c.LastRestore()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.Present {
		t.Fatal("the record exists")
	}
	if r.Succeeded() {
		t.Error("no exitCode must not mean success")
	}
}

func TestLastRestoreFailedExitCode(t *testing.T) {
	c := Client{StatusPath: writeStatus(t, "exitCode=1\nstanza=db\n")}
	r, _ := c.LastRestore()
	if r.Succeeded() {
		t.Error("exitCode=1 is a failure")
	}
	if r.ExitCode == nil || *r.ExitCode != 1 {
		t.Errorf("ExitCode should be reported as 1: %+v", r.ExitCode)
	}
}

func TestLastRestoreNoPathConfigured(t *testing.T) {
	r, err := Client{}.LastRestore()
	if err != nil || r.Present {
		t.Errorf("an unconfigured status path must be a quiet absent record: %+v %v", r, err)
	}
}

const sampleRestoreLog = `P00   INFO: restore command begin 2.55.1
P00   INFO: repo1: restore backup set 20260801-090002F
P01 DETAIL: restore file /var/lib/postgresql/data/pgdata/base/1/2657 (8KB, 0.4%) checksum e5f2
P02 DETAIL: restore file /var/lib/postgresql/data/pgdata/base/1/2658 (16KB, 12.5%) checksum a1b2
P01 DETAIL: restore file /var/lib/postgresql/data/pgdata/base/1/2659 (0B, 37.25%) checksum 0000
`

func TestParseProgressTakesLastPercent(t *testing.T) {
	p := ParseProgress(sampleRestoreLog)
	if !p.PercentOK {
		t.Fatal("percentage should be found in a detail-level restore log")
	}
	if p.Percent != 37.25 {
		t.Errorf("Percent = %v, want the cumulative figure from the last file line (37.25)", p.Percent)
	}
	if p.FilesSeen != 3 {
		t.Errorf("FilesSeen = %d, want 3", p.FilesSeen)
	}
}

// An info-level log carries no per-file lines at all. Progress must degrade to
// "unknown percentage" rather than reporting a wrong number or zero progress.
func TestParseProgressWithoutDetailLines(t *testing.T) {
	p := ParseProgress("P00   INFO: restore command begin 2.55.1\nP00   INFO: restore command end: completed successfully\n")
	if p.PercentOK {
		t.Errorf("no percentage should be claimed: %+v", p)
	}
	if p.FilesSeen != 0 {
		t.Errorf("FilesSeen = %d, want 0", p.FilesSeen)
	}
}

func TestParseProgressEmpty(t *testing.T) {
	if p := ParseProgress(""); p.PercentOK || p.FilesSeen != 0 {
		t.Errorf("empty log: %+v", p)
	}
}

// Integer percentages (older pgBackRest wording) must parse too.
func TestParseProgressIntegerPercent(t *testing.T) {
	p := ParseProgress("P01 DETAIL: restore file /x (8KB, 100%) checksum ab\n")
	if !p.PercentOK || p.Percent != 100 {
		t.Errorf("got %+v, want 100%%", p)
	}
}

// A failed attempt must not become this directory's provenance: restore.sh keeps the
// previous successful restore's descriptive fields and records what was attempted
// separately, because many failures copy nothing at all.
func TestLastRestoreFailedAttemptKeepsProvenance(t *testing.T) {
	c := Client{StatusPath: writeStatus(t, strings.Join([]string{
		"startedAt=2026-07-30T09:00:00Z",
		"stanza=db",
		"targetType=time",
		"target=2026-07-30 09:00:00+00",
		"backupSet=20260730-090000F",
		"exitCode=32",
		"attemptedTargetType=time",
		"attemptedTarget=2026-99-99 bad",
		"attemptedBackupSet=typo-set",
		"",
	}, "\n"))}
	r, err := c.LastRestore()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Succeeded() {
		t.Error("exitCode=32 must not read as success")
	}
	// Provenance: where the data actually came from.
	if r.BackupSet != "20260730-090000F" || r.Target != "2026-07-30 09:00:00+00" {
		t.Errorf("the previous restore's provenance must survive: %+v", r)
	}
	// And the failed attempt is still visible, separately.
	if r.AttemptedBackupSet != "typo-set" || r.AttemptedTarget != "2026-99-99 bad" {
		t.Errorf("the attempt should be reported: %+v", r)
	}
}

// #288 review: a failed attempt must not erase the volume's restore identity. write_status
// carries restoredAt (the finishedAt of the last SUCCESSFUL restore) across failures, because
// the agent ranks restore provenance in a failover election -- keying that off
// finishedAt+exitCode meant one mistyped retry, which copies nothing at all, let a stale peer
// win the lease and promote pre-restore data.
func TestLastRestoreFailedAttemptKeepsRestoredAt(t *testing.T) {
	c := Client{StatusPath: writeStatus(t, strings.Join([]string{
		"startedAt=2026-07-30T09:00:00Z",
		"finishedAt=2026-07-30T11:00:00Z", // the FAILED attempt's own finish time
		"restoredAt=2026-07-30T09:05:00Z", // the real restore, preserved
		"stanza=db",
		"exitCode=32",
		"",
	}, "\n"))}
	r, err := c.LastRestore()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Succeeded() {
		t.Error("exitCode=32 must not read as success")
	}
	if r.RestoredAt != "2026-07-30T09:05:00Z" {
		t.Errorf("restoredAt must survive a failed attempt, got %q", r.RestoredAt)
	}
	if r.FinishedAt == r.RestoredAt {
		t.Error("finishedAt describes the attempt, restoredAt the data -- they are distinct")
	}
}
