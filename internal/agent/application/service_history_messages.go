package application

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/chat/timeline"
)

// buildMessagesFromPipeline assembles chat context from the DCP pipeline's
// RenderedContext (RC) merged with assistant/tool turns (TR) from
// bot_history_messages. This gives chat mode the same event-driven context
// that discuss mode uses, replacing the legacy loadMessages path. The second
// return value is the token pressure of raw history and active summaries;
// the third is the creation time of the oldest loaded row.
func (s *Service) buildMessagesFromPipeline(ctx context.Context, req ChatRequest, contextTokenBudget int) ([]ModelMessage, int, time.Time) {
	sessionID := strings.TrimSpace(req.ThreadID)
	if s.pipeline == nil || sessionID == "" {
		return nil, 0, time.Time{}
	}
	rc := s.pipeline.GetRC(sessionID)
	if len(rc) == 0 {
		return nil, 0, time.Time{}
	}

	trs := s.loadTurnResponses(ctx, sessionID, contextTokenBudget)
	artifacts := s.loadTimelineArtifacts(ctx, req.BotID, sessionID)

	composed := timeline.ComposeContextWithArtifacts(rc, trs, artifacts)
	if composed == nil {
		return nil, 0, time.Time{}
	}

	messages := make([]ModelMessage, 0, len(composed.Messages))
	pressure := 0
	for _, m := range composed.Messages {
		if m.Source != nil && req.ExternalMessageID != "" {
			m.Source.Current = m.Source.Kind == "external" && m.Source.ID == req.ExternalMessageID
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

	return messages, pressure, time.UnixMilli(timeline.ViewFromMs(rc, trs)).UTC()
}

// loadTimelineArtifacts projects the session's active compaction frontier for
// timeline composition. A load failure degrades into the bounded admission
// window — the pipeline trim below still caps the recomposed raw history —
// and is surfaced with a stable key instead of passing silently (CM-ADM-003).
func (s *Service) loadTimelineArtifacts(ctx context.Context, botID, sessionID string) []timeline.CompactionArtifact {
	if s.queries == nil {
		return nil
	}
	artifacts, err := compaction.NewTimelineArtifactSource(s.queries).ActiveCompactionArtifacts(ctx, botID, sessionID)
	if err != nil {
		s.logger.WarnContext(ctx, "context_admission_degraded",
			slog.String("path", "pipeline_chat"),
			slog.String("reason", "artifact_load_failed"),
			slog.String("session_id", sessionID),
			slog.Any("error", err))
		return nil
	}
	return artifacts
}

// loadTurnResponses loads recent assistant/tool messages from bot_history_messages
// for use as the TR stream in pipeline-based context assembly. The load is
// byte-budgeted on the database side (CM-ADM-001): rows are admitted
// newest-first within the context budget and projected to content, so a
// dense 24-hour window, or legacy lifecycle metadata on its rows, can never
// materialize more than the budget's worth of payload.
func (s *Service) loadTurnResponses(ctx context.Context, sessionID string, contextTokenBudget int) []timeline.TurnResponseEntry {
	if s.messageService == nil {
		return nil
	}
	since := time.Now().UTC().Add(-24 * time.Hour)
	msgs, err := s.messageService.ListTurnResponseSourcesSinceBySessionWithinBytes(ctx, sessionID, since, s.historyLoadMaxBytes(contextTokenBudget))
	if err != nil {
		s.logger.WarnContext(ctx, "load TRs failed", slog.String("session_id", sessionID), slog.Any("error", err))
		return nil
	}
	return timeline.DecodeTurnResponseEntries(msgs)
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
