package agenttest

import (
	"context"
	"errors"
	"time"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// RecordingMessages is a message-service double that records persisted rows.
type RecordingMessages struct {
	Persisted               []messagepkg.PersistInput
	persistErr              error
	RoundPersistErr         error
	RoundOptions            []messagepkg.RoundPersistenceOptions
	Replaced                int
	ReplacementTurnID       string
	ReplacementTurnPosition *int64
	Deleted                 [][]string
	// RunRequest is the request row GetRunTurnRequest finds; nil finds none.
	RunRequest *messagepkg.RunTurnRequest
	// InterruptedFailures are the failures named onto interrupted rows;
	// InterruptedFailureErr, when set, refuses them.
	InterruptedFailures   []messagepkg.InterruptedStepFailure
	InterruptedFailureErr error
}

// RecordInterruptedStepFailure records the failure a run named on its
// checkpoint rows.
func (s *RecordingMessages) RecordInterruptedStepFailure(_ context.Context, failure messagepkg.InterruptedStepFailure) error {
	if s.InterruptedFailureErr != nil {
		return s.InterruptedFailureErr
	}
	s.InterruptedFailures = append(s.InterruptedFailures, failure)
	return nil
}

func (s *RecordingMessages) Persist(_ context.Context, input messagepkg.PersistInput) (messagepkg.Message, error) {
	if s.persistErr != nil {
		return messagepkg.Message{}, s.persistErr
	}
	s.Persisted = append(s.Persisted, input)
	return messagepkg.Message{ID: "message-id", SessionID: input.SessionID, Role: input.Role, Content: input.Content, DisplayContent: input.DisplayText}, nil
}

func (s *RecordingMessages) PersistRound(_ context.Context, inputs []messagepkg.PersistInput, options messagepkg.RoundPersistenceOptions) ([]messagepkg.Message, bool, error) {
	s.RoundOptions = append(s.RoundOptions, options)
	if s.RoundPersistErr != nil {
		return nil, true, s.RoundPersistErr
	}
	if s.persistErr != nil {
		return nil, true, s.persistErr
	}
	persisted := make([]messagepkg.Message, 0, len(inputs))
	if options.Replacement != nil && options.Replacement.RequestMessageID != "" {
		persisted = append(persisted, messagepkg.Message{ID: "cloned-request", Role: "user"})
	}
	for _, input := range inputs {
		s.Persisted = append(s.Persisted, input)
		persisted = append(persisted, messagepkg.Message{
			ID:             "message-id",
			SessionID:      input.SessionID,
			Role:           input.Role,
			Content:        input.Content,
			DisplayContent: input.DisplayText,
		})
	}
	return persisted, true, nil
}

// PersistAgentStep records the rows of a committed step like those of any other
// write, so a run's history reads from Persisted.
func (s *RecordingMessages) PersistAgentStep(_ context.Context, step messagepkg.AgentStep) ([]messagepkg.Message, error) {
	if s.RoundPersistErr != nil {
		return nil, s.RoundPersistErr
	}
	if s.persistErr != nil {
		return nil, s.persistErr
	}
	persisted := make([]messagepkg.Message, 0, len(step.Messages)+1)
	if step.Replacement != nil && step.Replacement.RequestMessageID != "" {
		persisted = append(persisted, messagepkg.Message{ID: "cloned-request", Role: "user"})
	}
	for _, input := range step.Messages {
		s.Persisted = append(s.Persisted, input)
		persisted = append(persisted, messagepkg.Message{
			ID:             "message-id",
			SessionID:      input.SessionID,
			Role:           input.Role,
			Content:        input.Content,
			DisplayContent: input.DisplayText,
		})
	}
	return persisted, nil
}

func (*RecordingMessages) List(context.Context, string) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*RecordingMessages) ListSince(context.Context, string, time.Time) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*RecordingMessages) ListActiveSince(context.Context, string, time.Time) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*RecordingMessages) ListLatest(context.Context, string, int32) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*RecordingMessages) ListBefore(context.Context, string, time.Time, int32) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*RecordingMessages) ListBySession(context.Context, string) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*RecordingMessages) ListSinceBySession(context.Context, string, time.Time) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*RecordingMessages) ListActiveSinceBySession(context.Context, string, time.Time) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*RecordingMessages) ListActiveAfterCompactionBySessionWithinBytes(context.Context, string, int64) (messagepkg.SessionHistoryAfterCompaction, error) {
	return messagepkg.SessionHistoryAfterCompaction{}, nil
}

func (*RecordingMessages) ListRecentUserBySession(context.Context, string, int) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*RecordingMessages) ListTurnResponseSourcesSinceBySessionWithinBytes(context.Context, string, time.Time, int64) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*RecordingMessages) MeasureActiveBySession(context.Context, string, time.Time) (messagepkg.ActiveMessagesMeasure, error) {
	return messagepkg.ActiveMessagesMeasure{}, nil
}

func (*RecordingMessages) ListLatestBySession(context.Context, string, int32) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*RecordingMessages) ListBeforeBySession(context.Context, string, time.Time, int32) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*RecordingMessages) ListBeforeMessageBySession(context.Context, string, string, int32) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*RecordingMessages) LocateByExternalIDBySession(context.Context, string, string, int32, int32) (messagepkg.LocateResult, error) {
	return messagepkg.LocateResult{}, nil
}

func (s *RecordingMessages) GetByIDBySession(_ context.Context, sessionID, messageID string) (messagepkg.Message, error) {
	for _, input := range s.Persisted {
		if input.SessionID == sessionID && input.Role == "user" {
			return messagepkg.Message{
				ID: messageID, SessionID: sessionID, Role: input.Role,
				Content: input.Content, DisplayContent: input.DisplayText,
			}, nil
		}
	}
	return messagepkg.Message{}, errors.New("message not found")
}

func (*RecordingMessages) ListVisibleFromBySession(context.Context, string, string) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*RecordingMessages) ListVisibleFromForModelBySession(context.Context, string, string) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*RecordingMessages) GetVisibleTurnByMessage(context.Context, string, string) (messagepkg.HistoryTurn, error) {
	return messagepkg.HistoryTurn{}, nil
}

func (*RecordingMessages) GetLatestVisibleTurnBySession(context.Context, string) (messagepkg.HistoryTurn, error) {
	return messagepkg.HistoryTurn{}, nil
}

func (s *RecordingMessages) ReplaceTurn(_ context.Context, _, _, replacementTurnID string, replacementTurnPosition *int64, _, _, _ string) (messagepkg.HistoryTurn, error) {
	s.Replaced++
	s.ReplacementTurnID = replacementTurnID
	s.ReplacementTurnPosition = replacementTurnPosition
	return messagepkg.HistoryTurn{}, nil
}

func (s *RecordingMessages) DeleteByIDs(_ context.Context, ids []string) error {
	s.Deleted = append(s.Deleted, append([]string(nil), ids...))
	return nil
}

func (*RecordingMessages) DeleteByBot(context.Context, string) error {
	return nil
}

func (*RecordingMessages) DeleteBySession(context.Context, string) error {
	return nil
}

func (*RecordingMessages) LinkAssets(context.Context, string, []messagepkg.AssetRef) error {
	return nil
}

// GetRunTurnRequest finds RunRequest, or reports no request row.
func (s *RecordingMessages) GetRunTurnRequest(context.Context, string, string, string) (messagepkg.RunTurnRequest, error) {
	if s.RunRequest != nil {
		return *s.RunRequest, nil
	}
	return messagepkg.RunTurnRequest{}, messagepkg.ErrRunRequestNotFound
}
