package application

import (
	"context"
	"errors"
	"strings"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/step"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/turn"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/runtimefence"
)

// BindSubagentNativeRun gives a worker the same history, provenance, step
// commit and checkpoint contract as any admitted native run. The parent hands
// over a task; only the child's rows and checkpoint become its model context.
func (s *Service) BindSubagentNativeRun(ctx context.Context, task tools.SpawnRunConfig, cfg *native.RunConfig) (func(context.Context) error, error) {
	handle, admitted := SubagentRunHandleFromContext(ctx)
	fence, fenced := runtimefence.FromContext(ctx)
	if !admitted || !fenced || handle.RunID != task.RunID || handle.BotID != task.Identity.BotID ||
		handle.SessionID != task.Identity.SessionID || handle.FencingToken <= 0 ||
		fence.BotID != handle.BotID || fence.SessionID != handle.SessionID || fence.Token != handle.FencingToken {
		return nil, errors.New("subagent execution requires its admitted child run and fence")
	}
	if strings.TrimSpace(task.TurnRequestMessageID) == "" {
		return nil, errors.New("subagent task must be persisted before execution")
	}
	req := ChatRequest{
		BotID: handle.BotID, ChatID: task.Identity.ChatID, ThreadID: handle.SessionID,
		RunID: handle.RunID, RunHandle: handle, TurnID: handle.TurnID,
		Query: task.Query, RawQuery: task.Query, SessionType: sessionpkg.TypeSubagent,
		UserID: task.Identity.UserID, SourceChannelIdentityID: task.Identity.ChannelIdentityID,
		CurrentChannel: task.Identity.CurrentPlatform, ReplyTarget: task.Identity.ReplyTarget,
		ConversationType:     task.Identity.ConversationType,
		UserMessagePersisted: true, PersistedUserMessageID: task.TurnRequestMessageID,
		RequiredHistoryMessageID: task.TurnRequestMessageID,
		SkipMemoryExtraction:     true, SkipTitleGeneration: true,
	}
	if task.Identity.WorkspaceTargetID != "" {
		req.WorkspaceTarget = &WorkspaceTarget{
			TargetID: task.Identity.WorkspaceTargetID,
			Kind:     task.Identity.WorkspaceTargetKind, Name: task.Identity.WorkspaceTargetName,
		}
	}
	watchdog, _ := ctx.Value(subagentWatchdogKey{}).(*SubagentWatchdog)
	if watchdog != nil {
		watchdog.Pause()
		defer watchdog.Resume()
	}
	history, err := s.loadChatHistory(ctx, req, false, cfg.ContextBudgetMaxTokens, task.ModelUUID)
	if err != nil {
		return nil, err
	}
	inputs := []turn.ContextMessageSource{{Kind: "history", ID: task.TurnRequestMessageID}}
	cfg.Messages = modelMessagesToSDKMessages(history.messages)
	cfg.ContextFrags = historyContextFragsForMessages(history.messages, history.records)
	cfg.ContextSourceFrags = nil
	cfg.ForkContextSourceMessageIDs = historySourceMessageIDsForMessages(history.messages, history.records)
	cfg.ContextProvenance = contextProvenance(history.messages, history.records, inputs)
	cfg.ContextTail = inputs
	cfg.Query = "" // The accepted task is already a child-owned history row.
	cfg.ContextQueryMaterialized = true
	cfg.ContextCurrentUserMessageIndex = nil
	markRequiredHistoryMessageCurrent(cfg, task.TurnRequestMessageID)
	if cfg.ContextCurrentUserMessageIndex == nil {
		return nil, errors.New("subagent context is missing its accepted task")
	}
	cfg.Identity.IsSubagent = true
	rc := resolvedContext{runConfig: *cfg, model: models.GetResponse{ID: task.ModelUUID}}
	committer := s.bindNativeRun(ctx, req, rc, cfg, nil)
	if committer == nil {
		return nil, errors.New("subagent step persistence is unavailable")
	}
	committed, interrupted := cfg.OnStepCommitted, committer.interrupt
	notify := func() {
		if task.OnStepPersisted != nil && len(committer.persistedMessages()) > 0 {
			task.OnStepPersisted()
		}
	}
	cfg.OnStepCommitted = func(ctx context.Context, index int, record *step.Record) (native.StepDirective, error) {
		directive, err := committed(ctx, index, record)
		notify()
		return directive, err
	}
	cfg.OnStepInterrupted = func(ctx context.Context, index int, record *step.Record) error {
		err := interrupted(ctx, index, record)
		notify()
		return err
	}
	if watchdog != nil {
		cfg.ModelIdleWindow = watchdog.timeout
		if fold := cfg.SwitchContext; fold != nil {
			cfg.SwitchContext = func(ctx context.Context, sw native.ContextSwitch) (string, error) {
				watchdog.Pause()
				defer watchdog.Resume()
				return fold(ctx, sw)
			}
		}
	}
	*cfg = s.prepareRunConfig(ctx, *cfg)
	return committer.finish, nil
}
