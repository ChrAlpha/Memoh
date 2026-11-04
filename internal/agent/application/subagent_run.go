package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/step"
	tools "github.com/felinics/memoh/internal/agent/tool"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/runtimefence"
)

// BindSubagentRun arms a spawned agent's run the way every admitted native run
// is armed, from the request a direct turn on the agent's thread makes: the
// run renders the thread's history under its checkpoint, the rows a fork
// inherited included, and answers the task's request row, which it keeps as
// its anchor. Its steps persist as they commit, and a request that does not
// fit compacts in place, the first one included. The parent only hands over a
// task: the thread's own rows and checkpoint are the run's context, and a fold
// moves only the thread's checkpoint.
//
// The watchdog that bounds the run's silence pauses while the history loads,
// which can fold the rows a load leaves out, and while the run compacts:
// neither streams an event to the run. The summarizer's own stream is cut off
// after the same silence.
func (s *Service) BindSubagentRun(ctx context.Context, task tools.SpawnRunConfig, cfg *native.RunConfig, watchdog *tools.SubagentWatchdog) (func(context.Context) error, error) {
	handle, admitted := SubagentRunHandleFromContext(ctx)
	fence, fenced := runtimefence.FromContext(ctx)
	switch {
	case !admitted || !fenced || handle.FencingToken <= 0:
		return nil, errors.New("subagent run is not admitted")
	case handle.RunID != task.RunID || handle.BotID != task.Identity.BotID || handle.SessionID != task.Identity.SessionID ||
		fence.BotID != handle.BotID || fence.SessionID != handle.SessionID || fence.Token != handle.FencingToken:
		return nil, errors.New("subagent run is not the admitted one")
	}
	requestID := strings.TrimSpace(task.TurnRequestMessageID)
	if requestID == "" {
		return nil, errors.New("subagent task has no persisted request")
	}
	req := subagentRunRequest(handle, task, requestID)
	budget := s.contextAbsoluteMaxTokens()
	if task.ContextBudgetMaxTokens > 0 {
		budget = min(task.ContextBudgetMaxTokens, budget)
	}
	cfg.ContextBudgetMaxTokens = budget

	watchdog.Pause()
	history, err := s.loadChatHistory(ctx, req, chatHistoryScope(req), false, budget, task.ModelUUID)
	watchdog.Resume()
	if err != nil {
		return nil, err
	}
	normalized := normalizeContext(history.messages)
	messages := normalized.models()
	estimates := make([]int, len(messages))
	for i := range messages {
		estimates[i] = estimateMessageTokens(messages[i])
	}
	cfg.ContextHistoryTokenEstimates = estimates
	cfg.ContextTrimmableMessages = len(messages)
	cfg.ContextFrags = historyContextFragsForMessages(messages, history.records)
	cfg.Messages = normalized.sent()
	cfg.ForkContextSourceMessageIDs = historySourceMessageIDsForMessages(messages, history.records)
	cfg.ContextProvenance = contextProvenance(messages, compactionInputs(req))
	cfg.Query = ""
	cfg.ContextQueryMaterialized = true
	markRequiredHistoryMessageCurrent(cfg, requestID)
	if cfg.ContextCurrentUserMessageIndex == nil {
		return nil, fmt.Errorf("subagent history does not render its request %s", requestID)
	}

	rc := resolvedContext{runConfig: *cfg, model: models.GetResponse{ID: task.ModelUUID}}
	committer := s.bindNativeRun(ctx, req, rc, cfg, newReasoningTimingTracker(nil))
	if committer == nil {
		return nil, errors.New("subagent step persistence is unavailable")
	}
	if task.OnStepPersisted != nil {
		commit, interrupt := cfg.OnStepCommitted, cfg.OnStepInterrupted
		durable := func() {
			if len(committer.persistedMessages()) > 0 {
				task.OnStepPersisted()
			}
		}
		cfg.OnStepCommitted = func(ctx context.Context, index int, record *step.Record) (native.StepDirective, error) {
			directive, err := commit(ctx, index, record)
			durable()
			return directive, err
		}
		cfg.OnStepInterrupted = func(ctx context.Context, index int, record *step.Record) error {
			err := interrupt(ctx, index, record)
			durable()
			return err
		}
	}
	cfg.ModelIdleWindow = watchdog.Timeout()
	if fold := cfg.SwitchContext; fold != nil {
		cfg.SwitchContext = func(ctx context.Context, sw native.ContextSwitch) (string, error) {
			watchdog.Pause()
			defer watchdog.Resume()
			return fold(ctx, sw)
		}
	}
	*cfg = s.prepareRunConfig(ctx, *cfg)
	return committer.finish, nil
}

// subagentRunRequest is the request a spawned task's run makes on the agent's
// thread: a subagent turn whose input is the persisted task row, with no
// memory extraction or title rewrite, as a direct turn on the thread has.
func subagentRunRequest(handle sessionruntime.RunHandle, task tools.SpawnRunConfig, requestID string) ChatRequest {
	identity := task.Identity
	req := ChatRequest{
		BotID:                    handle.BotID,
		ChatID:                   identity.ChatID,
		ThreadID:                 handle.SessionID,
		RunID:                    handle.RunID,
		RunHandle:                handle,
		TurnID:                   handle.TurnID,
		TurnPosition:             task.TurnPosition,
		Query:                    task.Query,
		RawQuery:                 task.Query,
		SessionType:              sessionpkg.TypeSubagent,
		UserID:                   identity.UserID,
		SourceChannelIdentityID:  identity.ChannelIdentityID,
		CurrentChannel:           identity.CurrentPlatform,
		ReplyTarget:              identity.ReplyTarget,
		ConversationType:         identity.ConversationType,
		UserMessagePersisted:     true,
		PersistedUserMessageID:   requestID,
		RequiredHistoryMessageID: requestID,
		SkipMemoryExtraction:     true,
		SkipTitleGeneration:      true,
		inputInHistory:           true,
	}
	if identity.WorkspaceTargetID != "" {
		req.WorkspaceTarget = &WorkspaceTarget{
			TargetID: identity.WorkspaceTargetID,
			Kind:     identity.WorkspaceTargetKind,
			Name:     identity.WorkspaceTargetName,
		}
	}
	return req
}

// SubagentFailure is the error a spawned attempt that ended with event reports
// to the spawn provider: the run's public failure, named as for any native run,
// with the event's cause in its chain.
func (*Service) SubagentFailure(event native.StreamEvent) error {
	return agentStreamFailure(event)
}

// SubagentRunObserver returns a per-event publisher that feeds one spawned
// agent run's stream into the session runtime, or nil when there is nothing to
// publish to — no runtime configured, or a context without an admitted handle.
//
// The returned function mirrors forwardWSStreamEvents' publishing discipline:
// events leave as publicAgentStreamEvent makes them, so a failed spawned run
// is named like any other native run; they are published on a context that
// survives the run's cancellation (an aborted run's final events are exactly
// the ones a subscriber must see); and a lost ownership stops publishing
// outright because every later event would fail identically.
func (s *Service) SubagentRunObserver(ctx context.Context) native.SpawnRunObserver {
	if s == nil || s.decisionRuntime == nil {
		return nil
	}
	handle, ok := SubagentRunHandleFromContext(ctx)
	if !ok || strings.TrimSpace(handle.RunID) == "" || handle.FencingToken <= 0 {
		return nil
	}
	publishCtx := context.WithoutCancel(ctx)
	var lost atomic.Bool
	return func(event native.StreamEvent) native.SpawnRunObservation {
		if lost.Load() {
			return native.SpawnRunObservation{}
		}
		_, status, err := s.decisionRuntime.HandleAgentEventWithStatus(publishCtx, handle, publicAgentStreamEvent(event))
		if err != nil {
			if errors.Is(err, sessionruntime.ErrRunOwnershipLost) {
				lost.Store(true)
			}
			if s.logger != nil {
				s.logger.WarnContext(ctx, "publish subagent runtime event failed",
					slog.Any("error", err),
					slog.String("bot_id", handle.BotID),
					slog.String("session_id", handle.SessionID),
					slog.String("run_id", handle.RunID),
				)
			}
			return native.SpawnRunObservation{}
		}
		if event.Type != native.EventAgentEnd && event.Type != native.EventAgentAbort {
			return native.SpawnRunObservation{}
		}
		observation := native.SpawnRunObservation{TerminalResolved: true}
		switch status {
		case sessionruntime.RunStatusCompleted:
			observation.TerminalOutcome = tools.SpawnAttemptCompleted
		case sessionruntime.RunStatusAborted:
			observation.TerminalOutcome = tools.SpawnAttemptAbort
		case sessionruntime.RunStatusErrored:
			observation.TerminalOutcome = tools.SpawnAttemptFailure
		default:
			return native.SpawnRunObservation{}
		}
		return observation
	}
}
