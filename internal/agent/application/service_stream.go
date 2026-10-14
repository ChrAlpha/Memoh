package application

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/schedule"
)

// WSStreamEvent represents a raw JSON event forwarded from the agent.
type WSStreamEvent = json.RawMessage

// terminalSnapshot captures the partial state extracted from a terminal
// agent event. It is used both for the success-path persistence and for the
// interrupted-path fallback so that real partial messages get saved instead
// of a synthetic placeholder.
type terminalSnapshot struct {
	sdkMessages             []sdk.Message
	internalFeedbackIndexes []int
	usage                   json.RawMessage
	reasoningTiming         []messagepkg.ReasoningTimingSegment
	deferredToolID          string
	aborted                 bool
	visibleOutput           bool
	failureCode             apperror.Code
}

// historyFailureCode is code when a failed turn's history records it, and empty
// otherwise, for the turns snapshotFailureCode and a replacement turn leave to
// it.
func historyFailureCode(code apperror.Code) apperror.Code {
	switch code {
	case apperror.CodeAgentResponseTimeout,
		apperror.CodeAgentToolTimeout,
		apperror.CodeScheduleExecutionTimeout,
		apperror.CodeAgentResponseInterrupted,
		apperror.CodeAgentLoopDetected,
		apperror.CodeAgentProviderOverloaded,
		apperror.CodeAgentProviderRateLimited,
		apperror.CodeAgentProviderQuotaExhausted,
		apperror.CodeAgentProviderAuthFailed,
		apperror.CodeAgentProviderPermissionDenied,
		apperror.CodeAgentProviderRequestRejected,
		apperror.CodeAgentProviderUnreachable:
		return code
	default:
		return ""
	}
}

func classifyUserMessageHookError(err error) error {
	if err == nil || apperror.CodeOf(err) == apperror.CodeHookUserMessageFailed {
		return err
	}
	return apperror.Wrap(apperror.CodeHookUserMessageFailed, err, nil)
}

func hasVisibleAgentStreamOutput(event native.StreamEvent) bool {
	switch event.Type {
	case native.EventTextDelta,
		native.EventReasoningDelta:
		return strings.TrimSpace(event.Delta) != ""
	case native.EventToolCallInputStart,
		native.EventToolCallStart,
		native.EventToolCallProgress,
		native.EventToolCallEnd,
		native.EventToolApprovalRequest,
		native.EventUserInputRequest,
		native.EventReaction,
		native.EventSpeech:
		return true
	case native.EventAttachment:
		return len(event.Attachments) > 0
	default:
		return false
	}
}

// agentStreamFailure translates the failure an error event reports into the
// run's public error, keeping the event's cause, and returns nil for any other
// event. Every consumer of agent events uses it, so the run's terminal write,
// the history marker, the session's live view and the published event name a
// failure alike.
//
// An event that already carries a catalogued code keeps it and its args: the
// application and the External Agent runtimes name their failures themselves. A native
// failure is named from its cause by nativeFailureCode.
func agentStreamFailure(event native.StreamEvent) error {
	if event.Type != native.EventError {
		return nil
	}
	if code := apperror.Code(strings.TrimSpace(event.Code)); code != "" {
		if _, ok := apperror.Lookup(code); ok {
			return apperror.Wrap(code, event.Cause, event.Args)
		}
	}
	return apperror.Wrap(nativeFailureCode(event.Cause), event.Cause, nil)
}

// nativeFailureCode names the failure a native run ended with. A provider
// that named why it refused the request is
// reported with that reason: an exhausted balance and a rejected key both need
// the user to go change something, and "the model response was interrupted,
// please try again" sends them back into a call that cannot succeed. A model
// call that got no response reports the provider unreachable. Anything else
// is an interrupted response; the runtime names its own failures itself.
func nativeFailureCode(cause error) apperror.Code {
	if code := providerFailureCode(cause, native.IsModelCallFailure(cause)); code != "" {
		return code
	}
	return apperror.CodeAgentResponseInterrupted
}

func agentFailureStreamEvent(cause error) native.StreamEvent {
	code := classifyRunFailure(cause)
	definition, _ := apperror.Lookup(code)
	event := native.StreamEvent{
		Type:  native.EventError,
		Code:  string(code),
		Error: definition.Detail,
	}
	if apperror.CodeOf(cause) == code {
		if args := apperror.ArgsOf(cause); len(args) > 0 {
			event.Args = args
		}
	}
	return event
}

// publicAgentStreamEvent is event as it leaves the application: an error
// event is replaced by its code and the catalog detail, so its cause never
// reaches a subscriber.
func publicAgentStreamEvent(event native.StreamEvent) native.StreamEvent {
	if cause := agentStreamFailure(event); cause != nil {
		return agentFailureStreamEvent(cause)
	}
	return event
}

// agentAbortCause is the cause a run that ended without naming a failure is
// reported with: the context's own cause, or the driver's stop marker when
// something outside the run ended it. Returning the marker rather than a bare
// error is what keeps "someone stopped this" distinguishable from "this run
// failed for a reason nobody named".
func agentAbortCause(ctx context.Context) error {
	if ctx != nil {
		if cause := context.Cause(ctx); cause != nil {
			if errors.Is(cause, schedule.ErrExecutionTimeout) {
				return apperror.Wrap(apperror.CodeScheduleExecutionTimeout, cause, nil)
			}
			return cause
		}
	}
	return errRunStopped
}

// extractTerminalSnapshot decodes a terminal stream event payload into the
// raw SDK messages plus auxiliary metadata. Returns ok=false when the event
// has no usable messages.
func extractTerminalSnapshot(data []byte) (terminalSnapshot, bool) {
	var envelope struct {
		InternalFeedbackIndexes []int           `json:"internal_feedback_indexes"`
		Type                    string          `json:"type"`
		Messages                json.RawMessage `json:"messages"`
		Usage                   json.RawMessage `json:"usage,omitempty"`
		ApprovalID              string          `json:"approvalId,omitempty"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return terminalSnapshot{}, false
	}
	if len(envelope.Messages) == 0 {
		return terminalSnapshot{}, false
	}
	var sdkMsgs []sdk.Message
	if err := json.Unmarshal(envelope.Messages, &sdkMsgs); err != nil || len(sdkMsgs) == 0 {
		return terminalSnapshot{}, false
	}
	return terminalSnapshot{
		sdkMessages:             sdkMsgs,
		internalFeedbackIndexes: envelope.InternalFeedbackIndexes,
		usage:                   envelope.Usage,
		deferredToolID:          strings.TrimSpace(envelope.ApprovalID),
		aborted:                 envelope.Type == string(native.EventAgentAbort),
	}, true
}

func (s *Service) persistTerminalSnapshotResult(ctx context.Context, req ChatRequest, rc resolvedContext, snap terminalSnapshot) ([]messagepkg.Message, error) {
	outputMessages := sdkMessagesWithOrigins(snap.sdkMessages, snap.internalFeedbackIndexes)
	if snap.failureCode != "" && (!snap.visibleOutput || !hasPersistableAssistantOutput(outputMessages)) {
		return s.persistTurnFailure(ctx, req, rc, snap.failureCode)
	}
	if snap.aborted && !snap.visibleOutput {
		s.logger.InfoContext(ctx, "skip persisting aborted terminal snapshot before visible output",
			slog.String("bot_id", req.BotID),
			slog.String("chat_id", req.ChatID),
			slog.Int("messages", len(outputMessages)),
		)
		return nil, nil
	}
	if !hasPersistableAssistantOutput(outputMessages) {
		s.logger.InfoContext(ctx, "skip persisting terminal snapshot without assistant output",
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
	roundMessages := prependTurnUserMessage(storeReq, outputMessages)

	if rc.injectedRecords != nil && len(*rc.injectedRecords) > 0 {
		roundMessages = interleaveInjectedMessages(roundMessages, *rc.injectedRecords)
	}

	persisted, err := s.storeRoundWithOptionsResult(ctx, storeReq, roundMessages, rc.model.ID, storeRoundOptions{
		AllowPendingToolCalls:  snap.deferredToolID != "",
		RequireCompletePersist: true,
		ContextLifecycle:       rc.runConfig.ContextLifecycle,
		ReasoningTiming:        snap.reasoningTiming,
	})
	if err != nil {
		return nil, err
	}
	if len(persisted) > 0 {
		if err := s.persistSessionWorkspaceTarget(ctx, storeReq); err != nil {
			return nil, err
		}
		rc.pressure.add(snap.sdkMessages)
	}

	if inputTokens := extractInputTokensFromUsage(snap.usage); inputTokens > 0 {
		s.maybeCompact(context.WithoutCancel(ctx), req, rc, inputTokens)
	}

	return persisted, nil
}

func hasPersistableAssistantOutput(messages []ModelMessage) bool {
	for _, msg := range messages {
		if strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") && !isEmptyAssistantMessage(msg) {
			return true
		}
	}
	return false
}

func (s *Service) persistTurnFailure(ctx context.Context, req ChatRequest, rc resolvedContext, code apperror.Code) ([]messagepkg.Message, error) {
	if code == "" || req.SkipHistoryTurn {
		return nil, nil
	}
	storeReq := req
	if req.ReusePersistedUserMessage {
		storeReq.UserMessagePersisted = true
	}
	output := []ModelMessage{{
		Role:    "assistant",
		Content: newTextContent(""),
	}}
	round := output
	if !storeReq.UserMessagePersisted {
		round = prependTurnUserMessage(storeReq, output)
	}
	assistantIdx := lastAssistantMessageIndex(round)
	if assistantIdx < 0 {
		return nil, nil
	}
	modelID := ""
	if rc.model.ID != "" {
		modelID = rc.model.ID
	}
	persisted, err := s.storeRoundWithOptionsResult(ctx, storeReq, round, modelID, storeRoundOptions{
		AllowEmptyAssistantText: true,
		MessageMetadataByIndex: map[int]map[string]any{
			assistantIdx: {
				messagepkg.AgentStepInterruptedMetadataKey: true,
				messagepkg.HistoryErrorCodeMetadataKey:     string(code),
			},
		},
		ContextLifecycle: rc.runConfig.ContextLifecycle,
	})
	if err != nil {
		return nil, err
	}
	s.logger.InfoContext(ctx, "persisted turn-level stream failure",
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
func interleaveInjectedMessages(round []ModelMessage, injections []InjectedMessageRecord) []ModelMessage {
	if len(injections) == 0 {
		return round
	}
	result := make([]ModelMessage, 0, len(round)+len(injections))
	injIdx := 0
	for i, msg := range round {
		result = append(result, msg)
		for injIdx < len(injections) && injections[injIdx].InsertAfter == i {
			result = append(result, ModelMessage{
				Role:    "user",
				Content: newTextContent(injections[injIdx].HeaderifiedText),
			})
			injIdx++
		}
	}
	for ; injIdx < len(injections); injIdx++ {
		result = append(result, ModelMessage{
			Role:    "user",
			Content: newTextContent(injections[injIdx].HeaderifiedText),
		})
	}
	return result
}

func extractInputTokensFromUsage(raw json.RawMessage) int {
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
