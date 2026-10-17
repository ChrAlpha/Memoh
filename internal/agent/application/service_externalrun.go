package application

import (
	"context"

	"github.com/felinics/memoh/internal/agent/externalrun"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/turn"
)

// The runtime controls and the session operations on an external runtime are
// the external run host's.

// RuntimeSessionExecutionInfo reports whether a session executes in the
// workspace and who owns its runtime.
func (s *Service) RuntimeSessionExecutionInfo(ctx context.Context, sessionID string) (externalrun.SessionExecutionInfo, error) {
	return s.externalrun.SessionExecutionInfo(ctx, sessionID)
}

// PrepareExternalFork forks the runtime-side conversation behind an external
// session and returns the runtime metadata the forked session carries.
func (s *Service) PrepareExternalFork(ctx context.Context, botID, sessionID, turnID string) (map[string]any, error) {
	return s.externalrun.PrepareFork(ctx, botID, sessionID, turnID)
}

// AbortSessionRuns interrupts the session's active run and waits until it stops.
func (s *Service) AbortSessionRuns(ctx context.Context, botID, sessionID string) error {
	return s.externalrun.AbortSessionRuns(ctx, botID, sessionID)
}

// AbortRuntimeRun routes an abort through the durable runtime.
func (s *Service) AbortRuntimeRun(ctx context.Context, botID, sessionID, runID, controlID string) (bool, error) {
	return s.externalrun.AbortRuntimeRun(ctx, botID, sessionID, runID, controlID)
}

// PatchSessionModelPreference handles the model picker PATCH.
func (s *Service) PatchSessionModelPreference(ctx context.Context, botID, sessionID string, modelRef, effort *string, expectedRevision string) error {
	return s.externalrun.PatchSessionModelPreference(ctx, botID, sessionID, modelRef, effort, expectedRevision)
}

// RuntimeModel reports the model a direct runtime session uses next turn.
func (s *Service) RuntimeModel(ctx context.Context, request turn.RuntimeControlRequest) (turn.RuntimeModelState, error) {
	return s.externalrun.RuntimeModel(ctx, request)
}

// SetRuntimeModel remembers a model and effort for a direct runtime session.
func (s *Service) SetRuntimeModel(ctx context.Context, request turn.RuntimeControlRequest) (turn.RuntimeModelState, error) {
	return s.externalrun.SetRuntimeModel(ctx, request)
}

// RuntimeControls reads the controls a runtime session offers.
func (s *Service) RuntimeControls(ctx context.Context, request turn.RuntimeControlRequest) (external.Controls, error) {
	return s.externalrun.RuntimeControls(ctx, request)
}

// RuntimeCommands reads the commands a runtime session declares.
func (s *Service) RuntimeCommands(ctx context.Context, request turn.RuntimeControlRequest) ([]external.Command, error) {
	return s.externalrun.RuntimeCommands(ctx, request)
}

// RuntimeGoal reads the goal of a runtime session.
func (s *Service) RuntimeGoal(ctx context.Context, request turn.RuntimeControlRequest) (*external.Goal, error) {
	return s.externalrun.RuntimeGoal(ctx, request)
}

// SetRuntimeMode changes the mode of a runtime session.
func (s *Service) SetRuntimeMode(ctx context.Context, request turn.RuntimeControlRequest) (external.ModeState, error) {
	return s.externalrun.SetRuntimeMode(ctx, request)
}

// ExecuteRuntimeCommand handles a command that does not create a turn.
func (s *Service) ExecuteRuntimeCommand(ctx context.Context, request turn.RuntimeControlRequest) (turn.RuntimeCommandResult, error) {
	return s.externalrun.ExecuteRuntimeCommand(ctx, request)
}

// ControlRuntimeGoal pauses or clears the goal of a runtime session.
func (s *Service) ControlRuntimeGoal(ctx context.Context, request turn.RuntimeControlRequest, action string) error {
	return s.externalrun.ControlRuntimeGoal(ctx, request, action)
}
