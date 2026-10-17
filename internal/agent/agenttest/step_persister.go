package agenttest

import (
	"context"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// RecordingStepPersister records the history boundary shared by native and
// subagent step tests, with an optional persistence failure.
type RecordingStepPersister struct {
	*RecordingMessages
	Steps   []messagepkg.AgentStep
	StepErr error
}

func (s *RecordingStepPersister) PersistAgentStep(_ context.Context, step messagepkg.AgentStep) ([]messagepkg.Message, error) {
	if s.StepErr != nil {
		return nil, s.StepErr
	}
	s.Steps = append(s.Steps, step)
	result := make([]messagepkg.Message, len(step.Messages))
	for i, input := range step.Messages {
		result[i] = messagepkg.Message{ID: "committed", Role: input.Role, BotID: input.BotID, SessionID: input.SessionID, Metadata: input.Metadata, Content: input.Content}
	}
	return result, nil
}

func (s *RecordingStepPersister) PersistAgentReplacementStep(ctx context.Context, step messagepkg.AgentStep) ([]messagepkg.Message, error) {
	return s.PersistAgentStep(ctx, step)
}

func (*RecordingStepPersister) FinalizeAgentReplacement(context.Context, string, messagepkg.TurnReplacement, string, string) error {
	return nil
}
