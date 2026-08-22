package mechanism

// Compile-time proof that every implementation satisfies Mechanism.
//
// Worth having explicitly: reconcile and cmd/agent hold a Mechanism, never a concrete type,
// so nothing else in the tree forces these assignments. Without this, adding a method to the
// interface would fail at the factory rather than here, and adding an implementation that
// quietly drifts from the interface would compile until it was selected at runtime.
var (
	_ Mechanism = (*Repmgr)(nil)
	_ Mechanism = (*Native)(nil)
)
