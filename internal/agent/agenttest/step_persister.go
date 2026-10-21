package agenttest

import (
	"context"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// RecordingStepPersister records the history boundary shared by native and
// subagent step tests, with an optional persistence failure.
type RecordingStepPersister struct {
	*RecordingMessages
	Steps              []messagepkg.AgentStep
	StepErr            error
	ReplacementRequest *messagepkg.Message
}

func (s *RecordingStepPersister) PersistAgentStep(_ context.Context, step messagepkg.AgentStep) ([]messagepkg.Message, error) {
	if s.StepErr != nil {
		return nil, s.StepErr
	}
	s.Steps = append(s.Steps, step)
	result := make([]messagepkg.Message, 0, len(step.Messages)+1)
	if step.Replacement != nil && step.Replacement.RequestMessageID != "" {
		request := messagepkg.Message{ID: "replacement-request", Role: "user", TurnID: step.Replacement.ReplacementTurnID}
		if s.ReplacementRequest != nil {
			request = *s.ReplacementRequest
		}
		result = append(result, request)
	}
	for _, input := range step.Messages {
		result = append(result, messagepkg.Message{ID: "committed", Role: input.Role, BotID: input.BotID, SessionID: input.SessionID, Metadata: input.Metadata, Content: input.Content})
	}
	return result, nil
}
