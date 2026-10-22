package plan

import (
	"context"
	"strings"

	"github.com/felinics/memoh/internal/agent/internal/drive"
	"github.com/felinics/memoh/internal/agent/internal/externalrun"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/schedule"
)

// attachCurrentTurnPrompt routes a trigger's rich prompt through Query so the
// context view classifies it as the live current request rather than history.
func attachCurrentTurnPrompt(cfg native.RunConfig, prompt string) native.RunConfig {
	cfg.Query = prompt
	return cfg
}

// schedulePrompt is the prompt a fire runs with.
func schedulePrompt(payload schedule.TriggerPayload) string {
	return native.GenerateSchedulePrompt(native.Schedule{
		ID:          payload.ID,
		Name:        payload.Name,
		Description: payload.Description,
		Pattern:     payload.Pattern,
		MaxCalls:    payload.MaxCalls,
		Command:     payload.Command,
	})
}

// ScheduleSetup resolves one fire's executor: the built-in agent, or the
// External Agent runtime the session runs on. It resolves around the request
// the driver handed it, which already carries the per-run wiring.
func (p *Planner) ScheduleSetup(spec drive.RunSpec, payload schedule.TriggerPayload, botID, token string, dispatch externalrun.Dispatch) func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
	return func(setupCtx context.Context, req runreq.ChatRequest) (drive.RunPlan, error) {
		req.RunHandle = spec.Run.Handle
		// The admitted turn's identity travels with the request: the step
		// committer refuses to arm without it, and a run without a committer
		// does not start.
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
		if err := p.admit.RecordResume(setupCtx, req); err != nil {
			return drive.RunPlan{}, err
		}
		if dispatch.Kind == externalrun.DispatchExternal {
			return p.externalSchedulePlan(setupCtx, req, spec, payload, dispatch.Driver)
		}
		return p.nativeSchedulePlan(setupCtx, req, payload)
	}
}

// nativeSchedulePlan resolves a scheduled run the built-in agent executes.
func (p *Planner) nativeSchedulePlan(ctx context.Context, req runreq.ChatRequest, payload schedule.TriggerPayload) (drive.RunPlan, error) {
	// The run acts for the bot's owner, who holds every permission, so there
	// is no principal to check; binding the workspace here puts the target on
	// the history rows the run writes.
	ctx, req, err := p.runconfig.BindRequestWorkspace(ctx, req, nil, true)
	if err != nil {
		return drive.RunPlan{}, err
	}
	rc, req, err := p.runconfig.Resolve(ctx, req)
	if err != nil {
		return drive.RunPlan{}, err
	}
	p.settleResolved(ctx, req, &rc)
	req.RunID = rc.RunConfig.RunID

	cfg := rc.RunConfig
	cfg.SessionType = sessionmode.Schedule
	cfg.Identity.ChannelIdentityID = strings.TrimSpace(payload.OwnerUserID)
	cfg.ContextScope.ChannelIdentityID = strings.TrimSpace(payload.OwnerUserID)
	cfg = attachCurrentTurnPrompt(cfg, schedulePrompt(payload))
	// Nobody is on the other end of a scheduled run, so it follows no tool
	// stream and cannot ask a question.
	return p.nativePlan(ctx, req, rc, cfg, nativeOptions{Steps: stepsFresh})
}

// externalSchedulePlan resolves a scheduled run an External Agent runtime
// executes. It prompts with the generated schedule prompt rather than the raw
// command, and nobody is there to answer a decision.
func (p *Planner) externalSchedulePlan(ctx context.Context, req runreq.ChatRequest, spec drive.RunSpec, payload schedule.TriggerPayload, driver external.Driver) (drive.RunPlan, error) {
	req.ModelQuery = schedulePrompt(payload)
	return p.externalRunPlan(ctx, req, spec, driver)
}
