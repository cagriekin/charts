package mechanism

// Compile-time proof that every implementation satisfies Mechanism.
//
// Worth having explicitly: reconcile and cmd/agent hold a Mechanism, never a concrete type,
// so nothing else in the tree forces this assignment. Without it, adding a method to the
// interface would fail at the factory rather than here, and an implementation that quietly
// drifted from the interface would compile until it was selected at runtime.
//
// One implementation since #294 removed mechanism.Repmgr. The INTERFACE deliberately stays:
// it is the seam that made the repmgr-to-native migration survivable one method at a time,
// and it is what a future second implementation (or the durability-parity work) builds on.
var _ Mechanism = (*Native)(nil)
