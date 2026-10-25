package application

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/chat/timeline"
)

// buildMessagesFromPipeline assembles chat context from the DCP pipeline's
// RenderedContext (RC) merged with assistant/tool turns (TR) from
// bot_history_messages. This gives chat mode the same event-driven context
// that discuss mode uses, replacing the legacy loadMessages path. Both
// streams compose under the session's history checkpoint, once it has
// stepped over the history from before checkpoints that the request cannot
// render: the notice of what it omits, what it holds, then what came after
// it. The second return value is the token pressure of what composes. A
// history or checkpoint load failure fails the request (CM-ADM-003).
func (s *Service) buildMessagesFromPipeline(ctx context.Context, req ChatRequest, contextTokenBudget int) ([]ModelMessage, int, error) {
	sessionID := strings.TrimSpace(req.ThreadID)
	if s.pipeline == nil || sessionID == "" {
		return nil, 0, nil
	}
	rc := s.pipeline.GetRC(sessionID)
	if len(rc) == 0 {
		return nil, 0, nil
	}

	if err := s.stepOverHistory(ctx, sessionID, nil, contextTokenBudget); err != nil {
		return nil, 0, err
	}
	var held timeline.HistoryCheckpoint
	if s.pipelineCheckpoints != nil {
		var err error
		if held, err = s.pipelineCheckpoints.Load(ctx, sessionID, rc); err != nil {
			return nil, 0, err
		}
	}
	trs, err := s.loadTurnResponses(ctx, sessionID, held.Position, contextTokenBudget)
	if err != nil {
		return nil, 0, fmt.Errorf("load turn history: %w", err)
	}

	composed := timeline.ComposeContextWithCheckpoint(rc, trs, held)
	if composed == nil {
		return nil, 0, nil
	}

	messages := make([]ModelMessage, 0, len(composed.Messages))
	pressure := 0
	for _, m := range composed.Messages {
		if m.Source != nil && req.ExternalMessageID != "" {
			m.Source.Current = m.Source.Kind == "external" && m.Source.ID == req.ExternalMessageID
		}
		if m.CompactionArtifactID != "" {
			m.Source = &turn.ContextMessageSource{Kind: "summary", ID: m.CompactionArtifactID}
		}
		contentJSON := m.RawContent
		if len(contentJSON) == 0 {
			var err error
			contentJSON, err = json.Marshal(m.Content)
			if err != nil {
				continue
			}
		}
		messages = append(messages, ModelMessage{
			Role:          m.Role,
			ContextSource: m.Source,
			Content:       contentJSON,
		})
		pressure += estimateMessageTokens(messages[len(messages)-1])
	}

	return messages, pressure, nil
}

// loadTurnResponses loads the assistant/tool messages of the thread's
// history after its checkpoint's position, for use as the TR stream in
// pipeline-based context assembly; the checkpoint stands for the older ones.
// The load is byte-budgeted on the database side (CM-ADM-001): rows are
// admitted newest-first within the context budget and projected to content,
// so a long history, or legacy lifecycle metadata on its rows, can never
// materialize more than the budget's worth of payload.
func (s *Service) loadTurnResponses(ctx context.Context, sessionID string, after messagepkg.HistoryPosition, contextTokenBudget int) ([]timeline.TurnResponseEntry, error) {
	if s.messageService == nil || strings.TrimSpace(sessionID) == "" {
		return nil, nil
	}
	msgs, err := s.messageService.ListTurnResponseSourcesWithinBytes(ctx, sessionID, after, s.historyLoadMaxBytes(contextTokenBudget))
	if err != nil {
		return nil, err
	}
	entries := timeline.DecodeTurnResponseEntries(msgs)
	stored := make(map[string]messagepkg.Message, len(msgs))
	for _, msg := range msgs {
		stored[msg.ID] = msg
	}
	for i := range entries {
		if msg, ok := stored[entries[i].SourceMessageID]; ok {
			entries[i].ContentHash = historyfrag.MessageContentHash(historyfrag.DecodeStoredModelMessage(nil, msg.ID, msg.Role, msg.Content))
		}
	}
	return entries, nil
}

// historyLoadMaxBytes converts a token budget into the byte budget used for
// database-side history admission; a missing budget falls back to the
// absolute cap so no load path is ever unbounded.
func (s *Service) historyLoadMaxBytes(contextTokenBudget int) int64 {
	if contextTokenBudget <= 0 {
		contextTokenBudget = s.contextAbsoluteMaxTokens()
	}
	return int64(contextTokenBudget) * contextfrag.EstimateBytesPerToken
}

// discussHistoryLoadMaxBytes is the content a Discuss composition loads after
// the checkpoint: its turn responses within the admission cap, as the Discuss
// driver reads them, whatever the model's window.
func (s *Service) discussHistoryLoadMaxBytes() int64 {
	return s.historyLoadMaxBytes(s.contextAbsoluteMaxTokens())
}
