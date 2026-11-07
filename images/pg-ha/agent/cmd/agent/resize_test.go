package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cagriekin/pg-ha-agent/internal/config"
	"github.com/cagriekin/pg-ha-agent/internal/k8s"
	"github.com/cagriekin/pg-ha-agent/internal/observe"
	"github.com/cagriekin/pg-ha-agent/internal/reconcile"
)

func TestDesiredSize(t *testing.T) {
	primary := reconcile.LocalState{HasData: true, Running: true}
	standby := reconcile.LocalState{HasData: true, Running: true, InRecovery: true}
	stopped := reconcile.LocalState{HasData: true}
	boom := errors.New("boom")
	for _, tc := range []struct {
		name   string
		action reconcile.Action
		local  reconcile.LocalState
		lease  bool
		actErr error
		want   podSize
	}{
		// Raise on the Promote tick itself: the server is still in recovery, the lease is held.
		{"promote", reconcile.Promote, standby, true, nil, sizePrimary},
		{"promote that failed", reconcile.Promote, standby, true, boom, sizeKeep},
		{"stay primary", reconcile.StayPrimary, primary, true, nil, sizePrimary},
		// A primary pod recreated small by a rollout, or a paused cluster (decision NoOp):
		// read-write with the lease is the primary whatever the tick decided.
		{"read-write lease holder, any decision", reconcile.NoOp, primary, true, nil, sizePrimary},
		{"stay primary whose act failed but still serves", reconcile.StayPrimary, primary, true, boom, sizePrimary},
		{"follower", reconcile.Follow, standby, false, nil, sizeStandby},
		{"standby on a paused cluster", reconcile.NoOp, standby, false, nil, sizeStandby},
		// A demoted primary is lowered only once its server is back as a standby.
		{"read-write without the lease (being fenced)", reconcile.DemoteFence, primary, false, nil, sizeKeep},
		{"demoted, restarted as standby", reconcile.Follow, standby, false, nil, sizeStandby},
		{"stopped", reconcile.StartLocal, stopped, false, nil, sizeKeep},
		{"stopped lease holder", reconcile.StartLocal, stopped, true, nil, sizeKeep},
		{"cloning", reconcile.BootstrapClone, reconcile.LocalState{}, false, nil, sizeKeep},
		{"lease holder in recovery that releases", reconcile.ReleaseLease, standby, true, nil, sizeKeep},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := desiredSize(reconcile.Decision{Action: tc.action}, reconcile.Observation{Local: tc.local, HoldLease: tc.lease}, tc.actErr)
			if got != tc.want {
				t.Errorf("desiredSize = %s, want %s", got, tc.want)
			}
		})
	}
}

func rq(reqCPU, reqMem, limCPU, limMem string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(reqCPU), corev1.ResourceMemory: resource.MustParse(reqMem)},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(limCPU), corev1.ResourceMemory: resource.MustParse(limMem)},
	}
}

type resizeHarness struct {
	a     *agent
	cs    *fake.Clientset
	logs  *bytes.Buffer
	clock time.Time
	big   corev1.ResourceRequirements
	small corev1.ResourceRequirements
}

func newResizeHarness(t *testing.T) *resizeHarness {
	t.Helper()
	h := &resizeHarness{
		big:   rq("200m", "1Gi", "2", "4Gi"),
		small: rq("50m", "256Mi", "500m", "512Mi"),
		logs:  &bytes.Buffer{},
		clock: time.Unix(1_700_000_000, 0),
	}
	small := h.small
	h.cs = fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-0", Namespace: "ns"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "postgresql", Resources: small}}},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "postgresql", Resources: &small}}},
	})
	h.a = &agent{
		cfg: &config.Config{
			PodName: "pg-0", LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second,
			Resize: map[string]config.ResizePair{"postgresql": {Primary: h.big, Standby: h.small}},
		},
		log:  slog.New(slog.NewTextHandler(h.logs, nil)),
		kube: k8s.NewWithClient(h.cs, "ns"),
		metr: observe.New(),
	}
	h.a.resize.now = func() time.Time { return h.clock }
	return h
}

func (h *resizeHarness) tickAsPrimary() {
	h.a.resizeTick(context.Background(), reconcile.Decision{Action: reconcile.StayPrimary},
		reconcile.Observation{Local: reconcile.LocalState{HasData: true, Running: true}, HoldLease: true}, nil)
}

func (h *resizeHarness) count(verb string) int {
	n := 0
	for _, a := range h.cs.Actions() {
		if a.GetVerb() == verb {
			n++
		}
	}
	return n
}

// The kubelet's side: report the spec'd size as actuated, or a resize condition.
func (h *resizeHarness) kubelet(t *testing.T, actuate bool, cond *corev1.PodCondition) {
	t.Helper()
	ctx := context.Background()
	p, err := h.cs.CoreV1().Pods("ns").Get(ctx, "pg-0", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if actuate {
		r := p.Spec.Containers[0].Resources
		p.Status.ContainerStatuses[0].Resources = &r
	}
	p.Status.Conditions = nil
	if cond != nil {
		p.Status.Conditions = []corev1.PodCondition{*cond}
	}
	if _, err := h.cs.CoreV1().Pods("ns").UpdateStatus(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func metricsText(m *observe.Metrics) string {
	w := httptest.NewRecorder()
	m.Handler(time.Minute).ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	return w.Body.String()
}

// #386: one patch to raise a small primary, no re-patch while the kubelet works on it, and
// once in effect the pod is not even read again until resizeRecheck.
func TestResizeTickRaisesOnceAndSettles(t *testing.T) {
	h := newResizeHarness(t)
	h.tickAsPrimary()
	if n := h.count("patch"); n != 1 {
		t.Fatalf("patches after first tick = %d, want 1", n)
	}
	if !strings.Contains(h.logs.String(), "state=requested") {
		t.Errorf("want a 'requested' log line, got:\n%s", h.logs)
	}
	h.tickAsPrimary() // spec matches, kubelet not done: poll, do not re-patch
	if n := h.count("patch"); n != 1 {
		t.Fatalf("re-patched while the resize was pending: %d patches", n)
	}
	if !strings.Contains(metricsText(h.a.metr), "pg_ha_agent_pod_resize_pending 1") {
		t.Error("pending gauge should be 1 while the kubelet has not actuated the size")
	}
	h.kubelet(t, true, nil)
	h.tickAsPrimary()
	out := metricsText(h.a.metr)
	if !strings.Contains(out, "pg_ha_agent_pod_sized_as_primary 1") || !strings.Contains(out, "pg_ha_agent_pod_resize_pending 0") {
		t.Errorf("gauges after actuation:\n%s", out)
	}
	gets := h.count("get")
	h.clock = h.clock.Add(30 * time.Second)
	h.tickAsPrimary()
	if h.count("get") != gets {
		t.Error("a settled size must not be re-read before resizeRecheck")
	}
	h.clock = h.clock.Add(resizeRecheck)
	h.tickAsPrimary()
	if h.count("get") != gets+1 {
		t.Error("a settled size must be re-read after resizeRecheck")
	}
	if c := strings.Count(h.logs.String(), "state=\"in effect\""); c != 1 {
		t.Errorf("'in effect' logged %d times, want once per change:\n%s", c, h.logs)
	}
}

// An Infeasible raise on the primary is an Error and the infeasible gauge, and the node is
// left serving -- nothing waits, and the patch is not hammered every tick.
func TestResizeTickInfeasibleRaiseIsReported(t *testing.T) {
	h := newResizeHarness(t)
	h.tickAsPrimary()
	h.kubelet(t, false, &corev1.PodCondition{Type: "PodResizePending", Status: corev1.ConditionTrue, Reason: k8s.ResizeInfeasible, Message: "Node didn't have enough capacity"})
	h.tickAsPrimary()
	if !strings.Contains(h.logs.String(), "level=ERROR") || !strings.Contains(h.logs.String(), "enough capacity") {
		t.Errorf("want an Error naming the kubelet's reason, got:\n%s", h.logs)
	}
	if !strings.Contains(metricsText(h.a.metr), "pg_ha_agent_pod_resize_infeasible 1") {
		t.Error("infeasible gauge should be 1")
	}
	gets, patches := h.count("get"), h.count("patch")
	h.tickAsPrimary()
	if h.count("get") != gets || h.count("patch") != patches {
		t.Error("an Infeasible resize is re-read only every resizeRecheck")
	}
}

// Lowering follows a demotion: the first standby tick asks for the small size.
func TestResizeTickLowersAStandby(t *testing.T) {
	h := newResizeHarness(t)
	h.tickAsPrimary()
	h.kubelet(t, true, nil)
	h.tickAsPrimary()
	h.a.resizeTick(context.Background(), reconcile.Decision{Action: reconcile.Follow},
		reconcile.Observation{Local: reconcile.LocalState{HasData: true, Running: true, InRecovery: true}}, nil)
	p, _ := h.cs.CoreV1().Pods("ns").Get(context.Background(), "pg-0", metav1.GetOptions{})
	if got := p.Spec.Containers[0].Resources.Limits.Memory(); got.Cmp(resource.MustParse("512Mi")) != 0 {
		t.Errorf("standby memory limit = %s, want 512Mi", got)
	}
}

// Off without PG_HA_RESIZE: no apiserver call at all.
func TestResizeTickOffWithoutSpec(t *testing.T) {
	h := newResizeHarness(t)
	h.a.cfg.Resize = nil
	h.tickAsPrimary()
	if len(h.cs.Actions()) != 0 {
		t.Errorf("resizeTick made %d API calls with resize off", len(h.cs.Actions()))
	}
}

// Kubernetes 1.35+ refuses a resize above node allocatable at admission (403) instead of
// setting PodResizePending=Infeasible; it is the same outcome and must be reported as one.
func TestResizeTickAdmissionRefusalIsInfeasible(t *testing.T) {
	h := newResizeHarness(t)
	h.cs.PrependReactor("patch", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "pg-0",
			errors.New("node didn't have enough allocatable resources: cpu, requested: 512100, allocatable: 16000"))
	})
	h.tickAsPrimary()
	out := metricsText(h.a.metr)
	if !strings.Contains(out, "pg_ha_agent_pod_resize_infeasible 1") {
		t.Errorf("admission refusal should set the infeasible gauge:\n%s", out)
	}
	if !strings.Contains(h.logs.String(), "level=ERROR") || !strings.Contains(h.logs.String(), "state=Infeasible") {
		t.Errorf("want an Error with state=Infeasible, got:\n%s", h.logs)
	}
}

// A transient patch failure (5xx, timeout) is retried after resizeRetry, not settled on for a
// whole resizeRecheck: a fresh primary must not sit at the standby size for a minute over one
// apiserver blip. And it is not retried every tick either (#298: the tick's apiserver budget).
func TestResizeTickTransientRefusalRetriesSoon(t *testing.T) {
	h := newResizeHarness(t)
	fail := true
	h.cs.PrependReactor("patch", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if fail {
			return true, nil, apierrors.NewInternalError(errors.New("etcdserver: leader changed"))
		}
		return false, nil, nil
	})
	h.tickAsPrimary()
	if strings.Contains(h.logs.String(), "level=ERROR") {
		t.Errorf("a transient failure is not the primary-cannot-be-raised Error:\n%s", h.logs)
	}
	gets := h.count("get")
	h.tickAsPrimary()
	if h.count("get") != gets {
		t.Error("a transient failure must not be retried on the very next tick")
	}
	fail = false
	h.clock = h.clock.Add(resizeRetry)
	h.tickAsPrimary()
	p, _ := h.cs.CoreV1().Pods("ns").Get(context.Background(), "pg-0", metav1.GetOptions{})
	if got := p.Spec.Containers[0].Resources.Limits.Memory(); got.Cmp(resource.MustParse("4Gi")) != 0 {
		t.Errorf("after resizeRetry the raise should be retried; memory limit = %s, want 4Gi", got)
	}
}
