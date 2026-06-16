package k8s

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PauseAnnotation, when set to "true" on the marker ConfigMap, puts the agent in
// maintenance mode (Part H1): it keeps renewing the Lease and serving but suspends
// automatic promote/demote/fence/self-health. An annotation (not a Data key) is
// used so it survives WriteMarker, which rewrites only the ConfigMap's Data. Toggle
// with `kubectl annotate configmap <fullname>-primary pg-ha/pause=true` (and
// `pg-ha/pause-` to resume).
const PauseAnnotation = "pg-ha/pause"

// Marker is the durable highwater primary marker (<fullname>-primary ConfigMap):
// the highest-timeline primary ever recorded, so a node booting first under
// OrderedReady can tell it is stale (#125). Malformed is set when the marker
// exists but its timeline is missing or unparseable — callers must fail closed
// (#174), never treating it as "no constraint".
type Marker struct {
	Present    bool
	Malformed  bool
	Primary    string
	Timeline   uint32
	TimelineOK bool
	Paused     bool // maintenance mode: PauseAnnotation == "true" on the ConfigMap
}

// ReadMarker reads the marker ConfigMap. A missing marker is Present=false (not an
// error). A present marker with a missing/unparseable timeline is Malformed.
func (c *Client) ReadMarker(ctx context.Context, name string) (Marker, error) {
	cm, err := c.cs.CoreV1().ConfigMaps(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Marker{Present: false}, nil
	}
	if err != nil {
		return Marker{}, fmt.Errorf("get marker %s: %w", name, err)
	}
	m := Marker{Present: true, Primary: cm.Data["primary"], Paused: strings.EqualFold(strings.TrimSpace(cm.Annotations[PauseAnnotation]), "true")}
	tlStr, ok := cm.Data["timeline"]
	if !ok || tlStr == "" {
		m.Malformed = true
		return m, nil
	}
	v, perr := strconv.ParseUint(tlStr, 10, 32)
	if perr != nil {
		m.Malformed = true
		return m, nil
	}
	m.Timeline, m.TimelineOK = uint32(v), true
	return m, nil
}

// WriteMarker records primary + timeline (decimal) in the marker ConfigMap,
// creating it if absent. Callers advance it monotonically (write only when the
// timeline is at least the recorded highwater).
func (c *Client) WriteMarker(ctx context.Context, name, primary string, timeline uint32) error {
	data := map[string]string{
		"primary":  primary,
		"timeline": strconv.FormatUint(uint64(timeline), 10),
	}
	cms := c.cs.CoreV1().ConfigMaps(c.namespace)
	cm, err := cms.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, cerr := cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.namespace},
			Data:       data,
		}, metav1.CreateOptions{})
		if cerr != nil {
			return fmt.Errorf("create marker %s: %w", name, cerr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get marker %s: %w", name, err)
	}
	cm.Data = data
	if _, uerr := cms.Update(ctx, cm, metav1.UpdateOptions{}); uerr != nil {
		return fmt.Errorf("update marker %s: %w", name, uerr)
	}
	return nil
}
