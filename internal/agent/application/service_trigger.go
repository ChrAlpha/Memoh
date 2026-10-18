package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
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
	runCtx, admission, release, err := s.admit.Triggered(ctx, admit.Trigger{
		BotID: botID, ThreadID: payload.SessionID, InvocationID: scheduleInvocationID(payload),
		Submission: submission, View: viewFn, StopCause: outcome.ErrRunStopped,
	})
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
	dispatch, err := s.externalrun.ResolveDispatch(runCtx, runreq.ChatRequest{BotID: botID, ThreadID: payload.SessionID})
	if err != nil {
		return schedule.TriggerResult{}, errs.Recorded(err)
	}

	spec := drive.RunSpec{
		Run: drive.AdmittedRun{
			Handle:       admission.Handle,
			TurnID:       admission.TurnID,
			TurnPosition: &admission.TurnPosition,
			Mode:         sessionmode.Schedule,
		},
		Req:        runreq.ChatRequest{BotID: botID, ThreadID: payload.SessionID, RunID: admission.RunID},
		Delivery:   drive.NoDelivery{},
		Input:      outcome.InputSynthetic,
		Unattended: true,
	}
	spec.Setup = s.plan.ScheduleSetup(spec, payload, botID, token, dispatch)

	return s.runScheduleSpec(runCtx, spec)
}

// runScheduleSpec runs one scheduled fire's spec and answers in the vocabulary
// the schedule service reads: the completed round's text and usage, or the
// failure under the code the run ended with. A fire that lost its run reports
// the ownership loss, which the schedule service treats as a run it must not
// report on.
func (s *Service) runScheduleSpec(runCtx context.Context, spec drive.RunSpec) (schedule.TriggerResult, error) {
	result := s.driver.Drive(runCtx, spec)
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
