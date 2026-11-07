package main

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/cagriekin/pg-ha-agent/internal/k8s"
	"github.com/cagriekin/pg-ha-agent/internal/observe"
	"github.com/cagriekin/pg-ha-agent/internal/reconcile"
)

// In-place pod resize on a role change (#386).
//
// With postgresql/pgbackrest standbyResources set, the StatefulSet template carries the
// STANDBY size, so every pod the controller creates -- a fresh install, a rolling update,
// a rescheduled primary -- starts small. The agent is the one component that knows the role
// at the moment it changes, so it sizes its own pod: up to the primary size once it is (or
// is becoming) the primary, down to the standby size once it is back as a standby. The
// pgbackrest sidecar moves with postgresql because the backup CronJob only ever execs into
// the primary's; on a standby it runs `sleep infinity`.
//
// Resizing is never on the critical path. It runs after act(), outside opMu, under its own
// fence budget, and nothing waits for the kubelet: a promotion is not delayed by a raise,
// and a raise the node cannot fit (Infeasible) or cannot fit yet (Deferred) leaves the
// primary serving at the size it has, reported by log and metric.

// podSize is the size this pod's observed role calls for.
type podSize int

const (
	sizeKeep    podSize = iota // no change: the role is not settled enough to size by
	sizePrimary                // postgresql.resources / pgbackrest.resources
	sizeStandby                // postgresql.standbyResources / pgbackrest.standbyResources
)

func (s podSize) String() string {
	return [...]string{"keep", "primary", "standby"}[s]
}

// resizeRecheck bounds how long a settled (or Infeasible / refused) size goes unread: long
// enough that a steady cluster costs one GET per pod per minute, short enough that an
// outside change (a manual kubectl resize, a VPA in recommend-and-apply mode) is undone
// within a minute rather than lasting until the next role change.
const resizeRecheck = time.Minute

// resizeRetry is the back-off after a failure that says nothing about the size: an unreadable
// pod or a patch that timed out / hit a 5xx / was throttled. Short, because a fresh primary
// waiting on its raise should not wait a whole resizeRecheck over an apiserver blip; non-zero,
// because retrying every tick against a blackholed apiserver would spend a resize budget on
// every tick and push the lease holder's tick past the /healthz staleness threshold (#298).
const resizeRetry = 15 * time.Second

// resizeState is resizeTick's memory between ticks (reconcile goroutine only).
type resizeState struct {
	target    podSize          // the size last asked for
	settled   bool             // target was actuated, or is in a state only time or an operator changes
	checkedAt time.Time        // last GET of the pod
	retryAt   time.Time        // after a transient failure for target: no apiserver call before this
	keepSince time.Time        // first tick of the current run of sizeKeep ticks; zero outside one
	logged    string           // last logged state, so the log carries one line per change
	now       func() time.Time // nil = time.Now; tests set it
}

// desiredSize maps what this tick observed and decided to the size the pod should have.
//
//   - primary: the tick promoted successfully (raise on the same tick pg_promote is
//     requested, without waiting for recovery to end), stays primary, or this node runs
//     read-write while holding the lease (a primary recreated small by a rolling update;
//     a paused cluster, whose decision is not StayPrimary).
//   - standby: it runs in recovery without the lease. A demoted primary only gets here
//     after its server has restarted as a standby (rewind or rejoin), so its memory use is
//     back to a standby's and fits under the lower limit; lowering earlier would have the
//     kubelet refuse the decrease against the old primary's working set.
//   - keep: anything else -- stopped, cloning, unreachable, a read-write node without the
//     lease that is being fenced, a lease holder still in recovery that did not promote.
//     Sizing by a role that is in flux would flap the pod for nothing.
func desiredSize(dec reconcile.Decision, obs reconcile.Observation, actErr error) podSize {
	if actErr == nil && (dec.Action == reconcile.Promote || dec.Action == reconcile.StayPrimary) {
		return sizePrimary
	}
	l := obs.Local
	if l.Running && !l.InRecovery && obs.HoldLease {
		return sizePrimary
	}
	if l.Running && l.InRecovery && !obs.HoldLease {
		return sizeStandby
	}
	return sizeKeep
}

// resizeSizes is the per-container size for s, from PG_HA_RESIZE.
func (a *agent) resizeSizes(s podSize) map[string]corev1.ResourceRequirements {
	out := make(map[string]corev1.ResourceRequirements, len(a.cfg.Resize))
	for name, p := range a.cfg.Resize {
		if s == sizePrimary {
			out[name] = p.Primary
		} else {
			out[name] = p.Standby
		}
	}
	return out
}

// resizeTick sizes this pod for its role (#386). A no-op unless PG_HA_RESIZE is set.
func (a *agent) resizeTick(ctx context.Context, dec reconcile.Decision, obs reconcile.Observation, actErr error) {
	if len(a.cfg.Resize) == 0 || a.kube == nil || a.dcs == nil {
		return
	}
	want := desiredSize(dec, obs, actErr)
	// A Promote or StayPrimary that returned nil is not proof this node still holds the lease:
	// act() returns nil when the lease lapsed between observe() and the promote, and after a
	// post-promote loss it leaves the fence to OnLost. Raising then would reserve the primary
	// size on a node that is about to be (or already is) a standby -- room the real primary's
	// raise may need on a shared node -- only to lower it again a few ticks later.
	if want == sizePrimary && !a.dcs.IsLeader() {
		want = sizeKeep
	}
	rs := &a.resize
	now := time.Now()
	if rs.now != nil {
		now = rs.now()
	}
	if want == sizeKeep {
		// The role is in flux, so no size is called for; re-read on the next sizing tick. The
		// gauges are cleared only once the flux has lasted resizeRecheck: a pod that has really
		// left its role must not keep a past role's infeasible/sized-as-primary paging, but a
		// single failed local probe on a primary stuck Infeasible must not zero the gauge either
		// -- each zero restarts the alerts' `for:` window, and a primary that flaps now and then
		// would be stuck at the standby size with PGHAPodResizeInfeasible/Stuck never firing.
		rs.settled = false
		if rs.keepSince.IsZero() {
			rs.keepSince = now
		}
		if now.Sub(rs.keepSince) >= resizeRecheck {
			a.metr.SetResize(observe.ResizeStats{})
		}
		return
	}
	rs.keepSince = time.Time{}
	if want == rs.target && rs.settled && now.Sub(rs.checkedAt) < resizeRecheck {
		return
	}
	if want == rs.target && now.Before(rs.retryAt) {
		return
	}
	// Bounded like refreshNodeCount, at HALF the fence budget: tick()'s context has no
	// deadline, and this GET (+PATCH) runs on the same tick as observe()'s marker/gossip reads,
	// the marker block, act() and topologyTick, each already spending a full budget against a
	// blackholed apiserver. A whole budget more would push the lease holder's worst-case tick
	// past 3x the reconcile interval and have the kubelet kill the serving primary over a
	// stale /healthz while the apiserver is unreachable (#298). A GET and one PATCH fit.
	rctx, cancel := context.WithTimeout(ctx, a.fenceBudget()/2)
	defer cancel()
	sizes := a.resizeSizes(want)
	st, err := a.kube.PodResizeStatus(rctx, a.cfg.PodName, sizes)
	if err != nil {
		rs.target, rs.settled, rs.retryAt = want, false, now.Add(resizeRetry)
		a.logResize(want, "unreadable", fmt.Sprintf("read pod for resize: %v", err), false)
		return
	}
	rs.retryAt = time.Time{}
	var refused error
	if !st.SpecMatches {
		refused = a.kube.ResizePod(rctx, a.cfg.PodName, sizes, st.Containers)
		switch {
		case refused == nil:
			// The kubelet has not seen it yet; whatever it reports next tick is the news.
			st = k8s.PodResize{SpecMatches: true}
		case k8s.ResizeRefusedInfeasible(refused):
			// The apiserver's own allocatable check (Kubernetes 1.35+): the kubelet's
			// Infeasible, answered at admission instead of on a pod condition.
			st.Pending, st.Message, refused = k8s.ResizeInfeasible, refused.Error(), nil
		case k8s.ResizeRefusedTransient(refused):
			// A timeout, 5xx, throttle or conflict says nothing about the size: retry after
			// resizeRetry rather than settle on it for a whole resizeRecheck, which would leave
			// a fresh primary at the standby size for a minute over one apiserver blip.
			rs.target, rs.settled, rs.retryAt = want, false, now.Add(resizeRetry)
			a.metr.SetResize(observe.ResizeStats{Pending: true})
			a.logResize(want, "retrying", refused.Error(), false)
			return
		}
	}
	rs.target, rs.checkedAt = want, now
	var state, detail string
	switch {
	case refused != nil:
		state, detail = "refused", refused.Error()
	case st.Actuated:
		state = "in effect"
	case st.Pending != "":
		state, detail = st.Pending, st.Message
	case st.InProgress:
		state, detail = "in progress", st.Message
	default:
		state = "requested"
	}
	// Settled means "nothing to poll for each tick": done, or a state the kubelet will not
	// leave on its own (Infeasible) or the apiserver will keep refusing. Those are re-read
	// (and a refused patch retried) every resizeRecheck instead.
	rs.settled = st.Actuated || st.Pending == k8s.ResizeInfeasible || refused != nil
	// Deferred, or in progress (a memory limit the kubelet will not lower below usage can stay
	// there indefinitely): nothing this agent does moves it, so poll it every resizeRetry
	// rather than spend a GET -- and half a fence budget against a slow apiserver -- on every
	// tick for as long as the node stays full.
	if !rs.settled && (st.Pending != "" || st.InProgress) {
		rs.retryAt = now.Add(resizeRetry)
	}
	a.metr.SetResize(observe.ResizeStats{
		SizedAsPrimary: want == sizePrimary && st.Actuated,
		Pending:        !st.Actuated,
		Infeasible:     st.Pending == k8s.ResizeInfeasible,
	})
	a.logResize(want, state, detail, want == sizePrimary && (st.Pending == k8s.ResizeInfeasible || refused != nil))
}

// logResize logs a resize state once per change. A primary that cannot get its size is an
// Error: it serves at the standby size and nothing else says so.
func (a *agent) logResize(want podSize, state, detail string, isError bool) {
	key := want.String() + "/" + state + "/" + detail
	if key == a.resize.logged {
		return
	}
	a.resize.logged = key
	args := []any{"size", want.String(), "state", state, "containers", resizeContainerNames(a.cfg.Resize)}
	if detail != "" {
		args = append(args, "detail", detail)
	}
	switch {
	case isError:
		a.log.Error("this pod is the primary but cannot be raised to the primary size; it keeps serving at its current size. Free room on the node, lower postgresql.resources / pgbackrest.resources, or move the pod (#386)", args...)
	case state == "in effect" || state == "requested" || state == "in progress":
		a.log.Info("in-place pod resize (#386)", args...)
	default:
		a.log.Warn("in-place pod resize not in effect (#386)", args...)
	}
}
