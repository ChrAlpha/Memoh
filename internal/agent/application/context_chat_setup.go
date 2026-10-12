package application

import (
	"context"
	"fmt"
	"strings"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/apperror"
)

// chatSetup is the Setup function for chat-type entries: Web send/retry/edit,
// IM, follow-up, and resume. It handles skill checks, runtime dispatch,
// workspace preparation, replacement preflight, user-message hook, resolve,
// title generation, and executor construction.
func (s *Service) chatSetup(ctx context.Context, req ChatRequest, spec runSpec) (runPlan, error) {
	// The intent is recorded before anything else: it is what lets a later
	// process resume this run after a crash, and it has to name the run's
	// input as it arrived rather than as setup rewrote it.
	if err := s.recordRunResumeContext(ctx, req); err != nil {
		return runPlan{}, err
	}
	if err := rejectReservedSkillMetadataIfPresent(req); err != nil {
		return runPlan{}, err
	}
	if err := s.rejectRequestedSkillsIfUnsupportedContext(ctx, req); err != nil {
		return runPlan{}, err
	}

	dispatch, err := s.resolveRuntimeDispatch(ctx, req)
	if err != nil {
		return runPlan{}, err
	}

	if dispatch.kind == dispatchExternal {
		if err := rejectExternalAgentWorkspaceTarget(req); err != nil {
			return runPlan{}, err
		}
		// Runtimes that own their conversation context have no rewind primitive,
		// so running a replacement turn would leave that context inconsistent
		// with the visible history.
		if spec.Replacement != nil {
			return runPlan{}, apperror.New(apperror.CodeExternalAgentTurnReplacementUnsupported, nil)
		}

		plan, req, err := s.prepareExternal(ctx, req, dispatch.driver)
		if err != nil {
			return runPlan{}, err
		}

		exec := &externalExecutor{
			service:    s,
			plan:       plan,
			req:        req,
			prompt:     req.Query,
			unattended: spec.Unattended,
			timing:     newReasoningTimingTracker(nil),
			rc: resolvedContext{runConfig: native.RunConfig{
				RunID:    req.RunID,
				Identity: native.SessionContext{BotID: req.BotID, SessionID: req.ThreadID},
			}},
		}

		return runPlan{
			Exec:       exec,
			Req:        req,
			IdleEffort: req.ReasoningEffort,
			Lifecycle:  nil, // External runtimes have no context lifecycle owned by us.
		}, nil
	}

	ctx, req, err = s.prepareWorkspaceRequest(ctx, req)
	if err != nil {
		return runPlan{}, err
	}

	if spec.Replacement != nil && spec.Replacement.Preflight != nil {
		if err := spec.Replacement.Preflight(ctx); err != nil {
			return runPlan{}, err
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
		req, err = s.applyUserMessageHook(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				return runPlan{}, err
			}
			return runPlan{}, classifyUserMessageHookError(err)
		}
	}

	rc, req, err := s.resolve(ctx, req)
	if err != nil {
		return runPlan{}, fmt.Errorf("resolve: %w", err)
	}
	req.Query = rc.query
	req.RunID = rc.runConfig.RunID

	s.maybeGenerateSessionTitle(context.WithoutCancel(ctx), req, req.RawQuery)

	cfg := rc.runConfig
	cfg.LiveToolStream = true
	// Whether this run may park on a question follows the process's user input
	// port, not its delivery end: an interruption resumed from the last process
	// keeps asking the same way the inbound message that started it did.
	cfg.CanRequestUserInput = s.canDeliverUserInputStream()

	reasoningTiming := newReasoningTimingTracker(nil)
	var stepCommitter *agentStepCommitter
	if !spec.Unattended {
		stepCommitter = s.newAgentStepCommitter(ctx, req, rc)
		configureNativeReasoningTiming(&cfg, reasoningTiming, stepCommitter)
		if stepCommitter != nil {
			stepCommitter.bindContinuation(&cfg)
		}
	}
	cfg = s.prepareRunConfig(ctx, cfg)

	if req.OnModelPreferenceSettled != nil {
		req.OnModelPreferenceSettled()
	}

	exec := &nativeExecutor{
		agent:     s.agent,
		cfg:       cfg,
		rc:        rc,
		req:       req,
		committer: stepCommitter,
		timing:    reasoningTiming,
		replace:   spec.Replacement,
		service:   s,
	}

	return runPlan{
		Exec:       exec,
		Req:        req,
		IdleEffort: reasoningEffortForIdle(cfg),
		Lifecycle:  s.contextLifecycleTerminal(ctx, cfg),
	}, nil
}
