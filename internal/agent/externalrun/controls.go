package externalrun

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/agentapi/runtimeerr"
	"github.com/felinics/memoh/internal/agent/decision/approval"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/bots"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/runtimefence"
	"github.com/felinics/memoh/internal/runtimekind"
)

func (h *Host) runtimeControlSession(ctx context.Context, request turn.RuntimeControlRequest) (session.Thread, error) {
	if request.TeamID != "" && h.allowedTeam != "" && request.TeamID != h.allowedTeam {
		return session.Thread{}, turn.ErrTeamNotServed
	}
	sess, err := h.sessions.Get(ctx, request.ThreadID)
	if err != nil {
		return sess, err
	}
	if sess.BotID != request.BotID || strings.TrimSpace(request.ActorID) == "" {
		return sess, approval.ErrForbidden
	}
	return sess, nil
}

func (h *Host) runtimeControlTarget(ctx context.Context, request turn.RuntimeControlRequest) (session.Thread, external.Driver, external.PromptInput, error) {
	sess, err := h.runtimeControlSession(ctx, request)
	if err != nil {
		return sess, nil, external.PromptInput{}, err
	}
	manage, err := h.permissions.HasBotPermission(ctx, request.BotID, request.ActorID, bots.PermissionManage)
	if err != nil {
		return sess, nil, external.PromptInput{}, err
	}
	if !manage {
		if sess.CreatedByUserID != request.ActorID {
			return sess, nil, external.PromptInput{}, approval.ErrForbidden
		}
		permission := bots.PermissionChat
		if session.UsesDecisionWaiter(sess) {
			permission = bots.PermissionWorkspaceExec
		}
		allowed, err := h.permissions.HasBotPermission(ctx, request.BotID, request.ActorID, permission)
		if err != nil {
			return sess, nil, external.PromptInput{}, err
		}
		if !allowed {
			return sess, nil, external.PromptInput{}, approval.ErrForbidden
		}
	}
	return sess, h.drivers[sess.RuntimeType], runtimeControlInput(sess, request), nil
}

func runtimeControlInput(sess session.Thread, request turn.RuntimeControlRequest) external.PromptInput {
	meta := runreq.RuntimeSessionMeta(sess)
	input := external.PromptInput{
		BotID: sess.BotID, BotAgentID: sess.BotAgentID, ThreadID: sess.ID,
		RuntimeMetadata: meta, Command: request.Command,
		RuntimeOwnerAccountID: runreq.MetadataString(meta, "runtime_owner_account_id"),
		ChannelIdentityID:     request.ActorID, ToolHTTPURL: request.ToolHTTPURL,
	}
	return input
}

func (h *Host) RuntimeControls(ctx context.Context, request turn.RuntimeControlRequest) (out external.Controls, resultErr error) {
	defer func() { resultErr = publicRuntimeControlError(resultErr) }()
	_, driver, input, err := h.runtimeControlTarget(ctx, request)
	if err != nil {
		return external.Controls{}, err
	}
	// Command discovery runs a CLI in the workspace the session's turns use.
	ctx, err = h.runtimeControlContext(ctx, request)
	if err != nil {
		return external.Controls{}, err
	}
	return external.ReadControls(ctx, driver, input)
}

// RuntimeCommands reads declarations without fetching unrelated runtime state.
// Catalog discovery needs chat access, not ownership of the runtime controls.
// Executing a declared command still uses the existing control/admission gates.
func (h *Host) RuntimeCommands(ctx context.Context, request turn.RuntimeControlRequest) (commands []external.Command, err error) {
	defer func() { err = publicRuntimeControlError(err) }()
	sess, err := h.runtimeControlSession(ctx, request)
	if err != nil {
		return nil, err
	}
	allowed, err := h.permissions.HasBotPermission(ctx, request.BotID, request.ActorID, bots.PermissionChat)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, approval.ErrForbidden
	}
	if provider, ok := h.drivers[sess.RuntimeType].(external.CommandProvider); ok {
		ctx, err = h.runtimeControlContext(ctx, request)
		if err != nil {
			return nil, err
		}
		return provider.Commands(ctx, runtimeControlInput(sess, request))
	}
	return []external.Command{}, nil
}

func (h *Host) RuntimeGoal(ctx context.Context, request turn.RuntimeControlRequest) (goal *external.Goal, err error) {
	defer func() { err = publicRuntimeControlError(err) }()
	_, driver, input, err := h.runtimeControlTarget(ctx, request)
	if err != nil {
		return nil, err
	}
	provider, ok := driver.(external.GoalProvider)
	if !ok {
		return nil, external.ErrControlUnsupported
	}
	return provider.Goal(ctx, input)
}

func (h *Host) SetRuntimeMode(ctx context.Context, request turn.RuntimeControlRequest) (out external.ModeState, err error) {
	defer func() { err = publicRuntimeControlError(err) }()
	_, driver, _, err := h.runtimeControlTarget(ctx, request)
	if err != nil {
		return out, err
	}
	if _, _, _, err := runtimeModeAccessors(driver, request.ModeKind); err != nil {
		return out, err
	}

	err = h.runRuntimeControl(ctx, request, true, func(ctx context.Context, sess session.Thread, driver external.Driver, input external.PromptInput) error {
		read, set, metadataKey, err := runtimeModeAccessors(driver, request.ModeKind)
		if err != nil {
			return err
		}
		modes, err := read(ctx, input)
		if err != nil {
			return err
		}
		valid := false
		for _, mode := range modes.AvailableModes {
			if mode.ID == request.ModeID {
				valid = true
				break
			}
		}
		// An unsupported snapshot may only mean the runtime is cold (ACP
		// reads its live session); let the driver's own set decide then.
		if modes.Supported && !valid {
			return external.ErrModeUnavailable
		}
		out, err = set(ctx, input, request.ModeID)
		if err != nil {
			return err
		}
		_, err = h.sessions.MergeRuntimeMetadata(ctx, sess.ID, sess.RuntimeType, map[string]any{metadataKey: out.CurrentModeID})
		return err
	})
	return out, err
}

type (
	readRuntimeMode func(context.Context, external.PromptInput) (external.ModeState, error)
	setRuntimeMode  func(context.Context, external.PromptInput, string) (external.ModeState, error)
)

func runtimeModeAccessors(driver external.Driver, kind string) (readRuntimeMode, setRuntimeMode, string, error) {
	switch kind {
	case "", "permission":
		if provider, ok := driver.(external.ModeProvider); ok {
			return provider.Modes, provider.SetMode, "permission_mode", nil
		}
	case "plan":
		if provider, ok := driver.(external.PlanModeProvider); ok {
			return provider.PlanMode, provider.SetPlanMode, "collaboration_mode", nil
		}
	default:
		return nil, nil, "", apperror.New(apperror.CodeRuntimeControlRequestInvalid, nil)
	}
	return nil, nil, "", external.ErrControlUnsupported
}

// ExecuteRuntimeCommand handles commands which do not create a conversation
// turn. Turn commands go through the existing chat admission/persistence path.
func (h *Host) ExecuteRuntimeCommand(ctx context.Context, request turn.RuntimeControlRequest) (result turn.RuntimeCommandResult, resultErr error) {
	defer func() { resultErr = publicRuntimeControlError(resultErr) }()
	_, driver, input, err := h.runtimeControlTarget(ctx, request)
	if err != nil {
		return turn.RuntimeCommandResult{}, err
	}
	provider, ok := driver.(external.CommandProvider)
	if !ok {
		return turn.RuntimeCommandResult{}, external.ErrCommandUnavailable
	}
	controlCtx, err := h.runtimeControlContext(ctx, request)
	if err != nil {
		return turn.RuntimeCommandResult{}, err
	}
	commands, err := provider.Commands(controlCtx, input)
	if err != nil {
		return turn.RuntimeCommandResult{}, err
	}
	command, ok := external.FindCommand(commands, request.Command)
	if !ok {
		return turn.RuntimeCommandResult{}, external.ErrCommandUnavailable
	}
	if command.Kind == external.CommandRead && !runtimekind.ReadCommandsRequireIdle(driver.RuntimeType()) {
		return provider.ReadCommand(controlCtx, input)
	}
	if command.Kind != external.CommandOperation && command.Kind != external.CommandRead {
		return turn.RuntimeCommandResult{}, external.ErrCommandUnavailable
	}
	// A read only holds the slot so it cannot meet a turn on the native
	// session; it is no assistant turn, and must not be shown as one.
	err = h.runRuntimeControl(ctx, request, command.Kind == external.CommandRead, func(ctx context.Context, sess session.Thread, driver external.Driver, input external.PromptInput) error {
		provider, ok := driver.(external.CommandProvider)
		if !ok {
			return external.ErrCommandUnavailable
		}
		commands, err := provider.Commands(ctx, input)
		if err != nil {
			return err
		}
		current, ok := external.FindCommand(commands, request.Command)
		if !ok || (current.Kind != external.CommandOperation && current.Kind != external.CommandRead) {
			return external.ErrCommandUnavailable
		}
		if current.Kind == external.CommandRead {
			result, err = provider.ReadCommand(ctx, input)
			return err
		}
		compactor, ok := driver.(external.Compactor)
		if !ok {
			return external.ErrControlUnsupported
		}
		result, err := compactor.Compact(ctx, input)
		if err != nil {
			return err
		}
		_, err = h.sessions.MergeRuntimeMetadata(ctx, sess.ID, sess.RuntimeType, result.RuntimeMetadata)
		return err
	})
	return result, err
}

func (h *Host) runRuntimeControl(ctx context.Context, request turn.RuntimeControlRequest, configurationOnly bool, run func(context.Context, session.Thread, external.Driver, external.PromptInput) error) (resultErr error) {
	return h.runSessionControl(ctx, request, configurationOnly, func(runCtx context.Context, runID string) error {
		// Re-read after admission: a queued send or another mode change may have
		// won while this request was validating its initial snapshot.
		sess, driver, input, err := h.runtimeControlTarget(runCtx, request)
		if err != nil {
			return err
		}
		input.RunID = runID
		controlCtx, err := h.runtimeControlContext(runCtx, request)
		if err != nil {
			return err
		}
		return run(controlCtx, sess, driver, input)
	})
}

// runSessionControl shares the durable turn slot with non-chat operations.
// Authorization is performed by the caller; state must be re-read in run.
func (h *Host) runSessionControl(ctx context.Context, request turn.RuntimeControlRequest, configurationOnly bool, run func(context.Context, string) error) (resultErr error) {
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	admission, err := h.admitter.Admit(runCtx, sessionruntime.AdmitInput{
		BotID: request.BotID, SessionID: request.ThreadID, InvocationID: uuid.NewString(), InitiatorID: request.ActorID, Payload: payload,
		Execution: sessionruntime.Execution{
			ConfigurationOnly: configurationOnly,
			Admission: func(context.Context, sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error) {
				return sessionruntime.RunAdmissionView{}, nil
			},
			Cancel: func() { cancel(context.Canceled) }, OwnershipCancel: cancel,
		},
	})
	// The session runtime is this adapter's port; callers outside it only see
	// the turn sentinel.
	if errors.Is(err, sessionruntime.ErrSessionBusy) {
		return turn.ErrSessionBusy
	}
	if err != nil {
		return err
	}
	if !admission.Started {
		return turn.ErrSessionBusy
	}
	handle := admission.Handle
	runCtx = runtimefence.WithContext(runCtx, runtimefence.Fence{BotID: handle.BotID, SessionID: handle.SessionID, Token: handle.FencingToken})
	defer func() {
		status := sessionruntime.RunStatusCompleted
		if resultErr != nil {
			status = sessionruntime.RunStatusErrored
		}
		if errors.Is(resultErr, context.Canceled) || runCtx.Err() != nil {
			status = sessionruntime.RunStatusAborted
		}
		h.driver.Finish(runCtx, drive.AdmittedRun{Handle: handle},
			drive.FromOutcome(outcome.RunOutcome{Status: status, Cause: publicRuntimeControlError(resultErr)}))
	}()
	return run(runCtx, handle.RunID)
}

// runtimeControlContext pins the session's workspace target for a runtime
// command. It carries no principal: the command continues a session whose
// target was authorized when it was chosen.
func (h *Host) runtimeControlContext(ctx context.Context, request turn.RuntimeControlRequest) (context.Context, error) {
	ctx, _, err := h.runconfig.ResolveWorkspace(ctx, runconfig.WorkspaceInput{
		BotID:      request.BotID,
		SessionID:  request.ThreadID,
		BestEffort: true,
	})
	return ctx, err
}

// publicRuntimeControlError keeps the team routing sentinel intact for its
// own transports; everything else goes through the shared runtime control
// translation.
func publicRuntimeControlError(err error) error {
	if errors.Is(err, turn.ErrTeamNotServed) {
		return err
	}
	return runtimeerr.RuntimeControlError(err)
}

func (h *Host) ControlRuntimeGoal(ctx context.Context, request turn.RuntimeControlRequest, action string) (err error) {
	defer func() { err = publicRuntimeControlError(err) }()
	if action != "pause" && action != "clear" {
		return apperror.New(apperror.CodeRuntimeControlRequestInvalid, nil)
	}
	_, driver, input, err := h.runtimeControlTarget(ctx, request)
	if err != nil {
		return err
	}
	provider, ok := driver.(external.GoalProvider)
	if !ok {
		return external.ErrControlUnsupported
	}
	ctx, err = h.runtimeControlContext(ctx, request)
	if err != nil {
		return err
	}
	return provider.ControlGoal(ctx, input, action)
}
