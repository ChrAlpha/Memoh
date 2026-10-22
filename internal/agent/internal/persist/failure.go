package persist

import (
	"context"
	"log/slog"

	"github.com/felinics/memoh/internal/agent/internal/outcome"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// FailureRowMetadata is the metadata every failed turn's rows carry. It is the
// one place a failure's code and args become history metadata, so a native
// step, a failure row and an external runtime round all record the same shape.
func FailureRowMetadata(f outcome.RunFailure, origin string, metadata map[string]any) map[string]any {
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

// interruptedRowMetadata is the metadata of an assistant row a failure cut
// short: the interrupted mark plus the failure's code and args. A native step
// and the turn's failure row both record a failure through it, so they read the
// same in history.
func interruptedRowMetadata(f outcome.RunFailure, origin string, metadata map[string]any) map[string]any {
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata[messagepkg.AgentStepInterruptedMetadataKey] = true
	return FailureRowMetadata(f, origin, metadata)
}

// FailureRow writes the user message and the empty assistant row that
// record a run whose failure left no assistant output. It is the only writer
// of a failure row for a turn without step commits: a run that had already
// committed steps records its failure as an interrupted step instead
// (StepCommitter.RecordFailure), which is what keeps a failure from appearing
// to be a second turn.
func (w *Writer) FailureRow(ctx context.Context, req runreq.ChatRequest, rc runreq.ResolvedContext, f outcome.RunFailure, origin string) ([]messagepkg.Message, error) {
	storeReq := req
	if req.ReusePersistedUserMessage {
		storeReq.UserMessagePersisted = true
	}
	output := []runreq.ModelMessage{{Role: "assistant", Content: runreq.NewTextContent("")}}
	round := output
	if !storeReq.UserMessagePersisted {
		round = runreq.PrependTurnUserMessage(storeReq, output)
	}
	assistantIdx := lastAssistantMessageIndex(round)
	if assistantIdx < 0 {
		return nil, nil
	}
	messageMetadata := map[int]map[string]any{assistantIdx: interruptedRowMetadata(f, origin, nil)}
	if origin != "" && !storeReq.UserMessagePersisted && len(round) > 1 {
		// The user row carries the same origin so history preparation can drop
		// the refused pair together.
		messageMetadata[0] = map[string]any{
			messagepkg.HistoryFailureOriginMetadataKey: origin,
		}
	}
	modelID := ""
	if rc.Model.ID != "" {
		modelID = rc.Model.ID
	}
	persisted, err := w.StoreRoundResult(ctx, storeReq, round, modelID, RoundOptions{
		AllowEmptyAssistantText: true,
		MessageMetadataByIndex:  messageMetadata,
		ContextLifecycle:        rc.RunConfig.ContextLifecycle,
		// A hook refusal hands the turn back to its submitter, so the user
		// message and the row explaining the refusal are one turn: writing only
		// half of it leaves a message with no answer and no explanation. The
		// atomic write needs the run's fence, which every admitted run carries.
		RequireCompletePersist: origin != "",
	})
	if err != nil {
		return nil, err
	}
	w.logger.InfoContext(ctx, "persisted run failure row",
		slog.String("bot_id", req.BotID),
		slog.String("chat_id", req.ChatID),
		slog.String("code", string(f.Code)),
		slog.String("origin", origin),
	)
	return persisted, nil
}
