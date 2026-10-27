package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/bots"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/errlog"
	"github.com/felinics/memoh/internal/hooks"
	"github.com/felinics/memoh/internal/runtimekind"
)

func (p *SubagentService) runExternalSubagentTask(ctx context.Context, req *agentRequest) (result agentRunResult) {
	result = agentRunResult{AgentID: req.agentID, SessionID: req.agentSessionID, TaskID: req.taskID, RunID: req.admission.RunID, RuntimeType: req.runtimeType, BotAgentID: req.botAgentID}
	if p.execution == nil {
		return result.failed(apperror.New(apperror.CodeExternalRuntimeUnavailable, nil))
	}
	if err := p.runSubagentHook(ctx, hooks.EventSubagentStart, req, result); err != nil {
		return result.failed(err)
	}
	defer func() {
		if err := p.runSubagentHook(context.WithoutCancel(ctx), hooks.EventSubagentStop, req, result); err != nil {
			record := errlog.Event(ctx, "subagent.stop_hook", err, errlog.Options{})
			p.logger.LogAttrs(ctx, record.Level, "external subagent stop hook failed", record.Attrs()...)
		}
	}()
	return p.execution.runExternalChild(ctx, req)
}

func (p *SubagentService) Control(ctx context.Context, parent tools.SessionContext, args tools.SubagentControlArgs) (out any, callErr error) {
	defer func() { callErr = p.publicError(ctx, "agent.subagent.control", callErr) }()
	if err := validateParentSession(parent); err != nil {
		return nil, err
	}
	if p.execution == nil {
		return nil, apperror.New(apperror.CodeRuntimeControlUnsupported, nil)
	}
	id, err := normalizeAgentID(args.ID)
	if err != nil {
		return nil, err
	}
	rec, err := p.findAgent(ctx, parent, id)
	if err != nil {
		return nil, err
	}
	s := p.execution
	child := rec.Thread
	if child.BotID != parent.BotID || child.ParentThreadID != parent.SessionID || child.SessionMode != session.TypeSubagent {
		return nil, apperror.New(apperror.CodeRuntimeControlForbidden, nil)
	}
	permission := bots.PermissionChat
	if runtimekind.IsExternal(child.RuntimeType) {
		permission = bots.PermissionWorkspaceExec
	}
	if s.botPermissions == nil {
		return nil, apperror.New(apperror.CodeRuntimeControlForbidden, nil)
	}
	allowed, err := s.botPermissions.HasBotPermission(ctx, parent.BotID, parent.UserID, permission)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, apperror.New(apperror.CodeRuntimeControlForbidden, nil)
	}
	state, err := s.subagentState(ctx, child, true)
	if err != nil {
		return nil, err
	}
	state["agent_id"] = id
	switch args.Action {
	case "status":
		return state, nil
	case "stop":
		if strings.TrimSpace(args.RunID) == "" || state["run_id"] != args.RunID {
			return nil, apperror.New(apperror.CodeRuntimeControlRequestInvalid, nil)
		}
		if state["execution_state"] == string(external.ExecutionUnknown) {
			settled, err := s.reconcileExternalChild(ctx, child, args.RunID)
			if err != nil {
				return nil, err
			}
			if settled {
				state["execution_state"] = string(external.ExecutionStopped)
				state["status"] = "failed"
				state["termination_confirmed"] = true
			}
			return state, nil
		}
		accepted, err := s.AbortRuntimeRun(ctx, child.BotID, child.ID, args.RunID, uuid.NewString())
		if err != nil {
			return nil, err
		}
		state["stop_accepted"] = accepted
		if accepted {
			state["status"] = "stopping"
		}
		return state, nil
	default:
		return nil, apperror.New(apperror.CodeRuntimeControlRequestInvalid, nil)
	}
}

// subagentState reads durable state and a bounded transcript report. It never
// starts a new run, and does not depend on the background manager's lifetime.
func (s *Service) subagentState(ctx context.Context, child session.Thread, includeResult bool) (map[string]any, error) {
	state := map[string]any{"session_id": child.ID, "runtime_type": child.RuntimeType, "status": "idle"}
	if child.BotAgentID != "" {
		state["bot_agent_id"] = child.BotAgentID
	}
	id, err := db.ParseUUID(child.ID)
	if err != nil {
		return nil, err
	}
	if s.queries == nil {
		return nil, errors.New("child run ledger unavailable")
	}
	run, err := s.queries.GetLatestSessionRun(ctx, id)
	if accepted := metadataString(child.RuntimeMetadata, "external_child_run"); runtimekind.IsExternal(child.RuntimeType) && accepted != "" {
		runID, parseErr := db.ParseUUID(accepted)
		if parseErr != nil {
			return nil, parseErr
		}
		run, err = s.queries.GetSessionRun(ctx, runID)
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	active := false
	if err == nil {
		state["run_id"] = run.RunID.String()
		state["status"] = run.State
		if run.ErrorCode.Valid && run.ErrorCode.String != "" {
			state["code"] = run.ErrorCode.String
		}
		switch run.State {
		case "accepted", "running", "waiting_decision", "finishing":
			active = true
		}
	}
	if held := metadataString(child.RuntimeMetadata, "external_execution_run"); held != "" {
		state["execution_state"] = "running"
		if !active || state["run_id"] != held || metadataString(child.RuntimeMetadata, "external_termination_unconfirmed") != "" {
			state["status"] = "execution_unknown"
			state["execution_state"] = string(external.ExecutionUnknown)
			state["run_id"] = held
			state["code"] = string(apperror.CodeExternalRuntimeTerminationUnconfirmed)
		}
	} else if runtimekind.IsExternal(child.RuntimeType) && metadataString(child.RuntimeMetadata, "external_child_run") != "" {
		state["execution_state"] = string(external.ExecutionStopped)
	}
	if includeResult && !active && s.messageService != nil {
		rows, err := s.messageService.ListLatestBySession(ctx, child.ID, 8)
		if err != nil {
			return nil, err
		}
		result := external.PromptResult{}
		// This is result readback, not a runtime-context or resume operation.
		for i := len(rows) - 1; i >= 0; i-- {
			row := rows[i]
			if row.Role != "assistant" || row.TurnID != run.TurnID.String() {
				continue
			}
			message := historyfrag.StoredModelMessageToSDKMessage(historyfrag.DecodeStoredModelMessage(s.logger, row.ID, row.Role, row.Content))
			result.Output = append(result.Output, message)
		}
		report := map[string]any{"text": boundedChildReport(result), "transcript_session_id": child.ID, "usage": nil}
		if run.RunID.Valid {
			usages, err := s.queries.ListSubagentRunUsage(ctx, sqlc.ListSubagentRunUsageParams{SessionID: id, RunID: run.RunID})
			if err != nil {
				return nil, err
			}
			if runtimekind.IsExternal(child.RuntimeType) && len(usages) > 1 {
				usages = usages[:1]
			}
			var reported *sdk.Usage
			for _, raw := range usages {
				var usage sdk.Usage
				if err := json.Unmarshal(raw, &usage); err != nil {
					return nil, err
				}
				if !runtimekind.IsExternal(child.RuntimeType) && usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.TotalTokens == 0 {
					continue // Native SDK zero aggregates carry no reported-usage evidence.
				}
				if reported == nil {
					reported = &usage
				} else {
					sum := reported.Add(usage)
					reported = &sum
				}
			}
			if reported != nil {
				report["usage"] = reported
				report["usage_scope"] = "reported_model_calls"
				if runtimekind.IsExternal(child.RuntimeType) {
					report["usage_scope"] = "runtime_turn"
				}
			}
		}
		state["result"] = report
	}
	return state, nil
}
