// Package control is the agent's authenticated control API (#276): a small mTLS
// HTTP surface for the operations that previously required either `kubectl annotate`
// on the marker ConfigMap (pause, switchover) or a hand-run kubectl recipe (restore).
//
// Three properties shape everything here.
//
// It is a FACADE, not a second brain. Cluster-scope verbs write the same marker
// annotations kubectl writes, and the reconcile loop remains the sole authority for
// when anything actually happens. What the API adds over kubectl is preflight
// validation and a synchronous answer: `kubectl annotate switchover-target=pg-9`
// succeeds even when pg-9 does not exist, is on a divergent timeline, or is far
// behind, and the operator finds out by reading logs.
//
// It is SEPARATE from observability. The read-only :9200 surface gains no route
// here; this listener is its own port so a NetworkPolicy can admit Prometheus to one
// and nobody to the other.
//
// It runs INSIDE PID 1, next to the supervised postmaster. A panicking handler would
// take down Postgres's parent, and a handler that drove the postmaster directly
// would race the reconcile loop that owns it -- so every mutation of local state goes
// through the loop as an intent, and the server wraps handlers in a recover.
package control

import (
	"context"
	"encoding/json"
	"time"

	"github.com/cagriekin/pg-ha-agent/internal/pgbackrest"
)

// Verb is the authorization class of a request. Restore is deliberately its own
// verb: its RBAC grant is the one that widens the pod's Kubernetes privileges, so
// admitting a client to pause/switchover must not admit it to restore.
type Verb string

const (
	VerbObserve Verb = "observe"
	VerbControl Verb = "control"
	VerbRestore Verb = "restore"
)

// Identity is the authenticated client, derived from the verified TLS chain. The
// fingerprint (not the CN) is the durable identifier -- CNs are reissued, and it is
// what an auditor can match against an issued certificate.
type Identity struct {
	CN          string
	Fingerprint string // SHA-256 of the leaf certificate DER, hex
	Serial      string
}

// MemberState is one cluster member as this node last observed it.
type MemberState struct {
	Name string `json:"name"`
	// Self marks the member this API is running on. Every other member's fields come
	// from a cross-pod probe (or gossip), so they are this node's VIEW, not truth.
	Self bool   `json:"self"`
	Role string `json:"role"`
	// No omitempty on the booleans: false is a meaningful state, not an absent one, and
	// hasData/running are exactly what a client polls during a reinitialize re-clone --
	// omitting them would make "no data yet" indistinguishable from "field not reported".
	Reachable  bool   `json:"reachable"`
	Running    bool   `json:"running"`
	HasData    bool   `json:"hasData"`
	Timeline   uint32 `json:"timeline,omitempty"`
	TimelineOK bool   `json:"timelineKnown"`
	LSN        string `json:"lsn,omitempty"`
	// Gossip is true when the position came from the peer's pod annotation rather
	// than a live SQL probe, i.e. the peer is not reachable right now.
	Gossip bool `json:"gossip"`
}

// DecisionView is the reconcile loop's most recent decision.
//
// This is exposed because it is the agent's actual reasoning and today lives only in
// log lines -- "why is this standby not promoting" is answerable in one request with
// it and nearly unanswerable without it. It names internal actions, so it is
// documented as diagnostic output rather than a stable contract: new actions appear
// and reasons are reworded without a version bump.
type DecisionView struct {
	Action string    `json:"action"`
	Target string    `json:"target,omitempty"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// RecoveryView is WAL-replay progress. After a restore this is the progress that
// matters: the copy Job is gone by the time the cluster is back up, and what the
// operator is waiting on is recovery reaching the point they asked for.
type RecoveryView struct {
	InRecovery     bool    `json:"inRecovery"`
	ReceiveLSN     string  `json:"receiveLsn,omitempty"`
	ReplayLSN      string  `json:"replayLsn,omitempty"`
	ReplayLagBytes *uint64 `json:"replayLagBytes,omitempty"`
	// LastReplayTime is the commit timestamp of the last replayed transaction --
	// directly comparable to a `targetType: time` restore target.
	LastReplayTime string `json:"lastReplayTime,omitempty"`
}

// Snapshot is the state the agent publishes to the API, refreshed once per reconcile
// tick. It is deliberately a CACHED view: re-probing every peer inside an HTTP
// handler would put N SQL round trips (and their timeouts) on the request path. The
// age is reported so a client can judge it, and the intents that need freshness read
// the marker directly instead.
type Snapshot struct {
	Node             string
	PGMajor          string
	HoldsLease       bool
	LeaseHolder      string
	Paused           bool
	SwitchoverTarget string
	MarkerPresent    bool
	MarkerPrimary    string
	Local            MemberState
	Peers            []MemberState
	Decision         DecisionView
	Recovery         RecoveryView
	LastRestore      pgbackrest.RestoreRecord
	ObservedAt       time.Time
}

// MarkerView is a FRESH read of the cluster's intent markers, used for preconditions
// that must not act on a cached value (pausing, and the restore gate that requires
// the cluster to be paused).
type MarkerView struct {
	Present          bool
	Paused           bool
	PausedBy         string
	SwitchoverTarget string
	Primary          string
}

// IntentKind is a node-local operation the reconcile loop performs on behalf of the
// API. Handlers never touch the postmaster: the loop owns it, and a concurrent stop
// from an HTTP goroutine would either be undone on the next tick or be read as a
// fault and trigger a failover.
type IntentKind int

const (
	// IntentRestart stops and starts the local postmaster in place. It restarts
	// explicitly rather than leaving the start to the loop, because a paused cluster's
	// loop is a no-op and would leave Postgres down.
	IntentRestart IntentKind = iota
	// IntentReload sends SIGHUP: re-read postgresql.conf/pg_hba.conf without a restart.
	IntentReload
	// IntentStop stops the local postmaster and leaves it stopped -- the restore
	// precondition. Only safe while paused, which is why the restore handler verifies
	// that against a fresh marker read first; an unpaused loop would restart it.
	IntentStop
	// IntentReinitialize discards this replica's data directory so the reconcile loop
	// rebuilds it from the lease holder. It stops PostgreSQL and empties PGDATA and
	// nothing more: the loop's ordinary "empty data, not the chosen primary -> clone
	// from the lease holder" path does the rebuild, so there is no second clone
	// implementation to diverge from the one every fresh replica uses.
	IntentReinitialize
)

func (k IntentKind) String() string {
	switch k {
	case IntentRestart:
		return "restart"
	case IntentReload:
		return "reload"
	case IntentStop:
		return "stop"
	case IntentReinitialize:
		return "reinitialize"
	default:
		return "unknown"
	}
}

// Cluster is the marker side of the API: cluster-scope intents, plus the fresh read
// the preconditions need.
type Cluster interface {
	Marker(ctx context.Context) (MarkerView, error)
	SetPause(ctx context.Context, on bool, requestedBy string) error
	SetSwitchoverTarget(ctx context.Context, target, requestedBy string) error
	ClearSwitchoverTarget(ctx context.Context) error
}

// Node is the local node: its published snapshot and the intent queue.
type Node interface {
	Snapshot() Snapshot
	// Submit hands an intent to the reconcile loop and waits for its outcome. It
	// returns ctx.Err() if the loop does not get to it in time -- the caller reports
	// that as a timeout, with the node's state still observable, rather than
	// pretending the operation did or did not happen.
	Submit(ctx context.Context, kind IntentKind) error
}

// RestoreRequest is the body of POST /v1/restore.
//
// TargetType/Target/BackupSet are the only fields that change what the restore does;
// they are applied as environment overrides on the cloned Job. PodOrdinal is
// confirm-only -- the volume a restore overwrites is rendered into the Job from
// values and is not an HTTP-body decision.
type RestoreRequest struct {
	// Node must equal the pod serving the request. The restore runs against THIS
	// pod's data volume and needs its postmaster stopped, so being explicit about
	// which pod you are addressing makes a wrong-pod call impossible even through a
	// misconfigured proxy.
	Node string `json:"node"`
	// Confirm must equal the cluster (StatefulSet) name, and Force must be true: this
	// overwrites a live data directory.
	Confirm string `json:"confirm"`
	Force   bool   `json:"force"`

	TargetType string `json:"targetType"`
	Target     string `json:"target"`
	BackupSet  string `json:"backupSet"`
	PodOrdinal *int   `json:"podOrdinal"`
	// Replace deletes a previous restore Job of the same name before creating this
	// one. Jobs are immutable and the old one is the record of the last restore, so
	// replacing it is explicit rather than automatic.
	Replace bool `json:"replace"`
}

// RestoreView is the state of the restore Job plus, where available, progress.
type RestoreView struct {
	// Phase is none | pending | running | succeeded | failed. "none" means no restore
	// Job exists -- which, after a scale-down/up cycle, is the normal end state, so
	// LastRestore carries the outcome instead.
	Phase          string               `json:"phase"`
	JobName        string               `json:"jobName,omitempty"`
	CreatedAt      *time.Time           `json:"createdAt,omitempty"`
	StartedAt      *time.Time           `json:"startedAt,omitempty"`
	CompletedAt    *time.Time           `json:"completedAt,omitempty"`
	Active         int32                `json:"active,omitempty"`
	Succeeded      int32                `json:"succeeded,omitempty"`
	Failed         int32                `json:"failed,omitempty"`
	RequestedBy    string               `json:"requestedBy,omitempty"`
	RequestedAt    string               `json:"requestedAt,omitempty"`
	PodName        string               `json:"podName,omitempty"`
	PodPhase       string               `json:"podPhase,omitempty"`
	WaitingReason  string               `json:"waitingReason,omitempty"`
	WaitingMessage string               `json:"waitingMessage,omitempty"`
	Progress       *pgbackrest.Progress `json:"progress,omitempty"`
	// ContainerStarted distinguishes a Pending pod whose container is at least
	// starting (image pull, config error -- WaitingReason says which) from one that has
	// not been scheduled or cannot attach its volume. Internal to the hint logic.
	ContainerStarted bool `json:"-"`
	// LastRestore is the outcome of the last restore that ran on this data volume,
	// read from the status file on the PVC. It outlives the Job (and its logs), so it
	// is what answers "what happened to my restore?" after the cluster is back up.
	LastRestore *pgbackrest.RestoreRecord `json:"lastRestore,omitempty"`
	// Hint explains a stuck restore in the terms of the operator's next action.
	Hint string `json:"hint,omitempty"`
	// NextSteps are the remaining runbook commands. The API cannot scale the
	// StatefulSet (scaling to 0 would delete the agent that would report progress), so
	// it hands back the steps it deliberately does not take.
	NextSteps []string `json:"nextSteps,omitempty"`
}

// Backups is the pgBackRest surface. A nil Backups means pgbackrest is not enabled
// for this release, and the backup/restore routes report that rather than 404ing as
// if they did not exist.
type Backups interface {
	Info(ctx context.Context) (json.RawMessage, error)
	Restore(ctx context.Context, req RestoreRequest, id Identity) (RestoreView, error)
	RestoreStatus(ctx context.Context) (RestoreView, error)
	DeleteRestore(ctx context.Context) error
	// RestoreEnabled reports whether triggering a restore is turned on (the separate
	// opt-in). Info/RestoreStatus remain available without it: reading the repository
	// and the last outcome needs no extra Kubernetes privilege.
	RestoreEnabled() bool
}

// Metrics counts control-plane traffic. Implemented by observe.Metrics; Nop keeps
// tests free of it.
type Metrics interface {
	IncControlRequest()
	IncControlRejected()
	IncControlIntent()
	IncControlRestoreRequest()
}

// Nop is a Metrics that counts nothing.
type Nop struct{}

func (Nop) IncControlRequest()        {}
func (Nop) IncControlRejected()       {}
func (Nop) IncControlIntent()         {}
func (Nop) IncControlRestoreRequest() {}
