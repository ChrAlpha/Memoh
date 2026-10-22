package agenttest

import "context"

// NoopFence is the runtime fence of a test that does not exercise fencing:
// activation always succeeds.
type NoopFence struct{}

// Activate implements the session runtime's fence.
func (NoopFence) Activate(context.Context, string, string, int64) error { return nil }
