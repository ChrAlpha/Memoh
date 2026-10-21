package persist

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// TerminalSnapshot captures the partial state extracted from a terminal
// agent event. It is used both for the success-path persistence and for the
// interrupted-path fallback so that real partial messages get saved instead
// of a synthetic placeholder.
type TerminalSnapshot struct {
	SDKMessages             []sdk.Message
	internalFeedbackIndexes []int
	Usage                   json.RawMessage
	ReasoningTiming         []messagepkg.ReasoningTimingSegment
	DeferredToolID          string
	aborted                 bool
	VisibleOutput           bool
	FailureCode             apperror.Code
}

// ExtractTerminalSnapshot decodes a terminal stream event payload into the
// raw SDK messages plus auxiliary metadata. Returns ok=false when the event
// has no usable messages.
func ExtractTerminalSnapshot(data []byte) (TerminalSnapshot, bool) {
	var envelope struct {
		InternalFeedbackIndexes []int           `json:"internal_feedback_indexes"`
		Type                    string          `json:"type"`
		Messages                json.RawMessage `json:"messages"`
		Usage                   json.RawMessage `json:"usage,omitempty"`
		ApprovalID              string          `json:"approvalId,omitempty"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return TerminalSnapshot{}, false
	}
	if len(envelope.Messages) == 0 {
		return TerminalSnapshot{}, false
	}
	var sdkMsgs []sdk.Message
	if err := json.Unmarshal(envelope.Messages, &sdkMsgs); err != nil || len(sdkMsgs) == 0 {
		return TerminalSnapshot{}, false
	}
	return TerminalSnapshot{
		SDKMessages:             sdkMsgs,
		internalFeedbackIndexes: envelope.InternalFeedbackIndexes,
		Usage:                   envelope.Usage,
		DeferredToolID:          strings.TrimSpace(envelope.ApprovalID),
		aborted:                 envelope.Type == string(native.EventAgentAbort),
	}, true
}

func (w *Writer) TerminalSnapshotResult(ctx context.Context, req runreq.ChatRequest, rc runreq.ResolvedContext, snap TerminalSnapshot) ([]messagepkg.Message, error) {
	outputMessages := runreq.SDKMessagesWithOrigins(snap.SDKMessages, snap.internalFeedbackIndexes)
	if snap.FailureCode != "" && (!snap.VisibleOutput || !hasPersistableAssistantOutput(outputMessages)) {
		return w.persistTurnFailure(ctx, req, rc, snap.FailureCode)
	}
	if snap.aborted && !snap.VisibleOutput {
		w.logger.InfoContext(ctx, "skip persisting aborted terminal snapshot before visible output",
			slog.String("bot_id", req.BotID),
			slog.String("chat_id", req.ChatID),
			slog.Int("messages", len(outputMessages)),
		)
		return nil, nil
	}
	if !hasPersistableAssistantOutput(outputMessages) {
		w.logger.InfoContext(ctx, "skip persisting terminal snapshot without assistant output",
			slog.String("bot_id", req.BotID),
			slog.String("chat_id", req.ChatID),
			slog.Int("messages", len(outputMessages)),
		)
		return nil, nil
	}

	storeReq := req
	if req.ReusePersistedUserMessage {
		storeReq.UserMessagePersisted = true
	}
	roundMessages := runreq.PrependTurnUserMessage(storeReq, outputMessages)

	if rc.InjectedRecords != nil && len(*rc.InjectedRecords) > 0 {
		roundMessages = interleaveInjectedMessages(roundMessages, *rc.InjectedRecords)
	}

	persisted, err := w.StoreRoundResult(ctx, storeReq, roundMessages, rc.Model.ID, RoundOptions{
		AllowPendingToolCalls:  snap.DeferredToolID != "",
		RequireCompletePersist: true,
		ContextLifecycle:       rc.RunConfig.ContextLifecycle,
		ReasoningTiming:        snap.ReasoningTiming,
	})
	if err != nil {
		return nil, err
	}
	if len(persisted) > 0 {
		if err := w.persistSessionWorkspaceTarget(ctx, storeReq); err != nil {
			return nil, err
		}
		rc.Pressure.Add(snap.SDKMessages)
	}

	if inputTokens := ExtractInputTokensFromUsage(snap.Usage); inputTokens > 0 {
		w.commits.RunCommitted(context.WithoutCancel(ctx), req, rc, inputTokens)
	}

	return persisted, nil
}

func hasPersistableAssistantOutput(messages []runreq.ModelMessage) bool {
	for _, msg := range messages {
		if strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") && !runreq.IsEmptyAssistantMessage(msg) {
			return true
		}
	}
	return false
}

func (w *Writer) persistTurnFailure(ctx context.Context, req runreq.ChatRequest, rc runreq.ResolvedContext, code apperror.Code) ([]messagepkg.Message, error) {
	if code == "" {
		return nil, nil
	}
	storeReq := req
	if req.ReusePersistedUserMessage {
		storeReq.UserMessagePersisted = true
	}
	output := []runreq.ModelMessage{{
		Role:    "assistant",
		Content: runreq.NewTextContent(""),
	}}
	round := output
	if !storeReq.UserMessagePersisted {
		round = runreq.PrependTurnUserMessage(storeReq, output)
	}
	assistantIdx := lastAssistantMessageIndex(round)
	if assistantIdx < 0 {
		return nil, nil
	}
	modelID := ""
	if rc.Model.ID != "" {
		modelID = rc.Model.ID
	}
	persisted, err := w.StoreRoundResult(ctx, storeReq, round, modelID, RoundOptions{
		AllowEmptyAssistantText: true,
		MessageMetadataByIndex: map[int]map[string]any{
			assistantIdx: {
				messagepkg.AgentStepInterruptedMetadataKey: true,
				messagepkg.HistoryErrorCodeMetadataKey:     string(code),
			},
		},
		ContextLifecycle: rc.RunConfig.ContextLifecycle,
	})
	if err != nil {
		return nil, err
	}
	w.logger.InfoContext(ctx, "persisted turn-level stream failure",
		slog.String("bot_id", req.BotID),
		slog.String("chat_id", req.ChatID),
		slog.String("code", string(code)),
	)
	return persisted, nil
}

// interleaveInjectedMessages inserts injected user messages at their correct
// positions within the round. Each record's InsertAfter value indicates how
// many output messages preceded the injection.
//
// round layout: [user_A, output_0, output_1, ..., output_N]
// InsertAfter=K → insert after round[K] (i.e. after the K-th output message).
func interleaveInjectedMessages(round []runreq.ModelMessage, injections []runreq.InjectedMessageRecord) []runreq.ModelMessage {
	if len(injections) == 0 {
		return round
	}
	result := make([]runreq.ModelMessage, 0, len(round)+len(injections))
	injIdx := 0
	for i, msg := range round {
		result = append(result, msg)
		for injIdx < len(injections) && injections[injIdx].InsertAfter == i {
			result = append(result, runreq.ModelMessage{
				Role:    "user",
				Content: runreq.NewTextContent(injections[injIdx].HeaderifiedText),
			})
			injIdx++
		}
	}
	for ; injIdx < len(injections); injIdx++ {
		result = append(result, runreq.ModelMessage{
			Role:    "user",
			Content: runreq.NewTextContent(injections[injIdx].HeaderifiedText),
		})
	}
	return result
}

func ExtractInputTokensFromUsage(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var u struct {
		InputTokens int `json:"inputTokens"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return 0
	}
	return u.InputTokens
}
