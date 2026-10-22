package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/externalrun"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/schedule"
)

// The admitted turn's identity has to reach the request the schedule setup
// builds. Without it the step committer refuses to arm and a scheduled run
// persists only a terminal snapshot instead of a step at a time.
func TestScheduleSetupCarriesTheAdmittedTurnIdentity(t *testing.T) {
	t.Parallel()

	pool := &recordingACPPrompter{}
	service, _ := scheduleLifecycleService(t, pool, &agenttest.RecordingMessages{}, &agenttest.RecordingLifecycleStore{})
	driver := &incompleteScheduleDriver{}
	position := int64(7)
	spec := drive.RunSpec{
		Run: drive.AdmittedRun{Handle: sessionruntime.RunHandle{BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID, RunID: agenttest.LifecycleRunID, TurnID: "turn-9", FencingToken: 1}, TurnID: "turn-9", TurnPosition: &position},
		Req: runreq.ChatRequest{BotID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID, RunID: agenttest.LifecycleRunID},
	}
	setup := service.plan.ScheduleSetup(spec, schedule.TriggerPayload{
		FireID: "fire-1", ID: "schedule-1", SessionID: agenttest.LifecycleSessionID, Command: "inspect", OwnerUserID: "user-1",
	}, agenttest.LifecycleBotID, "", externalrun.Dispatch{Kind: externalrun.DispatchExternal, Driver: driver})

	plan, err := setup(context.Background(), spec.Req)
	if err != nil {
		t.Fatalf("schedule setup: %v", err)
	}
	if plan.Req.TurnID != "turn-9" {
		t.Fatalf("plan request turn id = %q, want the admitted turn", plan.Req.TurnID)
	}
	if plan.Req.TurnPosition == nil || *plan.Req.TurnPosition != position {
		t.Fatalf("plan request turn position = %v, want %d", plan.Req.TurnPosition, position)
	}
}

// The user's message is written before the runtime is prompted. A subscriber
// sees what was sent while the turn runs, the runtime's decision rows have a
// request row to anchor to, and — when the runtime refuses the turn outright —
// there is one row to hand back rather than a message that was never written.
func TestScheduledExternalWritesTheLeadingUserMessageBeforePrompting(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	var rolesAtPrompt []string
	driver := noticeTestDriver{kind: "codex", prompt: func(_ context.Context, _ external.PromptInput) (external.PromptResult, error) {
		for _, input := range messages.Persisted {
			rolesAtPrompt = append(rolesAtPrompt, input.Role)
		}
		return external.PromptResult{TurnCompleted: true}, nil
	}}
	service := newACPLifecycleService(t, &recordingACPPrompter{}, messages, &agenttest.RecordingLifecycleStore{}, withScheduleDriver(driver))

	result, err := runScheduledExternal(context.Background(), service, schedule.TriggerPayload{
		FireID: "fire-1", ID: "schedule-1",
		SessionID: agenttest.LifecycleSessionID, Command: "inspect", OwnerUserID: "user-1",
	})
	if err != nil {
		t.Fatalf("scheduled fire: %v", err)
	}
	_ = result
	if len(rolesAtPrompt) == 0 || rolesAtPrompt[0] != "user" {
		t.Fatalf("history when the runtime was prompted = %v, want the user message already written", rolesAtPrompt)
	}
}

// A round whose history write failed is a round the runtime may still remember:
// the ending repairs the driver rather than rewriting the visible history, so
// the user message the turn wrote stays. Only a turn the runtime refused
// outright hands it back.
func TestScheduledExternalKeepsTheLeadingUserMessageWhenTheRoundFailsToWrite(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{RoundPersistErr: errors.New("history unavailable")}
	driver := noticeTestDriver{kind: "codex", prompt: func(context.Context, external.PromptInput) (external.PromptResult, error) {
		return external.PromptResult{TurnCompleted: true}, nil
	}}
	service := newACPLifecycleService(t, &recordingACPPrompter{}, messages, &agenttest.RecordingLifecycleStore{}, withScheduleDriver(driver))

	_, _ = runScheduledExternal(context.Background(), service, schedule.TriggerPayload{
		FireID: "fire-1", ID: "schedule-1",
		SessionID: agenttest.LifecycleSessionID, Command: "inspect", OwnerUserID: "user-1",
	})

	for _, deleted := range messages.Deleted {
		if len(deleted) != 0 {
			t.Fatalf("deleted rows = %v, want the leading user message kept after a failed round write", messages.Deleted)
		}
	}
}
