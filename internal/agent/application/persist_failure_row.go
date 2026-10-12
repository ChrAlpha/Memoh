package application

import (
	"context"
	"log/slog"

	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// failureRowMetadata is the metadata every failed turn's rows carry. It is the
// one place a failure's code and args become history metadata, so a native
// step, a failure row and an external runtime round all record the same shape.
func failureRowMetadata(f RunFailure, origin string, metadata map[string]any) map[string]any {
	if metadata == nil {
		metadata = map[string]any{}
	}
	if f.Code != "" {
		metadata[messagepkg.HistoryErrorCodeMetadataKey] = string(f.Code)
	}
	if len(f.Args) > 0 {
		metadata[messagepkg.HistoryErrorArgsMetadataKey] = f.Args
	}
	if origin != "" {
		metadata[messagepkg.HistoryFailureOriginMetadataKey] = origin
	}
	return metadata
}

// persistFailureRow writes the user message and the empty assistant row that
// record a run whose failure left no assistant output. It is the only writer
// of a failure row: a run that had already committed steps keeps them and adds
// nothing, which is what keeps a failure from appearing to be a second turn.
func (s *Service) persistFailureRow(ctx context.Context, req ChatRequest, rc resolvedContext, f RunFailure, origin string) ([]messagepkg.Message, error) {
	if req.SkipHistoryTurn {
		return nil, nil
	}
	storeReq := req
	if req.ReusePersistedUserMessage {
		storeReq.UserMessagePersisted = true
	}
	output := []ModelMessage{{Role: "assistant", Content: newTextContent("")}}
	round := output
	if !storeReq.UserMessagePersisted {
		round = prependTurnUserMessage(storeReq, output)
	}
	assistantIdx := lastAssistantMessageIndex(round)
	if assistantIdx < 0 {
		return nil, nil
	}
	metadata := failureRowMetadata(f, origin, map[string]any{
		messagepkg.AgentStepInterruptedMetadataKey: true,
	})
	messageMetadata := map[int]map[string]any{assistantIdx: metadata}
	if origin != "" && !storeReq.UserMessagePersisted && len(round) > 1 {
		// The user row carries the same origin so history preparation can drop
		// the refused pair together.
		messageMetadata[0] = map[string]any{
			messagepkg.HistoryFailureOriginMetadataKey: origin,
		}
	}
	modelID := ""
	if rc.model.ID != "" {
		modelID = rc.model.ID
	}
	persisted, err := s.storeRoundWithOptionsResult(ctx, storeReq, round, modelID, storeRoundOptions{
		AllowEmptyAssistantText: true,
		MessageMetadataByIndex:  messageMetadata,
		ContextLifecycle:        rc.runConfig.ContextLifecycle,
		// A hook refusal hands the turn back to its submitter, so the user
		// message and the row explaining the refusal are one turn: writing only
		// half of it leaves a message with no answer and no explanation. The
		// atomic write needs the run's fence, which every admitted run carries.
		RequireCompletePersist: origin != "",
	})
	if err != nil {
		return nil, err
	}
	s.logger.InfoContext(ctx, "persisted run failure row",
		slog.String("bot_id", req.BotID),
		slog.String("chat_id", req.ChatID),
		slog.String("code", string(f.Code)),
		slog.String("origin", origin),
	)
	return persisted, nil
}

// failureRowCode is the code a failure row records, or empty when the failure
// leaves no row at all.
func failureRowCode(f *RunFailure) apperror.Code {
	if f == nil {
		return ""
	}
	return f.Code
}
