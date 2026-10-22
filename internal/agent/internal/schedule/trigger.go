package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/felinics/memoh/internal/agent/internal/admit"
	"github.com/felinics/memoh/internal/agent/internal/drive"
	"github.com/felinics/memoh/internal/agent/internal/outcome"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	chatview "github.com/felinics/memoh/internal/agent/view"
	"github.com/felinics/memoh/internal/errs"
	schedulepkg "github.com/felinics/memoh/internal/schedule"
)

// Trigger executes a scheduled command via the internal agent.
func (r *Runner) Trigger(ctx context.Context, botID string, payload schedulepkg.TriggerPayload, token string) (schedulepkg.TriggerResult, error) {
	endActiveTurn, beginErr := r.activeTurns.Begin()
	if beginErr != nil {
		return schedulepkg.TriggerResult{}, beginErr
	}
	defer endActiveTurn()
	if strings.TrimSpace(payload.FireID) == "" {
		return schedulepkg.TriggerResult{}, errors.New("schedule fire id is required")
	}
	if strings.TrimSpace(botID) == "" {
		return schedulepkg.TriggerResult{}, errors.New("bot id is required")
	}
	if strings.TrimSpace(payload.Command) == "" {
		return schedulepkg.TriggerResult{}, errors.New("schedule command is required")
	}

	submission, err := json.Marshal(scheduleSubmission{
		Kind:       "schedule",
		ScheduleID: strings.TrimSpace(payload.ID),
		Command:    payload.Command,
	})
	if err != nil {
		return schedulepkg.TriggerResult{}, err
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
	runCtx, admission, release, err := r.admit.Triggered(ctx, admit.Trigger{
		BotID: botID, ThreadID: payload.SessionID, InvocationID: scheduleInvocationID(payload),
		Submission: submission, View: viewFn, StopCause: outcome.ErrRunStopped,
	})
	if errors.Is(err, sessionruntime.ErrSessionBusy) {
		// A fire that cannot take the thread's slot has no value once the next
		// one is due, so it is dropped rather than retried here.
		return schedulepkg.TriggerResult{}, fmt.Errorf("%w: %w", schedulepkg.ErrSessionBusy, err)
	}
	if err != nil {
		return schedulepkg.TriggerResult{}, err
	}
	// The driver names the run's outcome and writes its terminal state, so the
	// admission hands back only the release: two writers would race over one
	// run's ending.
	defer release()

	// Runtime sessions (ACP, codex, claude-code) must never silently degrade
	// to the built-in model: resolve the driver and run the scheduled turn
	// through it.
	dispatch, err := r.externalrun.ResolveDispatch(runCtx, runreq.ChatRequest{BotID: botID, ThreadID: payload.SessionID})
	if err != nil {
		return schedulepkg.TriggerResult{}, errs.Recorded(err)
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
	spec.Setup = r.plan.ScheduleSetup(spec, payload, botID, token, dispatch)

	return r.runSpec(runCtx, spec)
}

// runSpec runs one scheduled fire's spec and answers in the vocabulary
// the schedule service reads: the completed round's text and usage, or the
// failure under the code the run ended with. A fire that lost its run reports
// the ownership loss, which the schedule service treats as a run it must not
// report on.
func (r *Runner) runSpec(runCtx context.Context, spec drive.RunSpec) (schedulepkg.TriggerResult, error) {
	result := r.driver.Drive(runCtx, spec)
	switch result.End {
	case outcome.EndCompleted:
		return schedulepkg.TriggerResult{
			Status:     "ok",
			Text:       result.Summary.Text,
			UsageBytes: result.Summary.Usage,
			ModelID:    result.Summary.ModelID,
		}, nil
	case outcome.EndStopped:
		return schedulepkg.TriggerResult{}, errs.Recorded(outcome.AgentAbortCause(runCtx))
	case outcome.EndOwnershipLost:
		return schedulepkg.TriggerResult{}, sessionruntime.ErrRunOwnershipLost
	default:
		if result.Failure == nil {
			return schedulepkg.TriggerResult{}, errs.Recorded(errors.New("schedule run ended without an outcome"))
		}
		// A schedule log stores the text a fire returns and serves it back, so
		// the failure reads as its code; the agent's own words stay behind it
		// for the run's result record.
		return schedulepkg.TriggerResult{}, result.Failure.Recorded()
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
func scheduleInvocationID(payload schedulepkg.TriggerPayload) string {
	return "schedule:" + strings.TrimSpace(payload.ID) + ":" + strings.TrimSpace(payload.FireID)
}
