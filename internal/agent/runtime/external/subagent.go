package external

import (
	"context"
	"errors"

	"github.com/felinics/memoh/internal/agent/sessionmode"
)

// ExecutionState is separate from the turn outcome. An unsuccessful task can
// still own a live process; a protocol result alone does not release its hold.
type ExecutionState string

const (
	ExecutionNotStarted ExecutionState = "not_started"
	ExecutionStopped    ExecutionState = "stopped"
	ExecutionUnknown    ExecutionState = "unknown"
)

var ErrTerminationUnconfirmed = errors.New("external execution termination is unconfirmed")

// SubagentCapabilities declares the driver's managed worker contract.
// Exact resume and conversation fork are optional; replaying a transcript
// into a new prompt is neither capability.
type SubagentCapabilities struct {
	IsolatedExecution  bool `json:"isolated_execution"`
	NoNestedDelegation bool `json:"no_nested_delegation"`
	EarlyRuntimeAnchor bool `json:"early_runtime_anchor"`
	Resume             bool `json:"resume"`
	Fork               bool `json:"fork"`
}

func (c SubagentCapabilities) Supported() bool {
	return c.IsolatedExecution && c.NoNestedDelegation && c.EarlyRuntimeAnchor
}

// SubagentDriver keeps process control and its termination evidence with the
// runtime owner. ReconcileSubagent addresses the exact old child run, never
// the bot's parent or another child. It must not dispatch another prompt.
type SubagentDriver interface {
	Driver
	SubagentCapabilities() SubagentCapabilities
	ReconcileSubagent(context.Context, PromptInput) error
}

func (p PromptInput) IsSubagent() bool { return p.SessionMode == sessionmode.Subagent }
func (p PromptInput) CanAskUser() bool { return p.CanRequestUserInput && !p.IsSubagent() }

// RecordAnchor is a fenced durability barrier. Drivers call it as soon as
// they learn a runtime session/turn identity, before proceeding with work.
func (p PromptInput) RecordAnchor(ctx context.Context, metadata map[string]any) error {
	if p.PersistRuntimeState == nil {
		if p.IsSubagent() {
			return errors.New("child runtime anchor persistence is unavailable")
		}
		return nil
	}
	return p.PersistRuntimeState(ctx, metadata)
}
