package control

import (
	"errors"
	"strings"
	"testing"
)

// pausedHarness is the state a restore is allowed from: the cluster paused, no
// restore Job present.
func pausedHarness(t *testing.T) *harness {
	h := newHarness(t, nil)
	h.cl.marker.Paused = true
	return h
}

const goodRestore = `{"node":"pg-0","confirm":"pg","force":true,"targetType":"time","target":"2026-08-01 09:55:00+00"}`

// The ordering guarantee: Postgres must be STOPPED before the Job exists, or the Job
// races a live postmaster and pgBackRest refuses (correctly) to restore.
func TestRestoreStopsPostgresBeforeCreatingTheJob(t *testing.T) {
	h := pausedHarness(t)
	rec := h.do("POST", "/v1/restore", goodRestore, "dba")
	wantCode(t, rec, 202)
	want := []string{"intent:stop", "createJob"}
	if len(h.order) != len(want) {
		t.Fatalf("side effects = %v, want %v", h.order, want)
	}
	for i := range want {
		if h.order[i] != want[i] {
			t.Fatalf("side effects = %v, want %v", h.order, want)
		}
	}
	if h.bk.created == nil || h.bk.created.TargetType != "time" {
		t.Errorf("the PITR target must reach the Job: %+v", h.bk.created)
	}
}

func TestRestoreResponseCarriesJobAndNextSteps(t *testing.T) {
	h := pausedHarness(t)
	rec := h.do("POST", "/v1/restore", goodRestore, "dba")
	wantCode(t, rec, 202)
	v := decode[RestoreView](t, rec)
	if v.JobName == "" || v.Phase != "pending" {
		t.Errorf("bad view: %+v", v)
	}
	if v.RequestedBy != "dba" {
		t.Errorf("RequestedBy = %q, want the client identity", v.RequestedBy)
	}
	// The API cannot scale the StatefulSet, so it must hand back the steps it does not
	// take -- otherwise the Job sits Pending and nobody knows why.
	joined := strings.Join(v.NextSteps, "\n")
	if !strings.Contains(joined, "--replicas=0") || !strings.Contains(joined, "--replicas=<original replicas>") {
		t.Errorf("nextSteps should include the scale-down and scale-up: %v", v.NextSteps)
	}
	// And the hint must name the reason the Job cannot start yet.
	if !strings.Contains(v.Hint, "Scale the StatefulSet to 0") {
		t.Errorf("hint = %q", v.Hint)
	}
}

// Pause is load-bearing, not ceremonial: an active loop would restart the postmaster
// the moment the restore stopped it.
func TestRestoreRefusedWhenNotPaused(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.do("POST", "/v1/restore", goodRestore, "dba")
	wantCode(t, rec, 409)
	if !strings.Contains(rec.Body.String(), "/v1/pause") {
		t.Errorf("the error should name the fix: %s", rec.Body.String())
	}
	if len(h.order) != 0 {
		t.Errorf("nothing should have happened: %v", h.order)
	}
}

func TestRestoreRequiresConfirmAndForce(t *testing.T) {
	cases := []struct{ name, body string }{
		{"no confirm", `{"node":"pg-0","force":true}`},
		{"wrong confirm", `{"node":"pg-0","confirm":"other-cluster","force":true}`},
		{"no force", `{"node":"pg-0","confirm":"pg"}`},
		{"force false", `{"node":"pg-0","confirm":"pg","force":false}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := pausedHarness(t)
			wantCode(t, h.do("POST", "/v1/restore", tc.body, "dba"), 400)
			if len(h.order) != 0 {
				t.Errorf("nothing should have happened: %v", h.order)
			}
		})
	}
}

func TestRestoreRequiresAddressedNode(t *testing.T) {
	h := pausedHarness(t)
	wantCode(t, h.do("POST", "/v1/restore", `{"confirm":"pg","force":true}`, "dba"), 400)
	rec := h.do("POST", "/v1/restore", `{"node":"pg-1","confirm":"pg","force":true}`, "dba")
	wantCode(t, rec, 409)
	if len(h.order) != 0 {
		t.Errorf("nothing should have happened: %v", h.order)
	}
}

// Which volume gets overwritten is a values decision baked into the rendered Job.
// podOrdinal in the body may only confirm it.
func TestRestoreOrdinalIsConfirmOnly(t *testing.T) {
	h := pausedHarness(t)
	rec := h.do("POST", "/v1/restore", `{"node":"pg-0","confirm":"pg","force":true,"podOrdinal":1}`, "dba")
	wantCode(t, rec, 409)
	if !strings.Contains(rec.Body.String(), "pgbackrest.restore.podOrdinal") {
		t.Errorf("the error should point at where the target is decided: %s", rec.Body.String())
	}
	// The matching ordinal is accepted.
	h2 := pausedHarness(t)
	wantCode(t, h2.do("POST", "/v1/restore", `{"node":"pg-0","confirm":"pg","force":true,"podOrdinal":0}`, "dba"), 202)
}

// Talking to a pod that does not own the target volume cannot work: only that pod can
// stop the postmaster holding it.
func TestRestoreRefusesWrongTargetPod(t *testing.T) {
	h := newHarness(t, func(o *Options, _ *harness) {
		o.PodName = "pg-1"
		o.RestoreTargetPod = "pg-0"
		o.RestorePodOrdinal = 0
	})
	h.cl.marker.Paused = true
	rec := h.do("POST", "/v1/restore", `{"node":"pg-1","confirm":"pg","force":true}`, "dba")
	wantCode(t, rec, 409)
	if !strings.Contains(rec.Body.String(), "data-pg-0") {
		t.Errorf("the error should name the target volume: %s", rec.Body.String())
	}
}

func TestRestoreRequiresTargetTypeAndTargetTogether(t *testing.T) {
	for _, body := range []string{
		`{"node":"pg-0","confirm":"pg","force":true,"targetType":"time"}`,
		`{"node":"pg-0","confirm":"pg","force":true,"target":"2026-08-01 09:55:00+00"}`,
	} {
		h := pausedHarness(t)
		wantCode(t, h.do("POST", "/v1/restore", body, "dba"), 400)
	}
	// Neither set is the disaster-recovery case: latest backup, replay all WAL.
	h := pausedHarness(t)
	wantCode(t, h.do("POST", "/v1/restore", `{"node":"pg-0","confirm":"pg","force":true}`, "dba"), 202)
}

// The previous Job's status is the record of the last restore, so it is never
// clobbered implicitly.
func TestRestoreRefusesExistingJobWithoutReplace(t *testing.T) {
	h := pausedHarness(t)
	h.bk.status = RestoreView{Phase: "failed", JobName: "pg-pgbackrest-restore-api"}
	rec := h.do("POST", "/v1/restore", goodRestore, "dba")
	wantCode(t, rec, 409)
	if !strings.Contains(rec.Body.String(), "replace") {
		t.Errorf("the error should name the escape hatch: %s", rec.Body.String())
	}
	if h.bk.deletes != 0 || h.bk.created != nil {
		t.Error("nothing should be deleted or created")
	}
}

func TestRestoreReplaceDeletesFirst(t *testing.T) {
	h := pausedHarness(t)
	h.bk.status = RestoreView{Phase: "failed", JobName: "pg-pgbackrest-restore-api"}
	body := `{"node":"pg-0","confirm":"pg","force":true,"replace":true}`
	wantCode(t, h.do("POST", "/v1/restore", body, "dba"), 202)
	want := []string{"deleteJob", "intent:stop", "createJob"}
	if strings.Join(h.order, ",") != strings.Join(want, ",") {
		t.Errorf("side effects = %v, want %v", h.order, want)
	}
}

// If the Job cannot be created after Postgres was stopped, the operator must be told
// the database is down and how to bring it back.
func TestRestoreCreateFailureSaysPostgresIsStopped(t *testing.T) {
	h := pausedHarness(t)
	h.bk.createErr = errors.New("jobs.batch is forbidden")
	rec := h.do("POST", "/v1/restore", goodRestore, "dba")
	wantCode(t, rec, 502)
	if !strings.Contains(rec.Body.String(), "STOPPED") || !strings.Contains(rec.Body.String(), "/v1/restart") {
		t.Errorf("the error must disclose the stopped postmaster and the recovery step: %s", rec.Body.String())
	}
}

// A stop that never completes must not leave the caller thinking a restore started.
func TestRestoreStopTimeoutCreatesNoJob(t *testing.T) {
	h := pausedHarness(t)
	h.nd.hang = true
	rec := h.do("POST", "/v1/restore", goodRestore, "dba")
	wantCode(t, rec, 504)
	if h.bk.created != nil {
		t.Error("no Job should be created when the stop timed out")
	}
	if !strings.Contains(rec.Body.String(), "no restore Job was created") {
		t.Errorf("the error should say so: %s", rec.Body.String())
	}
}

// --- status / backups / feature gate ---

func TestRestoreStatusNoneCarriesNextSteps(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.do("GET", "/v1/restore", "", "ops")
	wantCode(t, rec, 200)
	v := decode[RestoreView](t, rec)
	if v.Phase != "none" {
		t.Errorf("phase = %q", v.Phase)
	}
	if len(v.NextSteps) == 0 {
		t.Error("nextSteps should be offered before a restore exists too")
	}
}

// A Pending pod whose container IS starting has its own reason (image pull); the
// volume-attach hint must not be attached to it.
func TestRestoreStatusHintOnlyForUnstartedPod(t *testing.T) {
	h := newHarness(t, nil)
	h.bk.status = RestoreView{Phase: "pending", PodPhase: "Pending", WaitingReason: "ImagePullBackOff"}
	v := decode[RestoreView](t, h.do("GET", "/v1/restore", "", "ops"))
	if v.Hint != "" {
		t.Errorf("no scale-down hint when the container has a reason of its own: %q", v.Hint)
	}

	h2 := newHarness(t, nil)
	h2.bk.status = RestoreView{Phase: "pending", PodPhase: "Pending"}
	v2 := decode[RestoreView](t, h2.do("GET", "/v1/restore", "", "ops"))
	if !strings.Contains(v2.Hint, "data-pg-0") {
		t.Errorf("hint should name the volume that cannot attach: %q", v2.Hint)
	}
}

func TestBackupsPassesThroughPgbackrestJSON(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.do("GET", "/v1/backups", "", "ops")
	wantCode(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"name": "db"`) && !strings.Contains(rec.Body.String(), `"name":"db"`) {
		t.Errorf("pgbackrest output should be forwarded: %s", rec.Body.String())
	}
}

func TestBackupsErrorIs502(t *testing.T) {
	h := newHarness(t, nil)
	h.bk.infoErr = errors.New("unable to reach repository")
	wantCode(t, h.do("GET", "/v1/backups", "", "ops"), 502)
}

// pgbackrest disabled entirely: the routes exist but say why they cannot work.
func TestBackupRoutesWithoutPgbackrest(t *testing.T) {
	h := newHarness(t, func(o *Options, _ *harness) { o.Backups = nil })
	for _, path := range []string{"/v1/backups", "/v1/restore"} {
		rec := h.do("GET", path, "", "ops")
		wantCode(t, rec, 501)
		if !strings.Contains(rec.Body.String(), "pgbackrest.enabled") {
			t.Errorf("%s should name the values key: %s", path, rec.Body.String())
		}
	}
	rec := h.do("POST", "/v1/restore", goodRestore, "dba")
	wantCode(t, rec, 501)
}

// Restore triggering off, control API on: the answer must be "not configured", not
// "not authorized" -- the restore allowlist is empty by construction when disabled.
func TestRestoreDisabledReportsConfiguration(t *testing.T) {
	h := newHarness(t, func(o *Options, hh *harness) {
		hh.bk = &fakeBackups{status: RestoreView{Phase: "none"}, enabled: false, order: &hh.order}
		o.Backups = hh.bk
		o.RestoreAllowedCNs = nil
	})
	h.cl.marker.Paused = true
	rec := h.do("POST", "/v1/restore", goodRestore, "dba")
	wantCode(t, rec, 501)
	if !strings.Contains(rec.Body.String(), "control.restore.enabled") {
		t.Errorf("the error should name the values key: %s", rec.Body.String())
	}
}

func TestRestoreDeleteIsIdempotent(t *testing.T) {
	h := newHarness(t, nil)
	wantCode(t, h.do("DELETE", "/v1/restore", "", "dba"), 200)
	wantCode(t, h.do("DELETE", "/v1/restore", "", "dba"), 200)
	if h.bk.deletes != 2 {
		t.Errorf("deletes = %d", h.bk.deletes)
	}
}

// --- authorization ---

// Restore is a separate verb: a client cleared for pause/switchover must not be able
// to overwrite the data directory.
func TestRestoreVerbIsSeparateFromControl(t *testing.T) {
	h := pausedHarness(t)
	rec := h.do("POST", "/v1/restore", goodRestore, "ops-admin") // not on the restore list
	wantCode(t, rec, 403)
	if len(h.order) != 0 {
		t.Errorf("nothing should have happened: %v", h.order)
	}
	// The same client can still pause.
	wantCode(t, h.do("POST", "/v1/pause", "", "ops-admin"), 200)
}

// An empty restore allowlist denies everyone; it must never read as "allow all".
func TestEmptyRestoreAllowlistDeniesEveryone(t *testing.T) {
	h := newHarness(t, func(o *Options, _ *harness) { o.RestoreAllowedCNs = nil })
	h.cl.marker.Paused = true
	wantCode(t, h.do("POST", "/v1/restore", goodRestore, "dba"), 403)
	wantCode(t, h.do("DELETE", "/v1/restore", "", "dba"), 403)
}

// An empty general allowlist DOES mean "any certificate from the CA" -- that set is
// already closed by who signs certificates.
func TestEmptyControlAllowlistAdmitsAnyValidIdentity(t *testing.T) {
	h := newHarness(t, func(o *Options, _ *harness) { o.AllowedCNs = nil })
	wantCode(t, h.do("GET", "/v1/status", "", "anyone"), 200)
}

func TestControlAllowlistRestrictsEveryVerb(t *testing.T) {
	h := newHarness(t, func(o *Options, _ *harness) {
		o.AllowedCNs = []string{"ops-admin"}
		o.RestoreAllowedCNs = []string{"dba"}
	})
	h.cl.marker.Paused = true
	// Reads are gated too: topology and lease state are not public.
	wantCode(t, h.do("GET", "/v1/status", "", "stranger"), 403)
	wantCode(t, h.do("POST", "/v1/pause", "", "stranger"), 403)
	wantCode(t, h.do("GET", "/v1/status", "", "ops-admin"), 200)
	// dba is on the restore list but NOT on the control list, so the control list wins.
	wantCode(t, h.do("POST", "/v1/restore", goodRestore, "dba"), 403)
}

// CN matching is case-insensitive and tolerates surrounding whitespace in values.
func TestAllowlistMatchingIsCaseInsensitive(t *testing.T) {
	h := newHarness(t, func(o *Options, _ *harness) { o.AllowedCNs = []string{" Ops-Admin "} })
	wantCode(t, h.do("GET", "/v1/status", "", "ops-admin"), 200)
}

// An identity with no CN cannot match any list entry, including an empty-string one.
func TestEmptyCNNeverMatches(t *testing.T) {
	h := newHarness(t, func(o *Options, _ *harness) { o.AllowedCNs = []string{""} })
	wantCode(t, h.do("GET", "/v1/status", "", ""), 403)
}
