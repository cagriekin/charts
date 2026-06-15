package process

import "context"

// Supervisor wraps a Postmaster with the agent's lifecycle operations. It is thin
// and Postmaster-agnostic so the demote-mode policy is unit-testable.
type Supervisor struct {
	pm Postmaster
}

// NewSupervisor wraps pm.
func NewSupervisor(pm Postmaster) *Supervisor { return &Supervisor{pm: pm} }

func (s *Supervisor) Start(ctx context.Context) error  { return s.pm.Start(ctx) }
func (s *Supervisor) Reload(ctx context.Context) error { return s.pm.Reload(ctx) }
func (s *Supervisor) Stop(ctx context.Context, mode StopMode) error {
	return s.pm.Stop(ctx, mode)
}

// Demote stops the local Postgres. fence=true uses Immediate (the bounded
// crash-stop for the soft-fence path, decoupled from checkpoint load); fence=false
// uses Fast (a clean shutdown for graceful/preStop handoff).
func (s *Supervisor) Demote(ctx context.Context, fence bool) error {
	mode := Fast
	if fence {
		mode = Immediate
	}
	return s.pm.Stop(ctx, mode)
}
