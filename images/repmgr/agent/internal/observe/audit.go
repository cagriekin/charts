package observe

import "log/slog"

// Audit emits one structured line per reconcile decision, so post-incident
// "why did pod-X demote at 14:03?" is answerable from logs (Part H6). Reasons come
// from the reconcile package; never log secrets here.
func Audit(l *slog.Logger, holdLease bool, action, target, reason string) {
	l.Info("reconcile decision",
		"hold_lease", holdLease,
		"action", action,
		"target", target,
		"reason", reason,
	)
}
