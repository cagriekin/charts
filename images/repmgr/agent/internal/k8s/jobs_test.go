package k8s

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

var testNow = time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)

// restoreCronJob mirrors the security-relevant shape of the chart's rendered
// pgbackrest-restore CronJob: a dedicated SA with its token NOT mounted, explicit
// security contexts, the live data PVC, and a secretKeyRef for the S3 key.
func restoreCronJob() *batchv1.CronJob {
	no := false
	uid := int64(101)
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-pgbackrest-restore", Namespace: ns},
		Spec: batchv1.CronJobSpec{
			Suspend: func() *bool { b := true; return &b }(),
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"app.kubernetes.io/component": "pgbackrest-restore"},
					Annotations: map[string]string{"ignore-check.kube-linter.io/no-liveness-probe": "one-shot"},
				},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							ServiceAccountName:           "pg-repmgr",
							AutomountServiceAccountToken: &no,
							RestartPolicy:                corev1.RestartPolicyNever,
							SecurityContext:              &corev1.PodSecurityContext{RunAsUser: &uid},
							Containers: []corev1.Container{{
								Name:    "pgbackrest-restore",
								Image:   "cagriekin/repmgr:trixie-5.5.0-29",
								Command: []string{"/bin/bash", "/scripts/restore.sh"},
								Env: []corev1.EnvVar{
									{Name: "PGBACKREST_STANZA", Value: "db"},
									{Name: "TARGET_TYPE", Value: ""},
									{Name: "TARGET", Value: ""},
									{Name: "BACKUP_SET", Value: ""},
									{Name: "FORCE", Value: "false"},
									{Name: "PGBACKREST_REPO1_S3_KEY", ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{Name: "pg-pgbackrest"},
											Key:                  "accessKeyId",
										},
									}},
								},
								VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/postgresql/data"}},
							}},
							Volumes: []corev1.Volume{{
								Name: "data",
								VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: "data-pg-0",
								}},
							}},
						},
					},
				},
			},
		},
	}
}

func env(ctr corev1.Container, name string) (corev1.EnvVar, bool) {
	for _, e := range ctr.Env {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

// The whole safety argument for the API path is that it clones what the release
// declares. Everything security-relevant must survive the clone untouched.
func TestCloneCronJobToJobCopiesSpecVerbatim(t *testing.T) {
	cs := fake.NewSimpleClientset(restoreCronJob())
	c := NewWithClient(cs, ns)
	if _, err := c.CloneCronJobToJob(context.Background(), "pg-pgbackrest-restore", "pg-pgbackrest-restore-api",
		map[string]string{"TARGET_TYPE": "time"}, "dba", testNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	job, err := cs.BatchV1().Jobs(ns).Get(context.Background(), "pg-pgbackrest-restore-api", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("job not created: %v", err)
	}
	ps := job.Spec.Template.Spec
	if ps.ServiceAccountName != "pg-repmgr" {
		t.Errorf("serviceAccountName = %q, want the chart's", ps.ServiceAccountName)
	}
	if ps.AutomountServiceAccountToken == nil || *ps.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken must stay false: the restore Job needs no API access")
	}
	if ps.SecurityContext == nil || ps.SecurityContext.RunAsUser == nil || *ps.SecurityContext.RunAsUser != 101 {
		t.Error("pod securityContext must be carried over verbatim")
	}
	if ps.Containers[0].Image != "cagriekin/repmgr:trixie-5.5.0-29" {
		t.Errorf("image = %q, want the chart's", ps.Containers[0].Image)
	}
	if len(ps.Volumes) != 1 || ps.Volumes[0].PersistentVolumeClaim.ClaimName != "data-pg-0" {
		t.Errorf("the target PVC must come from the rendered spec, never from the request: %+v", ps.Volumes)
	}
	if len(job.OwnerReferences) != 0 {
		t.Errorf("no ownerReference, matching kubectl create job --from: %+v", job.OwnerReferences)
	}
	if job.Annotations[instantiateAnnotation] != "manual" {
		t.Error("the instantiate annotation makes the API path indistinguishable from the kubectl path")
	}
	if job.Annotations[RequestedByAnnotation] != "dba" || job.Annotations[RequestedAtAnnotation] != "2026-08-01T10:00:00Z" {
		t.Errorf("provenance annotations missing: %v", job.Annotations)
	}
	if job.Labels["app.kubernetes.io/component"] != "pgbackrest-restore" {
		t.Errorf("jobTemplate labels should carry over: %v", job.Labels)
	}
}

func TestCloneAppliesEnvOverridesInPlace(t *testing.T) {
	cs := fake.NewSimpleClientset(restoreCronJob())
	c := NewWithClient(cs, ns)
	_, err := c.CloneCronJobToJob(context.Background(), "pg-pgbackrest-restore", "j",
		map[string]string{
			"TARGET_TYPE":                  "time",
			"TARGET":                       "2026-08-01 09:55:00+00",
			"BACKUP_SET":                   "20260801-090002F",
			"PGBACKREST_LOG_LEVEL_CONSOLE": "detail",
		}, "dba", testNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	job, _ := cs.BatchV1().Jobs(ns).Get(context.Background(), "j", metav1.GetOptions{})
	ctr := job.Spec.Template.Spec.Containers[0]
	if e, _ := env(ctr, "TARGET_TYPE"); e.Value != "time" {
		t.Errorf("TARGET_TYPE = %q, want time", e.Value)
	}
	if e, _ := env(ctr, "TARGET"); e.Value != "2026-08-01 09:55:00+00" {
		t.Errorf("TARGET = %q", e.Value)
	}
	// An override with no existing entry must be appended, not dropped.
	if e, ok := env(ctr, "PGBACKREST_LOG_LEVEL_CONSOLE"); !ok || e.Value != "detail" {
		t.Errorf("a new env var should be appended: %+v", ctr.Env)
	}
	// Untouched entries must keep their values.
	if e, _ := env(ctr, "PGBACKREST_STANZA"); e.Value != "db" {
		t.Errorf("PGBACKREST_STANZA = %q, want db", e.Value)
	}
	if e, _ := env(ctr, "FORCE"); e.Value != "false" {
		t.Errorf("FORCE must keep the rendered value when not overridden, got %q", e.Value)
	}
}

// Replacing a secretKeyRef with a literal would both break the restore and turn a
// secret reference into a request-controlled value.
func TestCloneRefusesToOverrideValueFrom(t *testing.T) {
	cs := fake.NewSimpleClientset(restoreCronJob())
	c := NewWithClient(cs, ns)
	_, err := c.CloneCronJobToJob(context.Background(), "pg-pgbackrest-restore", "j",
		map[string]string{"PGBACKREST_REPO1_S3_KEY": "stolen"}, "dba", testNow)
	if err == nil {
		t.Fatal("overriding a valueFrom env must be refused")
	}
	if !strings.Contains(err.Error(), "valueFrom") {
		t.Errorf("error should name the reason: %v", err)
	}
	if _, gerr := cs.BatchV1().Jobs(ns).Get(context.Background(), "j", metav1.GetOptions{}); gerr == nil {
		t.Error("no Job should be created when the override is refused")
	}
}

// A sidecar would make "the restore container" ambiguous; refusing beats guessing.
func TestCloneRefusesMultipleContainers(t *testing.T) {
	cj := restoreCronJob()
	cj.Spec.JobTemplate.Spec.Template.Spec.Containers = append(cj.Spec.JobTemplate.Spec.Template.Spec.Containers,
		corev1.Container{Name: "sidecar", Image: "busybox"})
	c := NewWithClient(fake.NewSimpleClientset(cj), ns)
	_, err := c.CloneCronJobToJob(context.Background(), "pg-pgbackrest-restore", "j", nil, "dba", testNow)
	if err == nil || !strings.Contains(err.Error(), "exactly 1") {
		t.Errorf("a multi-container template must be refused: %v", err)
	}
}

func TestCloneMissingCronJob(t *testing.T) {
	c := NewWithClient(fake.NewSimpleClientset(), ns)
	_, err := c.CloneCronJobToJob(context.Background(), "pg-pgbackrest-restore", "j", nil, "dba", testNow)
	if err == nil || !strings.Contains(err.Error(), "pgbackrest.restore.enabled") {
		t.Errorf("the error should point at the values that render the CronJob: %v", err)
	}
}

// Jobs are immutable and the existing one is the record of the previous restore, so
// a second create must conflict rather than clobber.
func TestCloneRefusesExistingJob(t *testing.T) {
	cs := fake.NewSimpleClientset(restoreCronJob(),
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: ns}})
	c := NewWithClient(cs, ns)
	_, err := c.CloneCronJobToJob(context.Background(), "pg-pgbackrest-restore", "j", nil, "dba", testNow)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("want an already-exists error: %v", err)
	}
}

func TestGetJobAbsentIsNotAnError(t *testing.T) {
	c := NewWithClient(fake.NewSimpleClientset(), ns)
	v, err := c.GetJob(context.Background(), "j")
	if err != nil {
		t.Fatalf("a missing Job must not be an error: %v", err)
	}
	if v.Present {
		t.Error("want Present=false")
	}
}

func TestGetJobReportsStatus(t *testing.T) {
	start := metav1.NewTime(testNow)
	cs := fake.NewSimpleClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "j", Namespace: ns,
			CreationTimestamp: start,
			Annotations:       map[string]string{RequestedByAnnotation: "dba", RequestedAtAnnotation: "2026-08-01T10:00:00Z"},
		},
		Status: batchv1.JobStatus{Active: 1, StartTime: &start},
	})
	v, err := NewWithClient(cs, ns).GetJob(context.Background(), "j")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Present || v.Active != 1 || v.StartTime == nil || v.CompletionTime != nil {
		t.Errorf("bad view: %+v", v)
	}
	if v.RequestedBy != "dba" {
		t.Errorf("RequestedBy = %q", v.RequestedBy)
	}
}

func TestDeleteJobIdempotent(t *testing.T) {
	cs := fake.NewSimpleClientset(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: ns}})
	c := NewWithClient(cs, ns)
	if err := c.DeleteJob(context.Background(), "j"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := c.DeleteJob(context.Background(), "j"); err != nil {
		t.Errorf("deleting an absent Job must be a no-op: %v", err)
	}
}

func jobPod(name, label string, phase corev1.PodPhase, created time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels:            map[string]string{label: "j"},
			CreationTimestamp: metav1.NewTime(created),
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func TestJobPodFoundByCurrentLabel(t *testing.T) {
	cs := fake.NewSimpleClientset(jobPod("j-abc", "batch.kubernetes.io/job-name", corev1.PodRunning, testNow))
	v, err := NewWithClient(cs, ns).JobPod(context.Background(), "j")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Present || v.Name != "j-abc" || v.Phase != "Running" {
		t.Errorf("bad view: %+v", v)
	}
}

// Older clusters set only the bare job-name label.
func TestJobPodFallsBackToLegacyLabel(t *testing.T) {
	cs := fake.NewSimpleClientset(jobPod("j-xyz", "job-name", corev1.PodPending, testNow))
	v, err := NewWithClient(cs, ns).JobPod(context.Background(), "j")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Present || v.Name != "j-xyz" {
		t.Errorf("bad view: %+v", v)
	}
}

func TestJobPodAbsent(t *testing.T) {
	v, err := NewWithClient(fake.NewSimpleClientset(), ns).JobPod(context.Background(), "j")
	if err != nil || v.Present {
		t.Errorf("a Job with no pod yet is not an error: %+v %v", v, err)
	}
}

// A retried Job has a failed pod and a current one; the current attempt is what the
// operator is watching.
func TestJobPodPicksNewest(t *testing.T) {
	old := jobPod("j-old", "batch.kubernetes.io/job-name", corev1.PodFailed, testNow)
	recent := jobPod("j-new", "batch.kubernetes.io/job-name", corev1.PodRunning, testNow.Add(time.Minute))
	v, err := NewWithClient(fake.NewSimpleClientset(old, recent), ns).JobPod(context.Background(), "j")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Name != "j-new" {
		t.Errorf("Name = %q, want the newest pod", v.Name)
	}
}

func TestJobPodSurfacesWaitingReason(t *testing.T) {
	p := jobPod("j-abc", "batch.kubernetes.io/job-name", corev1.PodPending, testNow)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "pgbackrest-restore",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "no pull secret"}},
	}}
	v, _ := NewWithClient(fake.NewSimpleClientset(p), ns).JobPod(context.Background(), "j")
	if v.WaitingReason != "ImagePullBackOff" || v.WaitingMessage != "no pull secret" {
		t.Errorf("bad view: %+v", v)
	}
	if v.ContainerStarted {
		t.Error("a waiting container has not started")
	}
}

// The volume-attach case: Pending with NO container statuses at all. There is no
// reason to report from the pod, which is why the caller explains it locally.
func TestJobPodPendingWithoutContainerStatuses(t *testing.T) {
	cs := fake.NewSimpleClientset(jobPod("j-abc", "batch.kubernetes.io/job-name", corev1.PodPending, testNow))
	v, _ := NewWithClient(cs, ns).JobPod(context.Background(), "j")
	if v.Phase != "Pending" || v.WaitingReason != "" || v.ContainerStarted {
		t.Errorf("bad view: %+v", v)
	}
}

// The requester must reach the CONTAINER, not just the Job object: the downward API
// can only read a pod's own annotations, and restore.sh records the requester on the
// data volume from that env.
func TestCloneStampsRequesterOnThePodTemplate(t *testing.T) {
	cs := fake.NewSimpleClientset(restoreCronJob())
	c := NewWithClient(cs, ns)
	if _, err := c.CloneCronJobToJob(context.Background(), "pg-pgbackrest-restore", "j", nil, "dba-break-glass", testNow); err != nil {
		t.Fatal(err)
	}
	job, _ := cs.BatchV1().Jobs(ns).Get(context.Background(), "j", metav1.GetOptions{})
	if got := job.Spec.Template.Annotations[RequestedByAnnotation]; got != "dba-break-glass" {
		t.Errorf("pod template annotation = %q, want the requester", got)
	}
	if got := job.Annotations[RequestedByAnnotation]; got != "dba-break-glass" {
		t.Errorf("job annotation = %q, want the requester", got)
	}
}
