package plan

import (
	"context"
	"fmt"
	"strings"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/externalrun"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/apperror"
)

// ChatSetup is the Setup function for chat-type entries: Web send/retry/edit,
// IM, follow-up, and resume. It handles skill checks, runtime dispatch,
// workspace preparation, replacement preflight, user-message hook, resolve,
// title generation, and executor construction.
func (p *Planner) ChatSetup(ctx context.Context, req runreq.ChatRequest, spec drive.RunSpec) (drive.RunPlan, error) {
	// The intent is recorded before anything else: it is what lets a later
	// process resume this run after a crash, and it has to name the run's
	// input as it arrived rather than as setup rewrote it.
	if err := p.admit.RecordResume(ctx, req); err != nil {
		return drive.RunPlan{}, err
	}
	if err := runreq.RejectReservedSkillMetadataIfPresent(req); err != nil {
		return drive.RunPlan{}, err
	}
	if err := p.runconfig.RejectRequestedSkillsIfUnsupportedContext(ctx, req); err != nil {
		return drive.RunPlan{}, err
	}

	dispatch, err := p.externalrun.ResolveDispatch(ctx, req)
	if err != nil {
		return drive.RunPlan{}, err
	}

	if dispatch.Kind == externalrun.DispatchExternal {
		if err := runconfig.RejectExternalAgentWorkspaceTarget(req); err != nil {
			return drive.RunPlan{}, err
		}
		// Runtimes that own their conversation context have no rewind primitive,
		// so running a replacement turn would leave that context inconsistent
		// with the visible history.
		if spec.Replacement != nil {
			return drive.RunPlan{}, apperror.New(apperror.CodeExternalAgentTurnReplacementUnsupported, nil)
		}
		// A runtime that owns its conversation context is prompted with what
		// the entry resolved rather than with a query: discuss hands it the
		// whole batch, which is history and not a message of its own.
		return p.externalRunPlan(ctx, req, spec, dispatch.Driver)
	}

	ctx, req, err = p.runconfig.BindRequestWorkspace(ctx, req, runconfig.UserPrincipal(req), false)
	if err != nil {
		return drive.RunPlan{}, err
	}

	if spec.Replacement != nil && spec.Replacement.Preflight != nil {
		if err := spec.Replacement.Preflight(ctx); err != nil {
			return drive.RunPlan{}, err
		}
	}

	if req.RawQuery == "" {
		req.RawQuery = strings.TrimSpace(req.Query)
	}

	// The hook runs when the input has not been persisted yet and this is not a
	// replacement turn. For a retry, ReusePersistedUserMessage is true only
	// when the target turn was itself rejected by the hook, so the retry can
	// pass through the hook again.
	shouldRunHook := !req.UserMessagePersisted && !req.ReusePersistedUserMessage && spec.Replacement == nil
	if shouldRunHook {
		req, err = p.bot.ApplyUserMessageHook(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				return drive.RunPlan{}, err
			}
			return drive.RunPlan{}, outcome.ClassifyUserMessageHookError(err)
		}
	}

	rc, req, err := p.runconfig.Resolve(ctx, req)
	if err != nil {
		return drive.RunPlan{}, fmt.Errorf("resolve: %w", err)
	}
	p.settleResolved(ctx, req, &rc)
	req.Query = rc.Query
	req.RunID = rc.RunConfig.RunID

	p.title.MaybeGenerate(context.WithoutCancel(ctx), req, req.RawQuery)

	// Whether this run may park on a question follows the process's user input
	// port, not its delivery end: an interruption resumed from the last process
	// keeps asking the same way the inbound message that started it did.
	opts := nativeOptions{
		LiveToolStream:      true,
		CanRequestUserInput: true,
		Steps:               stepsFresh,
		Replacement:         spec.Replacement,
	}
	if spec.Unattended {
		opts.Steps = stepsOff
	}
	plan, err := p.nativePlan(ctx, req, rc, rc.RunConfig, opts)
	if err != nil {
		return drive.RunPlan{}, err
	}
	return plan, nil
}

// externalRunPlan resolves a run an External Agent runtime executes, for every
// entry that has one: the chat entries and the schedule fire. What the driver
// is prompted with is the request's model text, which an entry whose prompt is
// not the user's own message sets on the request itself (ModelQuery): a
// schedule fire prompts with its generated prompt, and discuss with the context
// it assembles. It is never a parameter, so an entry that prompts with
// something else cannot forget to pass it. The plan carries the context
// lifecycle the run's context sections build, so the lifecycle row of a
// runtime's turn records what the runtime was given, whoever started it.
func (p *Planner) externalRunPlan(ctx context.Context, req runreq.ChatRequest, spec drive.RunSpec, driver external.Driver) (drive.RunPlan, error) {
	plan, req, err := p.externalrun.Prepare(ctx, req, driver)
	if err != nil {
		return drive.RunPlan{}, err
	}
	lifecycle := contextfrag.NewLifecycleHolder()
	if plan.ContextManifest != nil {
		lifecycle.SetManifest(*plan.ContextManifest)
	}
	if plan.MemoryTrace != nil {
		lifecycle.SetMemoryRecall(*plan.MemoryTrace)
	}
	cfg := native.RunConfig{
		RunID:            req.RunID,
		Identity:         native.SessionContext{BotID: req.BotID, SessionID: req.ThreadID},
		ContextLifecycle: lifecycle,
	}
	return drive.RunPlan{
		Exec: p.externalrun.NewExecutor(externalrun.ExecutorSpec{
			Plan:       plan,
			Req:        req,
			Unattended: spec.Unattended,
			Input:      spec.Input,
			Lifecycle:  lifecycle,
			RunConfig:  cfg,
		}),
		Req:        req,
		IdleEffort: req.ReasoningEffort,
		Lifecycle:  p.lifecycle.Terminal(ctx, cfg),
	}, nil
}
