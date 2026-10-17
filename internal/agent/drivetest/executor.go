package drivetest

import (
	"context"

	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// ScriptedExecutor produces a fixed sequence of events and reports a fixed
// ending, so a driver test reads as the run's shape rather than as a runtime's.
type ScriptedExecutor struct {
	Events   []native.StreamEvent
	Terminal *native.StreamEvent
	Cause    error
	Phase    outcome.RunPhase
	Deferred bool
	// CommitErr, when set, is what the ending's history write returns.
	CommitErr error
	// CommitRows records the row flag each commit was called with.
	CommitRows *[]bool
	// StoppedWrites, when non-nil, gives the ending a CommitStopped hook that
	// counts its calls into it; nil models an executor that has nothing left to
	// write when its run is stopped.
	StoppedWrites *int
	// StoppedErr is what that hook returns.
	StoppedErr error
	// EmitErr, when set, is what Execute hands back as its cause.
	EmitErr error
	// BlockUntil, when non-nil, holds Execute open until it is closed.
	BlockUntil chan struct{}
}

func (e *ScriptedExecutor) Execute(ctx context.Context, emit func(native.StreamEvent)) drive.ExecEnding {
	if e.BlockUntil != nil {
		select {
		case <-e.BlockUntil:
		case <-ctx.Done():
		}
	}
	for _, ev := range e.Events {
		emit(ev)
	}
	ending := drive.ExecEnding{
		Terminal: e.Terminal,
		Cause:    e.Cause,
		Phase:    e.Phase,
		Deferred: e.Deferred,
	}
	if c := e.EmitErr; c != nil {
		ending.Cause = c
	}
	if ending.Phase == 0 {
		ending.Phase = outcome.PhaseExecution
	}
	ending.Commit = func(_ context.Context, _ *outcome.RunFailure, row bool) (drive.CommitSummary, error) {
		if e.CommitRows != nil {
			*e.CommitRows = append(*e.CommitRows, row)
		}
		return drive.CommitSummary{}, e.CommitErr
	}
	if e.StoppedWrites != nil {
		ending.CommitStopped = func(context.Context) (drive.CommitSummary, error) {
			*e.StoppedWrites++
			return drive.CommitSummary{Text: "kept"}, e.StoppedErr
		}
	}
	return ending
}
