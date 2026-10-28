package application

import (
	"context"
	"fmt"
	"slices"
	"strings"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// historyContextFragsForMessages is the fragment of each summary the
// messages render, at the message that renders it.
func historyContextFragsForMessages(messages []ModelMessage, records []historyfrag.HistoryRecord) []contextfrag.ContextFrag {
	summaries := make(map[string]historyfrag.HistoryRecord)
	for _, record := range records {
		if record.Kind == contextfrag.KindConversationSummary {
			summaries[record.Ref.ID] = record
		}
	}
	var frags []contextfrag.ContextFrag
	for i, message := range messages {
		source := message.ContextSource
		if source == nil || source.Kind != "summary" {
			continue
		}
		record, ok := summaries[source.ID]
		if !ok {
			continue
		}
		frag := historyfrag.ToFrag(record)
		frag.ID = fmt.Sprintf("message.%03d", i)
		frag.Provenance.Index = i
		frags = append(frags, frag)
	}
	return frags
}

// historySourceMessageIDsForMessages names, for each message, the row it
// copies as stored: a fork snapshot can then reference the row instead of
// its bytes. A message that renders a row differently (a closure repaired
// from it, content sanitized) names none.
func historySourceMessageIDsForMessages(messages []ModelMessage, records []historyfrag.HistoryRecord) []string {
	rows := make(map[string]historyfrag.HistoryRecord, len(records))
	for _, record := range records {
		if id := strings.TrimSpace(record.DBMessageID); id != "" {
			rows[id] = record
		}
	}
	sourceIDs := make([]string, len(messages))
	for i, message := range messages {
		source := message.ContextSource
		if source == nil || source.Kind != "history" {
			continue
		}
		if record, ok := rows[source.ID]; ok && modelMessageSourceKey(record.ModelMessage) == modelMessageSourceKey(message) {
			sourceIDs[i] = source.ID
		}
	}
	return sourceIDs
}

// contextProvenance names what each message of a request renders, by
// position, for a run that compacts in place: the source the message
// carries, the row, inbound message or summary it was loaded from. A
// message without one is runtime context. A message that renders the
// request's own input is an anchor.
func contextProvenance(messages []ModelMessage, inputs []turn.ContextMessageSource) []native.MessageProvenance {
	out := make([]native.MessageProvenance, len(messages))
	for i, message := range messages {
		if message.ContextSource == nil || message.ContextSource.ID == "" {
			continue
		}
		source := *message.ContextSource
		out[i].Anchor = source.Current || slices.ContainsFunc(inputs, func(input turn.ContextMessageSource) bool {
			return input.Kind == source.Kind && input.ID == source.ID
		})
		source.Current = false
		out[i].Sources = []turn.ContextMessageSource{source}
	}
	return out
}

func modelMessageSourceKey(message ModelMessage) string {
	if hash := historyfrag.MessageContentHash(message); hash != "" {
		return hash
	}
	return strings.ToLower(strings.TrimSpace(message.Role)) + "\x00" + string(message.Content)
}

// historyCut is where an edit or retry replays history: the checkpoint its
// replacement keeps, and the history before its cutoff message. The zero
// historyCut is the session's history under its active checkpoint.
type historyCut struct {
	replacedTurnID string
	before         *messagepkg.HistoryPosition
}

// loadHistoryCut resolves the cutoff of an edit or retry; no cutoff message
// means no cut. A cutoff that does not resolve fails the request rather than
// replaying the wrong history (CM-ADM-003).
func (s *Service) loadHistoryCut(ctx context.Context, req ChatRequest) (historyCut, error) {
	messageID := strings.TrimSpace(req.HistoryCutoffBeforeMessageID)
	if messageID == "" || s == nil || s.messageService == nil || strings.TrimSpace(req.ThreadID) == "" {
		return historyCut{}, nil
	}
	position, err := s.messageService.HistoryPositionByID(ctx, req.ThreadID, messageID)
	if err != nil {
		return historyCut{}, fmt.Errorf("load history cutoff: %w", err)
	}
	cut := historyCut{before: &position}
	if req.TurnReplacement != nil {
		cut.replacedTurnID = strings.TrimSpace(req.TurnReplacement.OldTurnID)
	}
	return cut, nil
}
