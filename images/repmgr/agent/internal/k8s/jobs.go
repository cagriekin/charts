package k8s

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// instantiateAnnotation is the annotation `kubectl create job --from=cronjob/...`
// stamps on the Job it clones. The control API sets it too, so an API-triggered
// restore is indistinguishable from the documented kubectl runbook -- the two paths
// are meant to be the same operation, not two similar ones (#276).
const instantiateAnnotation = "cronjob.kubernetes.io/instantiate"

// RequestedByAnnotation records the control-API client identity that triggered a
// restore, so the Job itself carries its provenance for `kubectl describe`.
const RequestedByAnnotation = "pg-ha/requested-by"

// RequestedAtAnnotation records when the control API created the Job.
const RequestedAtAnnotation = "pg-ha/requested-at"

// JobView is the subset of a Job's status the control API reports. Absent times
// stay nil rather than zero so "not started" and "started at the epoch" cannot be
// confused by a client.
type JobView struct {
	Present        bool
	Name           string
	CreatedAt      *time.Time
	StartTime      *time.Time
	CompletionTime *time.Time
	Active         int32
	Succeeded      int32
	Failed         int32
	// RequestedBy/At come from the annotations above ("" when the Job was created by
	// kubectl rather than the API).
	RequestedBy string
	RequestedAt string
	// TargetType/Target/BackupSet are the recovery point the Job's container actually
	// carries, whichever supplied it: the request's override or the release's rendered
	// value. Read back from the object rather than echoed from the request, so what is
	// reported is what will really run.
	TargetType string
	Target     string
	BackupSet  string
}

// PodView is the restore pod's schedulability/liveness, which is what actually
// explains a Job that appears stuck. WaitingReason carries the container-level
// reason (ImagePullBackOff, CreateContainerConfigError); a Pending pod with NO
// container statuses at all is the volume-attach case the caller explains locally.
type PodView struct {
	Present        bool
	Name           string
	Phase          string
	WaitingReason  string
	WaitingMessage string
	// ContainerStarted is false while the pod is Pending for any reason -- including
	// a PVC that cannot attach because a StatefulSet pod still has it mounted.
	ContainerStarted bool
}

func ts(t *metav1.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	v := t.Time
	return &v
}

// CloneCronJobToJob creates jobName from cronJobName's jobTemplate, applying
// envOverrides to the (single) container. This is deliberately a CLONE, not a
// construction: everything security-relevant -- image, serviceAccountName,
// automountServiceAccountToken, security contexts, volumes, secret references --
// comes from the chart-rendered CronJob verbatim, and the only mutations are the
// name, two provenance annotations, and the named environment values. Nothing in
// this function can synthesise a pod spec the release did not already declare.
//
// An existing Job of the same name is a conflict, not an overwrite: Jobs are
// immutable, and silently clobbering a previous restore's record would destroy the
// evidence of what just ran. Callers delete it explicitly first.
func (c *Client) CloneCronJobToJob(ctx context.Context, cronJobName, jobName string, envOverrides map[string]string, requestedBy string, now time.Time) (JobView, error) {
	cj, err := c.cs.BatchV1().CronJobs(c.namespace).Get(ctx, cronJobName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return JobView{}, fmt.Errorf("restore CronJob %s not found: the release must render it (pgbackrest.restore.enabled with mode=cronjob)", cronJobName)
	}
	if err != nil {
		return JobView{}, fmt.Errorf("get restore CronJob %s: %w", cronJobName, err)
	}

	spec := *cj.Spec.JobTemplate.Spec.DeepCopy()
	if n := len(spec.Template.Spec.Containers); n != 1 {
		// The override targets "the restore container". If the chart ever grows a
		// sidecar here, refuse rather than guess which container's environment to
		// rewrite -- a restore aimed at the wrong target is worse than a failed call.
		return JobView{}, fmt.Errorf("restore CronJob %s renders %d containers; expected exactly 1, so the agent will not guess which to configure", cronJobName, n)
	}
	if err := applyEnvOverrides(&spec.Template.Spec.Containers[0], envOverrides); err != nil {
		return JobView{}, err
	}

	labels := map[string]string{}
	for k, v := range cj.Spec.JobTemplate.Labels {
		labels[k] = v
	}
	annotations := map[string]string{}
	for k, v := range cj.Spec.JobTemplate.Annotations {
		annotations[k] = v
	}
	annotations[instantiateAnnotation] = "manual"
	annotations[RequestedByAnnotation] = requestedBy
	annotations[RequestedAtAnnotation] = now.UTC().Format(time.RFC3339)

	// Stamp the requester on the POD template too. The downward API can only read a
	// pod's own annotations, and the chart's restore.sh reads RESTORE_REQUESTED_BY from
	// there to record who asked for the restore on the data volume -- provenance that
	// outlives the Job.
	if spec.Template.Annotations == nil {
		spec.Template.Annotations = map[string]string{}
	}
	spec.Template.Annotations[RequestedByAnnotation] = requestedBy

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        jobName,
			Namespace:   c.namespace,
			Labels:      labels,
			Annotations: annotations,
			// No ownerReference, matching `kubectl create job --from`: owning it by the
			// CronJob would make the CronJob controller adopt it and count it against
			// concurrencyPolicy, and this Job must outlive nothing but itself.
		},
		Spec: spec,
	}
	created, err := c.cs.BatchV1().Jobs(c.namespace).Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return JobView{}, fmt.Errorf("restore Job %s already exists: delete it first (Jobs are immutable, and its status is the record of the previous restore)", jobName)
	}
	if err != nil {
		return JobView{}, fmt.Errorf("create restore Job %s: %w", jobName, err)
	}
	return jobView(created), nil
}

// applyEnvOverrides sets name=value on the container, replacing an existing literal
// entry in place (order preserved) and appending otherwise.
//
// It REFUSES to overwrite an entry backed by valueFrom. Those are the secretKeyRefs
// the chart renders for the S3 key and the repository cipher passphrase; replacing
// one with a literal from an HTTP request would both break the restore and turn a
// secret reference into a request-controlled value. Fail loudly instead.
func applyEnvOverrides(ctr *corev1.Container, overrides map[string]string) error {
	// Deterministic order so the resulting Job is byte-stable for a given request.
	for _, k := range sortedKeys(overrides) {
		v := overrides[k]
		replaced := false
		for i := range ctr.Env {
			if ctr.Env[i].Name != k {
				continue
			}
			if ctr.Env[i].ValueFrom != nil {
				return fmt.Errorf("refusing to override env %s: it is sourced from valueFrom (a secret or field reference), not a literal", k)
			}
			ctr.Env[i].Value = v
			replaced = true
			break
		}
		if !replaced {
			ctr.Env = append(ctr.Env, corev1.EnvVar{Name: k, Value: v})
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Small maps; insertion sort keeps this dependency-free.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func jobView(j *batchv1.Job) JobView {
	v := JobView{
		Present:        true,
		Name:           j.Name,
		CreatedAt:      ts(&j.CreationTimestamp),
		StartTime:      ts(j.Status.StartTime),
		CompletionTime: ts(j.Status.CompletionTime),
		Active:         j.Status.Active,
		Succeeded:      j.Status.Succeeded,
		Failed:         j.Status.Failed,
		RequestedBy:    j.Annotations[RequestedByAnnotation],
		RequestedAt:    j.Annotations[RequestedAtAnnotation],
	}
	// The recovery point as the object really carries it (see the field comments).
	if cs := j.Spec.Template.Spec.Containers; len(cs) == 1 {
		for _, e := range cs[0].Env {
			switch e.Name {
			case "TARGET_TYPE":
				v.TargetType = e.Value
			case "TARGET":
				v.Target = e.Value
			case "BACKUP_SET":
				v.BackupSet = e.Value
			}
		}
	}
	return v
}

// WaitJobGone blocks until name no longer exists, or timeout elapses. Needed because a
// Foreground delete returns while the object is still present behind its finalizer, and
// the agent re-creates the same deterministic name.
func (c *Client) WaitJobGone(ctx context.Context, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := c.cs.BatchV1().Jobs(c.namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("wait for Job %s to be deleted: %w", name, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Job %s still exists %s after deletion was requested; its pods may still be terminating", name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// GetJob reads one Job by name. A missing Job is Present=false, not an error: "no
// restore has been triggered" is a normal state for this endpoint to report.
func (c *Client) GetJob(ctx context.Context, name string) (JobView, error) {
	j, err := c.cs.BatchV1().Jobs(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return JobView{}, nil
	}
	if err != nil {
		return JobView{}, fmt.Errorf("get Job %s: %w", name, err)
	}
	return jobView(j), nil
}

// DeleteJob removes a Job and its pods (Foreground propagation, so a follow-up
// create cannot race the old pod still holding the data volume). A missing Job is a
// no-op, keeping the endpoint idempotent.
func (c *Client) DeleteJob(ctx context.Context, name string) error {
	policy := metav1.DeletePropagationForeground
	err := c.cs.BatchV1().Jobs(c.namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete Job %s: %w", name, err)
	}
	return nil
}

// jobPodSelectors are the labels the Job controller puts on its pods. The
// batch.kubernetes.io/ form is current; the bare form is still set for compatibility
// and is all that older clusters have. Tried in order.
var jobPodSelectors = []string{"batch.kubernetes.io/job-name=%s", "job-name=%s"}

// JobPod finds the pod of a Job by label. It uses the agent's existing (unscoped)
// pods list grant -- the Job's pod name is generated, so it cannot be fetched by a
// resourceName-scoped get. A Job with no pod yet is Present=false, not an error.
func (c *Client) JobPod(ctx context.Context, jobName string) (PodView, error) {
	for _, sel := range jobPodSelectors {
		list, err := c.cs.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf(sel, jobName),
			Limit:         10,
		})
		if err != nil {
			return PodView{}, fmt.Errorf("list pods for Job %s: %w", jobName, err)
		}
		if len(list.Items) == 0 {
			continue
		}
		// Newest first: a backoffLimit>0 Job can have a failed pod and a running one,
		// and the current attempt is the interesting one.
		newest := &list.Items[0]
		for i := range list.Items {
			if list.Items[i].CreationTimestamp.After(newest.CreationTimestamp.Time) {
				newest = &list.Items[i]
			}
		}
		return podView(newest), nil
	}
	return PodView{}, nil
}

func podView(p *corev1.Pod) PodView {
	v := PodView{Present: true, Name: p.Name, Phase: string(p.Status.Phase)}
	for i := range p.Status.ContainerStatuses {
		cs := &p.Status.ContainerStatuses[i]
		if cs.State.Waiting != nil && v.WaitingReason == "" {
			v.WaitingReason = cs.State.Waiting.Reason
			v.WaitingMessage = cs.State.Waiting.Message
		}
		if cs.State.Running != nil || cs.State.Terminated != nil {
			v.ContainerStarted = true
		}
	}
	return v
}

// PodLogTail returns the last tailLines of a pod's log, additionally bounded by
// maxBytes. TailLines (not LimitBytes alone) is what makes this the TAIL: LimitBytes
// truncates from the START of the stream, which on a long restore would return the
// oldest output and a progress figure from minutes ago.
//
// This is the only call in the agent that needs `get pods/log`, which cannot be
// resourceName-scoped (the Job's pod name is generated) and therefore grants log read
// across the namespace -- so it is reached only when the operator has explicitly
// opted in.
func (c *Client) PodLogTail(ctx context.Context, podName, container string, tailLines, maxBytes int64) (string, error) {
	opts := &corev1.PodLogOptions{Container: container, LimitBytes: &maxBytes}
	if tailLines > 0 {
		opts.TailLines = &tailLines
	}
	rc, err := c.cs.CoreV1().Pods(c.namespace).GetLogs(podName, opts).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("stream logs for pod %s: %w", podName, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, maxBytes))
	if err != nil && !strings.Contains(err.Error(), "unexpected EOF") {
		return "", fmt.Errorf("read logs for pod %s: %w", podName, err)
	}
	return string(b), nil
}
