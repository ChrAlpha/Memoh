package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	chatview "github.com/felinics/memoh/internal/agent/view"
	"github.com/felinics/memoh/internal/errs"
	"github.com/felinics/memoh/internal/schedule"
)

// TriggerSchedule executes a scheduled command via the internal agent.
func (s *Service) TriggerSchedule(ctx context.Context, botID string, payload schedule.TriggerPayload, token string) (schedule.TriggerResult, error) {
	endActiveTurn, beginErr := s.activeTurns.Begin()
	if beginErr != nil {
		return schedule.TriggerResult{}, beginErr
	}
	defer endActiveTurn()
	if strings.TrimSpace(payload.FireID) == "" {
		return schedule.TriggerResult{}, errors.New("schedule fire id is required")
	}
	if strings.TrimSpace(botID) == "" {
		return schedule.TriggerResult{}, errors.New("bot id is required")
	}
	if strings.TrimSpace(payload.Command) == "" {
		return schedule.TriggerResult{}, errors.New("schedule command is required")
	}

	submission, err := json.Marshal(scheduleSubmission{
		Kind:       "schedule",
		ScheduleID: strings.TrimSpace(payload.ID),
		Command:    payload.Command,
	})
	if err != nil {
		return schedule.TriggerResult{}, err
	}
	// Project the command as the run's request user turn so subscribers (an
	// open web session, any future thread subscriber) see what fired this run
	// while it is still executing — not only after it finishes. Text must equal
	// the persisted user message, otherwise the bubble's content would visibly
	// change when the runtime projection hands over to the database at run end.
	viewFn := func(handle sessionruntime.RunHandle) *sessionruntime.RunAdmissionView {
		return &sessionruntime.RunAdmissionView{
			RequestUserTurn: &chatview.UITurn{
				TurnID:    handle.TurnID,
				Role:      "user",
				Text:      payload.Command,
				Timestamp: time.Now(),
			},
		}
	}
	runCtx, admission, _, release, err := s.admitTriggeredRun(ctx, sessionmode.Schedule, botID, payload.SessionID, scheduleInvocationID(payload), submission, viewFn, outcome.ErrRunStopped)
	if errors.Is(err, sessionruntime.ErrSessionBusy) {
		// A fire that cannot take the thread's slot has no value once the next
		// one is due, so it is dropped rather than retried here.
		return schedule.TriggerResult{}, fmt.Errorf("%w: %w", schedule.ErrSessionBusy, err)
	}
	if err != nil {
		return schedule.TriggerResult{}, err
	}
	// The driver names the run's outcome and writes its terminal state, so the
	// admission hands back only the release: two writers would race over one
	// run's ending.
	defer release()

	// Runtime sessions (ACP, codex, claude-code) must never silently degrade
	// to the built-in model: resolve the driver and run the scheduled turn
	// through it.
	dispatch, err := s.resolveRuntimeDispatch(runCtx, runreq.ChatRequest{BotID: botID, ThreadID: payload.SessionID})
	if err != nil {
		return schedule.TriggerResult{}, errs.Recorded(err)
	}

	spec := runSpec{
		Run: admittedRun{
			Handle:       admission.Handle,
			TurnID:       admission.TurnID,
			TurnPosition: &admission.TurnPosition,
			Mode:         sessionmode.Schedule,
		},
		Req:        runreq.ChatRequest{BotID: botID, ThreadID: payload.SessionID, RunID: admission.RunID},
		Delivery:   noDelivery{},
		Input:      outcome.InputSynthetic,
		Unattended: true,
	}
	spec.Setup = s.scheduleSetup(spec, payload, botID, token, dispatch)

	return s.runScheduleSpec(runCtx, spec)
}

// runScheduleSpec runs one scheduled fire's spec and answers in the vocabulary
// the schedule service reads: the completed round's text and usage, or the
// failure under the code the run ended with. A fire that lost its run reports
// the ownership loss, which the schedule service treats as a run it must not
// report on.
func (s *Service) runScheduleSpec(runCtx context.Context, spec runSpec) (schedule.TriggerResult, error) {
	result := s.drive(runCtx, spec)
	switch result.End {
	case outcome.EndCompleted:
		return schedule.TriggerResult{
			Status:     "ok",
			Text:       result.Summary.Text,
			UsageBytes: result.Summary.Usage,
			ModelID:    result.Summary.ModelID,
		}, nil
	case outcome.EndStopped:
		return schedule.TriggerResult{}, errs.Recorded(outcome.AgentAbortCause(runCtx))
	case outcome.EndOwnershipLost:
		return schedule.TriggerResult{}, sessionruntime.ErrRunOwnershipLost
	default:
		if result.Failure == nil {
			return schedule.TriggerResult{}, errs.Recorded(errors.New("schedule run ended without an outcome"))
		}
		// A schedule log stores the text a fire returns and serves it back, so
		// the failure reads as its code; the agent's own words stay behind it
		// for the run's result record.
		return schedule.TriggerResult{}, result.Failure.Recorded()
	}
}

// attachCurrentTurnPrompt routes a trigger's rich prompt through Query so the
// context view classifies it as the live current request rather than history.
func attachCurrentTurnPrompt(cfg native.RunConfig, prompt string) native.RunConfig {
	cfg.Query = prompt
	return cfg
}

// scheduleSetup resolves one fire's executor: the built-in agent, or the
// External Agent runtime the session runs on. It resolves around the request
// the driver handed it, which already carries the per-run wiring.
func (s *Service) scheduleSetup(spec runSpec, payload schedule.TriggerPayload, botID, token string, dispatch runtimeDispatch) func(context.Context, runreq.ChatRequest) (runPlan, error) {
	return func(setupCtx context.Context, req runreq.ChatRequest) (runPlan, error) {
		req.RunHandle = spec.Run.Handle
		// The admitted turn's identity travels with the request: the step
		// committer refuses to arm without it, and a scheduled run without one
		// would persist only as a terminal snapshot.
		req.TurnID = spec.Run.TurnID
		req.TurnPosition = spec.Run.TurnPosition
		req.Query = payload.Command
		req.RawQuery = payload.Command
		req.UserID = payload.OwnerUserID
		req.Token = token
		req.Model = payload.ModelID
		req.RuntimeModelID = payload.RuntimeModelID
		req.ReasoningEffort = payload.ReasoningEffort
		req.SessionType = sessionmode.Schedule
		req.ChatID = botID
		// External schedules identify the triggering user as the channel
		// identity, matching the native path (nativeSchedulePlan's cfg.Identity).
		req.SourceChannelIdentityID = payload.OwnerUserID
		if err := s.recordRunResumeContext(setupCtx, req); err != nil {
			return runPlan{}, err
		}
		if dispatch.kind == dispatchExternal {
			return s.externalSchedulePlan(setupCtx, req, spec, payload, dispatch.driver)
		}
		return s.nativeSchedulePlan(setupCtx, req, payload)
	}
}

// nativeSchedulePlan resolves a scheduled run the built-in agent executes.
func (s *Service) nativeSchedulePlan(ctx context.Context, req runreq.ChatRequest, payload schedule.TriggerPayload) (runPlan, error) {
	// The run acts for the bot's owner, who holds every permission, so there
	// is no principal to check; binding the workspace here puts the target on
	// the history rows the run writes.
	ctx, req, err := s.bindRequestWorkspace(ctx, req, nil, true)
	if err != nil {
		return runPlan{}, err
	}
	rc, req, err := s.resolve(ctx, req)
	if err != nil {
		return runPlan{}, err
	}
	req.RunID = rc.RunConfig.RunID

	cfg := rc.RunConfig
	cfg.SessionType = sessionmode.Schedule
	cfg.Identity.ChannelIdentityID = strings.TrimSpace(payload.OwnerUserID)
	cfg.ContextScope.ChannelIdentityID = strings.TrimSpace(payload.OwnerUserID)
	cfg = attachCurrentTurnPrompt(cfg, native.GenerateSchedulePrompt(native.Schedule{
		ID:          payload.ID,
		Name:        payload.Name,
		Description: payload.Description,
		Pattern:     payload.Pattern,
		MaxCalls:    payload.MaxCalls,
		Command:     payload.Command,
	}))
	// Nobody is on the other end of a scheduled run.
	cfg.LiveToolStream = false
	cfg.CanRequestUserInput = false
	cfg = s.prepareRunConfig(ctx, cfg)

	timing := newReasoningTimingTracker(nil)
	committer := s.newAgentStepCommitter(ctx, req, rc)
	configureNativeReasoningTiming(&cfg, timing, committer)
	if committer != nil {
		committer.bindContinuation(&cfg)
	}
	s.bindStepCompaction(&cfg, req, committer)
	return runPlan{
		Exec: &nativeExecutor{
			agent:     s.agent,
			cfg:       cfg,
			rc:        rc,
			req:       req,
			committer: committer,
			timing:    timing,
			service:   s,
		},
		Req:        req,
		IdleEffort: reasoningEffortForIdle(cfg),
		Lifecycle:  s.contextLifecycleTerminal(ctx, cfg),
	}, nil
}

// externalSchedulePlan resolves a scheduled run an External Agent runtime
// executes. It prompts with the generated schedule prompt rather than the raw
// command, and nobody is there to answer a decision.
func (s *Service) externalSchedulePlan(ctx context.Context, req runreq.ChatRequest, spec runSpec, payload schedule.TriggerPayload, driver external.Driver) (runPlan, error) {
	prompt := native.GenerateSchedulePrompt(native.Schedule{
		ID:          payload.ID,
		Name:        payload.Name,
		Description: payload.Description,
		Pattern:     payload.Pattern,
		MaxCalls:    payload.MaxCalls,
		Command:     payload.Command,
	})
	req.ModelQuery = prompt
	return s.externalRunPlan(ctx, req, spec, driver)
}

// scheduleSubmission is the canonical fingerprint input for a triggered turn.
// It carries what the trigger asked for and nothing about when it ran, so
// re-running one tick's work is recognized as the same submission rather than
// a new one.
type scheduleSubmission struct {
	Kind       string `json:"kind"`
	ScheduleID string `json:"schedule_id"`
	Command    string `json:"command"`
}

// scheduleInvocationID identifies one persisted fire, independently of its target session.
func scheduleInvocationID(payload schedule.TriggerPayload) string {
	return "schedule:" + strings.TrimSpace(payload.ID) + ":" + strings.TrimSpace(payload.FireID)
}
