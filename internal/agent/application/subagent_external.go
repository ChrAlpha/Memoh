package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/background"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/botagents"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/errlog"
	"github.com/felinics/memoh/internal/errs"
	"github.com/felinics/memoh/internal/workdir"
	"github.com/felinics/memoh/internal/workspace"
)

const maxExternalSubagents = 4

type subagentBackendCatalog interface {
	List(context.Context, string) ([]botagents.BotAgent, error)
	GetActive(context.Context, string, string) (botagents.BotAgent, error)
	ValidateConfiguration(context.Context, botagents.BotAgent, map[string]any) error
}

type externalExecutionStore interface {
	BeginExternalSubagentExecution(context.Context, session.Thread, string, int) error
	FinishExternalSubagentExecution(context.Context, string, string) error
	ListExternalExecutionHolds(context.Context, string) ([]session.Thread, error)
	ClaimExternalSubagentReap(context.Context, string, string, string) (bool, error)
	ResolveExternalSubagentTermination(context.Context, string, string, string) (bool, error)
}

// SetExecutionService binds application execution after the tool graph exists.
func (p *SubagentService) SetExecutionService(s *Service, catalog subagentBackendCatalog) {
	p.execution = s
	s.subagentBackends = catalog
}

func (s *Service) subagentParent(ctx context.Context, parent tools.SessionContext) (session.Thread, error) {
	if s.sessionService == nil || parent.IsSubagent || parent.SessionType == session.TypeSubagent {
		return session.Thread{}, apperror.New(apperror.CodeRuntimeControlForbidden, nil)
	}
	thread, err := s.sessionService.Get(ctx, parent.SessionID)
	if err != nil {
		return thread, err
	}
	if thread.BotID != parent.BotID || thread.SessionMode == session.TypeSubagent || thread.Type == session.TypeSubagent {
		return thread, apperror.New(apperror.CodeRuntimeControlForbidden, nil)
	}
	if (parent.WorkspaceTargetKind != "" && parent.WorkspaceTargetKind != workdir.TargetKindNative) || (parent.WorkspaceTargetID != "" && parent.WorkspaceTargetID != workspace.WorkspaceTargetNative) {
		return thread, apperror.New(apperror.CodeExternalAgentContainerWorkspaceRequired, nil)
	}
	return thread, s.requireRuntimeOwnerWorkspaceExec(ctx, parent.BotID, parent.UserID)
}

func (s *Service) subagentBackend(ctx context.Context, botID, id string) (botagents.BotAgent, external.SubagentDriver, error) {
	if s.subagentBackends == nil {
		return botagents.BotAgent{}, nil, apperror.New(apperror.CodeBotAgentUnavailable, nil)
	}
	agent, err := s.subagentBackends.GetActive(ctx, botID, id)
	if err != nil {
		return agent, nil, apperror.Wrap(apperror.CodeBotAgentUnavailable, err, nil)
	}
	driver, ok := s.externalDrivers[botagents.SessionRuntime(agent.Runtime)].(external.SubagentDriver)
	if !ok || !driver.SubagentCapabilities().Supported() {
		return agent, nil, apperror.New(apperror.CodeRuntimeControlUnsupported, nil)
	}
	var metadata map[string]any
	if s.queries != nil {
		id, err := db.ParseUUID(botID)
		if err != nil {
			return agent, nil, err
		}
		bot, err := s.queries.GetBotByID(ctx, id)
		if err != nil {
			return agent, nil, err
		}
		if len(bot.Metadata) > 0 {
			if err = json.Unmarshal(bot.Metadata, &metadata); err != nil {
				return agent, nil, err
			}
		}
	}
	if err = s.subagentBackends.ValidateConfiguration(ctx, agent, metadata); err != nil {
		return agent, nil, apperror.Wrap(apperror.CodeBotAgentInvalidMetadata, err, nil)
	}
	return agent, driver, nil
}

func (p *SubagentService) Backends(ctx context.Context, parent tools.SessionContext) (out any, callErr error) {
	defer func() { callErr = p.publicError(ctx, "agent.subagent.backends", callErr) }()
	if p.execution == nil {
		return nil, apperror.New(apperror.CodeExternalRuntimeUnavailable, nil)
	}
	s := p.execution
	if _, err := s.subagentParent(ctx, parent); err != nil {
		return nil, err
	}
	agents, err := s.subagentBackends.List(ctx, parent.BotID)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(agents))
	for _, agent := range agents {
		_, driver, err := s.subagentBackend(ctx, parent.BotID, agent.ID)
		item := map[string]any{"bot_agent_id": agent.ID, "name": agent.Name, "runtime_type": botagents.SessionRuntime(agent.Runtime), "available": err == nil}
		if driver != nil {
			item["capabilities"] = driver.SubagentCapabilities()
		}
		if err != nil {
			item["code"] = string(apperror.CodeOf(err))
		}
		items = append(items, item)
	}
	return map[string]any{"backends": items, "count": len(items)}, nil
}

func (p *SubagentService) spawnExternal(ctx context.Context, parent tools.SessionContext, id, task string, args tools.SpawnAgentArgs) (any, error) {
	if p.execution == nil {
		return nil, apperror.New(apperror.CodeExternalRuntimeUnavailable, nil)
	}
	s := p.execution
	thread, err := s.subagentParent(ctx, parent)
	if err != nil {
		return nil, err
	}
	agent, _, err := s.subagentBackend(ctx, parent.BotID, args.BotAgentID)
	if err != nil {
		return nil, err
	}
	projectPath := firstNonEmpty(parent.WorkdirPath, "/data")
	if resolved, bound, err := s.resolveSessionWorkdirBinding(ctx, parent.BotID, thread.ID); err != nil {
		return nil, err
	} else if bound {
		if resolved.Kind != workdir.TargetKindNative {
			return nil, apperror.New(apperror.CodeExternalAgentContainerWorkspaceRequired, nil)
		}
		projectPath = resolved.WorkDir
	}
	child, _, err := p.sessionService.CreateSubagent(ctx, session.CreateSubagentInput{Thread: session.CreateInput{
		BotID: parent.BotID, ParentThreadID: parent.SessionID, CreatedByUserID: parent.UserID,
		Type: session.TypeSubagent, SessionMode: session.TypeSubagent, RuntimeType: botagents.SessionRuntime(agent.Runtime), BotAgentID: agent.ID,
		WorkdirID: thread.WorkdirID, WorkdirPath: projectPath, Title: truncateTitle(task, 100),
		Metadata:        map[string]any{"agent_id": id, "agent_control_version": agentControlVersion},
		RuntimeMetadata: map[string]any{"runtime_owner_account_id": parent.UserID, "project_path": projectPath},
	}})
	if err != nil {
		return nil, err
	}
	rec := agentRecord{AgentID: id, SessionID: child.ID, RuntimeType: child.RuntimeType, BotAgentID: child.BotAgentID, Title: child.Title, CreatedAt: child.CreatedAt, UpdatedAt: child.UpdatedAt}
	return p.submitAgentTask(ctx, parent, rec, session.SubagentConfig{}, task, args.RunInBackground)
}

// managedChildDriver adds durable dispatch/anchor barriers to an existing
// runtime turn. Context remains driver-owned; it never replays stored output.
type managedChildDriver struct {
	external.SubagentDriver
	service *Service
	child   session.Thread
	store   externalExecutionStore
	started bool
	result  external.PromptResult
}

func (d *managedChildDriver) Prompt(ctx context.Context, input external.PromptInput) (external.PromptResult, error) {
	if err := d.store.BeginExternalSubagentExecution(ctx, d.child, input.RunID, maxExternalSubagents); err != nil {
		if errors.Is(err, session.ErrExternalChildCapacity) {
			err = apperror.Wrap(apperror.CodeExternalRuntimeBusy, err, nil)
		}
		return external.PromptResult{}, err
	}
	d.started = true
	input.CanRequestUserInput = false
	input.SessionMode = session.TypeSubagent
	input.Steering = nil
	input.PersistRuntimeState = func(ctx context.Context, metadata map[string]any) error {
		_, err := d.service.sessionService.MergeRuntimeMetadata(ctx, d.child.ID, d.child.RuntimeType, metadata)
		return err
	}
	result, err := d.SubagentDriver.Prompt(ctx, input)
	d.result = result
	if result.Execution != external.ExecutionNotStarted && result.Execution != external.ExecutionStopped {
		_, persistErr := d.service.sessionService.MergeRuntimeMetadata(context.WithoutCancel(ctx), d.child.ID, d.child.RuntimeType, map[string]any{"external_termination_unconfirmed": input.RunID})
		return result, apperror.Wrap(apperror.CodeExternalRuntimeTerminationUnconfirmed, errors.Join(err, persistErr), nil)
	}
	return result, err
}

func (s *Service) runExternalChild(ctx context.Context, task *agentRequest) agentRunResult {
	res := agentRunResult{AgentID: task.agentID, SessionID: task.agentSessionID, TaskID: task.taskID, RunID: task.admission.RunID, RuntimeType: task.runtimeType, BotAgentID: task.botAgentID}
	fail := func(err error) agentRunResult {
		res.fail(err)
		res.AttemptResolved = true
		res.AttemptOutcome = tools.SpawnAttemptFailure
		return res
	}
	if _, err := s.subagentParent(ctx, task.parentSession); err != nil {
		return fail(err)
	}
	child, err := s.sessionService.Get(ctx, task.agentSessionID)
	if err != nil {
		return fail(err)
	}
	if child.BotID != task.parentSession.BotID || child.ParentThreadID != task.parentSession.SessionID || child.BotAgentID != task.botAgentID || child.RuntimeType != task.runtimeType || child.SessionMode != session.TypeSubagent {
		return fail(apperror.New(apperror.CodeBotAgentUnavailable, nil))
	}
	agent, driver, err := s.subagentBackend(ctx, child.BotID, child.BotAgentID)
	if err != nil {
		return fail(err)
	}
	if botagents.SessionRuntime(agent.Runtime) != child.RuntimeType {
		return fail(apperror.New(apperror.CodeBotAgentUnavailable, nil))
	}
	if metadataString(child.RuntimeMetadata, "external_child_run") != "" || metadataString(child.RuntimeMetadata, "external_execution_run") != "" {
		return fail(apperror.New(apperror.CodeExternalRuntimeBusy, nil))
	}
	handle, ok := SubagentRunHandleFromContext(ctx)
	if !ok || handle.RunID != task.admission.RunID || handle.SessionID != child.ID || handle.BotID != child.BotID || handle.FencingToken <= 0 {
		return fail(errors.New("external child requires its admitted run"))
	}
	store, ok := s.sessionService.(externalExecutionStore)
	if !ok {
		return fail(errors.New("external child execution store unavailable"))
	}
	wrapped := &managedChildDriver{SubagentDriver: driver, service: s, child: child, store: store}
	req := ChatRequest{
		BotID: child.BotID, ChatID: firstNonEmpty(task.parentSession.ChatID, child.BotID), ThreadID: child.ID,
		RunID: handle.RunID, RunHandle: handle, TurnID: task.admission.TurnID, TurnPosition: &task.admission.TurnPosition,
		Query: task.message, RawQuery: task.message, UserID: task.parentSession.UserID, SourceChannelIdentityID: task.parentSession.ChannelIdentityID,
		ChatToken: task.parentSession.SessionToken, CurrentChannel: task.parentSession.CurrentPlatform, ReplyTarget: task.parentSession.ReplyTarget, ConversationType: task.parentSession.ConversationType,
		SessionType: session.TypeSubagent, RuntimeType: child.RuntimeType, SkipMemoryExtraction: true, SkipTitleGeneration: true,
		UserMessagePersisted: task.messagePersisted, PersistedUserMessageID: task.requestMessageID,
	}
	events := make(chan WSStreamEvent, 16)
	done := make(chan error, 1)
	go func() { _, err := s.streamRuntimeWS(ctx, wrapped, req, events, nil, false); done <- err; close(events) }()
	observer := s.SubagentRunObserver(ctx)
	var streamErr error
	for raw := range events {
		var event native.StreamEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			streamErr = err
			continue
		}
		if event.Type == native.EventError {
			streamErr = agentStreamFailure(event)
		}
		if observer != nil {
			observation := observer(event)
			if observation.TerminalResolved {
				res.AttemptResolved = true
				res.AttemptOutcome = observation.TerminalOutcome
			}
		}
	}
	streamErr = errors.Join(streamErr, <-done)
	res.Text = boundedChildReport(wrapped.result)
	res.Usage = wrapped.result.Usage
	res.ExecutionState = string(wrapped.result.Execution)
	if wrapped.started && (wrapped.result.Execution == external.ExecutionStopped || wrapped.result.Execution == external.ExecutionNotStarted) {
		streamErr = errors.Join(streamErr, store.FinishExternalSubagentExecution(context.WithoutCancel(ctx), child.ID, handle.RunID))
	}
	if wrapped.started && wrapped.result.Execution != external.ExecutionStopped && wrapped.result.Execution != external.ExecutionNotStarted {
		res.ExecutionState = string(external.ExecutionUnknown)
		return fail(apperror.Wrap(apperror.CodeExternalRuntimeTerminationUnconfirmed, streamErr, nil))
	}
	if streamErr != nil {
		return fail(streamErr)
	}
	if !res.AttemptResolved {
		res.AttemptResolved = true
		res.AttemptOutcome = tools.SpawnAttemptAbort
		if wrapped.result.TurnCompleted {
			res.AttemptOutcome = tools.SpawnAttemptCompleted
		}
	}
	if res.AttemptOutcome == tools.SpawnAttemptAbort {
		res.Status = string(background.TaskKilled)
	}
	return res
}

func boundedChildReport(result external.PromptResult) string {
	text := result.Text
	if text == "" {
		var report strings.Builder
		for _, message := range result.Output {
			if message.Role == sdk.MessageRoleAssistant {
				for _, part := range message.Content {
					if textPart, ok := part.(sdk.TextPart); ok {
						report.WriteString(textPart.Text)
					}
				}
			}
		}
		text = report.String()
	}
	const limit = 16 * 1024
	if len(text) > limit {
		text = strings.ToValidUTF8(text[len(text)-limit:], "")
	}
	return text
}

// reconcileExternalChild never replays a prompt. Its durable claim excludes
// live runs and names the exact execution, including a soft-deleted child.
func (s *Service) reconcileExternalChild(ctx context.Context, child session.Thread, runID string) (bool, error) {
	store, ok := s.sessionService.(externalExecutionStore)
	if !ok {
		return false, errors.New("external child execution store unavailable")
	}
	driver, ok := s.externalDrivers[child.RuntimeType].(external.SubagentDriver)
	if !ok {
		return false, apperror.New(apperror.CodeRuntimeControlUnsupported, nil)
	}
	if runID == "" || metadataString(child.RuntimeMetadata, "external_execution_run") != runID {
		return false, apperror.New(apperror.CodeRuntimeControlRequestInvalid, nil)
	}
	claim := uuid.NewString()
	claimed, err := store.ClaimExternalSubagentReap(ctx, child.ID, runID, claim)
	if err != nil || !claimed {
		return false, err
	}
	reapCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	err = driver.ReconcileSubagent(reapCtx, external.PromptInput{BotID: child.BotID, BotAgentID: child.BotAgentID, ThreadID: child.ID, RunID: runID, SessionMode: session.TypeSubagent, RuntimeMetadata: child.RuntimeMetadata})
	if err != nil {
		return false, apperror.Wrap(apperror.CodeExternalRuntimeTerminationUnconfirmed, err, nil)
	}
	return store.ResolveExternalSubagentTermination(reapCtx, child.ID, runID, claim)
}

func (p *SubagentService) publicError(ctx context.Context, operation string, err error) error {
	if err == nil {
		return nil
	}
	public, fault := errs.Answer(ctx, err)
	if p.logger != nil && (fault == apperror.FaultServer || fault == apperror.FaultDependency) {
		record := errlog.Event(ctx, operation, err, errlog.Options{})
		p.logger.LogAttrs(ctx, record.Level, "subagent operation failed", record.Attrs()...)
	}
	return public
}

func marshalSubagentUsage(usage *sdk.Usage) json.RawMessage {
	if usage == nil {
		return nil
	}
	raw, _ := json.Marshal(usage)
	return raw
}
