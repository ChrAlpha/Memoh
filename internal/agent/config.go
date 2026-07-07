package agent

import (
	"context"
	"log/slog"

	agenttools "github.com/memohai/memoh/internal/agent/tools"
	"github.com/memohai/memoh/internal/hooks"
	"github.com/memohai/memoh/internal/workspace/bridge"
)

// ContextViewApplier rebuilds the provider-facing RunConfig through the
// context view pipeline. It runs after tool usage is appended to the system
// prompt and before generate options are built, so the selection, placement
// and cache plan cover the exact provider input.
type ContextViewApplier func(context.Context, RunConfig) RunConfig

const (
	DefaultToolOutputMaxBytes  = 64 * 1024
	DefaultToolOutputMaxLines  = 2000
	DefaultSystemFilesMaxBytes = 32 * 1024
)

type Limits struct {
	ToolOutputMaxBytes  int
	ToolOutputMaxLines  int
	SystemFilesMaxBytes int
}

// Deps holds all service dependencies for the Agent.
type Deps struct {
	BridgeProvider     bridge.Provider
	HookService        *hooks.Service
	Logger             *slog.Logger
	Limits             Limits
	ContextViewApplier ContextViewApplier
	LoopReselectMode   LoopReselectMode
}

// LoopReselectMode is the server-level rollout mode for the in-loop context
// step reselector (cfg.ContextStepReselector).
type LoopReselectMode string

const (
	// LoopReselectActive invokes the reselector and applies its result.
	LoopReselectActive LoopReselectMode = "active"
	// LoopReselectShadow invokes the reselector but never applies its
	// result: legacy mid-task pruning remains the actual mutation. The
	// reselector's would-be Dropped/Truncated/DropReasons still land on the
	// step's StepSnapshot (with ReselectionApplied=false) for comparison
	// against the legacy prune's real MutationMidTaskPrune record.
	LoopReselectShadow LoopReselectMode = "shadow"
	// LoopReselectOff skips the reselector entirely, as if
	// cfg.ContextStepReselector were nil.
	LoopReselectOff LoopReselectMode = "off"
)

// Normalize maps an unrecognized or empty mode to LoopReselectActive.
func (m LoopReselectMode) Normalize() LoopReselectMode {
	switch m {
	case LoopReselectShadow, LoopReselectOff:
		return m
	default:
		return LoopReselectActive
	}
}

func DefaultLimits() Limits {
	return Limits{
		ToolOutputMaxBytes:  DefaultToolOutputMaxBytes,
		ToolOutputMaxLines:  DefaultToolOutputMaxLines,
		SystemFilesMaxBytes: DefaultSystemFilesMaxBytes,
	}
}

func LimitsFromValues(toolOutputMaxBytes, toolOutputMaxLines, systemFilesMaxBytes int) Limits {
	return Limits{
		ToolOutputMaxBytes:  toolOutputMaxBytes,
		ToolOutputMaxLines:  toolOutputMaxLines,
		SystemFilesMaxBytes: systemFilesMaxBytes,
	}.Normalize()
}

func (l Limits) Normalize() Limits {
	defaults := DefaultLimits()
	if l.ToolOutputMaxBytes <= 0 {
		l.ToolOutputMaxBytes = defaults.ToolOutputMaxBytes
	}
	if l.ToolOutputMaxLines <= 0 {
		l.ToolOutputMaxLines = defaults.ToolOutputMaxLines
	}
	if l.SystemFilesMaxBytes <= 0 {
		l.SystemFilesMaxBytes = defaults.SystemFilesMaxBytes
	}
	return l
}

func (l Limits) ToolOutputLimit() agenttools.ToolOutputLimit {
	l = l.Normalize()
	return agenttools.ToolOutputLimit{
		MaxBytes: l.ToolOutputMaxBytes,
		MaxLines: l.ToolOutputMaxLines,
	}
}
