package agent

import (
	"context"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/step"
	"github.com/felinics/memoh/internal/agent/subagent"
	tools "github.com/felinics/memoh/internal/agent/tool"
)

// The spawned-agent ports the tool layer is wired to are the subagent host's.

// AdmitSubagentRun admits a spawned agent's turn.
func (s *Service) AdmitSubagentRun(ctx context.Context, botID, threadID, invocationID string, submission []byte) (context.Context, tools.SubagentAdmission, func(tools.SubagentTerminal), error) {
	return s.subagent.Admit(ctx, botID, threadID, invocationID, submission)
}

// RecordSubagentResume records what resumes a spawned agent's run.
func (s *Service) RecordSubagentResume(ctx context.Context, session tools.SessionContext, sessionID, query, model string) error {
	return s.subagent.RecordResume(ctx, session, sessionID, query, model)
}

// SubagentStepCommit returns the per-step persistence of a spawned agent's run.
func (s *Service) SubagentStepCommit(ctx context.Context, botID, sessionID, modelID, turnRequestMessageID string, holder *contextfrag.LifecycleHolder, onPersisted func()) (func(context.Context, int, *step.Record) error, func(context.Context, int, *step.Record) error) {
	return s.subagent.StepCommit(ctx, botID, sessionID, modelID, turnRequestMessageID, holder, onPersisted)
}

// SubagentRunObserver returns the publisher of a spawned agent's run events.
func (s *Service) SubagentRunObserver(ctx context.Context) native.SpawnRunObserver {
	return s.subagent.Observer(ctx)
}

// SubagentStepCompactor returns the between-calls compactor of a spawned run.
func (s *Service) SubagentStepCompactor(ctx context.Context, run native.SpawnCompaction) native.StepContextCompactor {
	return s.subagent.StepCompactor(ctx, run)
}

// SubagentFailure is the error a spawned attempt that ended with event reports.
func (*Service) SubagentFailure(event native.StreamEvent) error {
	return subagent.Failure(event)
}
