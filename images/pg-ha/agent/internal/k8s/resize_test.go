package k8s

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func size(reqCPU, reqMem, limCPU, limMem string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(reqCPU), corev1.ResourceMemory: resource.MustParse(reqMem)},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(limCPU), corev1.ResourceMemory: resource.MustParse(limMem)},
	}
}

func resizePod(pg, br corev1.ResourceRequirements, conds ...corev1.PodCondition) *corev1.Pod {
	pgS, brS := pg, br
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-0", Namespace: ns},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "postgresql", Resources: pg},
			{Name: "pgbackrest", Resources: br},
		}},
		Status: corev1.PodStatus{
			Conditions: conds,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "postgresql", Resources: &pgS},
				{Name: "pgbackrest", Resources: &brS},
			},
		},
	}
}

// #386: the resize is ONE patch on the pods/resize subresource naming both containers, so
// postgresql and pgbackrest move to the same role's size together -- a patch on the pod
// itself is rejected for resources, and two patches could leave the pod half-sized.
func TestResizePodPatchesResizeSubresourceWithBothContainers(t *testing.T) {
	small := size("50m", "256Mi", "500m", "512Mi")
	cs := fake.NewSimpleClientset(resizePod(small, small))
	var sub string
	var body []byte
	cs.PrependReactor("patch", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pa := a.(k8stesting.PatchAction)
		sub, body = pa.GetSubresource(), pa.GetPatch()
		if pa.GetPatchType() != "application/strategic-merge-patch+json" {
			t.Errorf("patch type = %s, want strategic merge", pa.GetPatchType())
		}
		return true, nil, nil
	})
	c := NewWithClient(cs, ns)
	want := map[string]corev1.ResourceRequirements{
		"postgresql": size("200m", "1Gi", "2", "4Gi"),
		"pgbackrest": size("100m", "256Mi", "1", "1Gi"),
	}
	if err := c.ResizePod(context.Background(), "pg-0", want, []string{"postgresql", "pgbackrest"}); err != nil {
		t.Fatal(err)
	}
	if sub != "resize" {
		t.Fatalf("subresource = %q, want resize", sub)
	}
	var got struct {
		Spec struct {
			Containers []struct {
				Name      string `json:"name"`
				Resources struct {
					Requests map[string]string `json:"requests"`
					Limits   map[string]string `json:"limits"`
				} `json:"resources"`
			} `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("patch body %s: %v", body, err)
	}
	// The pod's own order (postgresql, then pgbackrest), NOT name order: a strategic merge
	// reorders the list to the patch's order, and the apiserver refuses a reordering resize.
	if len(got.Spec.Containers) != 2 || got.Spec.Containers[0].Name != "postgresql" || got.Spec.Containers[1].Name != "pgbackrest" {
		t.Fatalf("containers in patch = %s", body)
	}
	pgc := got.Spec.Containers[0].Resources
	if pgc.Requests["cpu"] != "200m" || pgc.Requests["memory"] != "1Gi" || pgc.Limits["cpu"] != "2" || pgc.Limits["memory"] != "4Gi" {
		t.Errorf("postgresql resources in patch = %+v", pgc)
	}
}

// Only cpu and memory are the agent's: an ephemeral-storage limit the template set must not
// be dropped by a patch that would otherwise replace the whole map.
func TestResizePatchCarriesOnlyCPUAndMemory(t *testing.T) {
	w := size("1", "1Gi", "1", "1Gi")
	w.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse("2Gi")
	b, err := resizePatch(map[string]corev1.ResourceRequirements{"postgresql": w}, []string{"postgresql"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ephemeral") {
		t.Errorf("patch carries a resource the agent does not manage: %s", b)
	}
}

func TestResizeStateOf(t *testing.T) {
	small := size("50m", "256Mi", "500m", "512Mi")
	big := size("200m", "1Gi", "2", "4Gi")
	want := map[string]corev1.ResourceRequirements{"postgresql": big, "pgbackrest": big}
	cond := func(typ, reason string) corev1.PodCondition {
		return corev1.PodCondition{Type: corev1.PodConditionType(typ), Status: corev1.ConditionTrue, Reason: reason, Message: "m"}
	}
	for _, tc := range []struct {
		name                              string
		pod                               *corev1.Pod
		specMatches, actuated, inProgress bool
		pending                           string
	}{
		{name: "not yet patched", pod: resizePod(small, small)},
		{name: "done", pod: resizePod(big, big), specMatches: true, actuated: true},
		{name: "deferred", pod: func() *corev1.Pod {
			p := resizePod(big, big, cond(condResizePending, ResizeDeferred))
			p.Status.ContainerStatuses[0].Resources = &small
			return p
		}(), specMatches: true, pending: ResizeDeferred},
		{name: "infeasible", pod: resizePod(big, big, cond(condResizePending, ResizeInfeasible)), specMatches: true, pending: ResizeInfeasible},
		{name: "in progress", pod: resizePod(big, big, cond(condResizeInProgress, "")), specMatches: true, inProgress: true},
		{name: "spec patched, kubelet not yet reporting", pod: func() *corev1.Pod {
			p := resizePod(big, big)
			p.Status.ContainerStatuses[1].Resources = nil
			return p
		}(), specMatches: true},
		{name: "one container still small", pod: resizePod(big, small)},
		{name: "equal by value, not by string", pod: resizePod(size("0.2", "1024Mi", "2000m", "4096Mi"), big), specMatches: true, actuated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := resizeStateOf(tc.pod, want)
			if err != nil {
				t.Fatal(err)
			}
			if st.SpecMatches != tc.specMatches || st.Actuated != tc.actuated || st.InProgress != tc.inProgress || st.Pending != tc.pending {
				t.Errorf("got %+v, want specMatches=%v actuated=%v inProgress=%v pending=%q", st, tc.specMatches, tc.actuated, tc.inProgress, tc.pending)
			}
		})
	}
}

// A container named in the spec but absent from the pod is a chart/env mismatch: an error,
// not a resize to wait out forever.
func TestResizeStateOfMissingContainer(t *testing.T) {
	small := size("50m", "256Mi", "500m", "512Mi")
	p := resizePod(small, small)
	p.Spec.Containers = p.Spec.Containers[:1]
	if _, err := resizeStateOf(p, map[string]corev1.ResourceRequirements{"pgbackrest": small}); err == nil {
		t.Fatal("want an error for a container the pod does not have")
	}
}

func TestResizePatchRefusesAContainerOutsideThePod(t *testing.T) {
	w := size("1", "1Gi", "1", "1Gi")
	if _, err := resizePatch(map[string]corev1.ResourceRequirements{"pgbackrest": w}, []string{"postgresql"}); err == nil {
		t.Fatal("want an error for a container the pod does not have")
	}
}
