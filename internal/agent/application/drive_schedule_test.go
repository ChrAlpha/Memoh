package application

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/schedule"
)

// scheduleLifecycleService is the ACP schedule fixture the driver needs: the
// runtime's driver registered where the entry resolves it, and an admitted run
// for the terminal write.
func scheduleLifecycleService(
	t *testing.T,
	pool acpPrompter,
	messages messagepkg.Service,
	lifecycles lifecycle.Store,
) (*Service, *agenttest.LifecycleAdmitter) {
	t.Helper()
	service := newACPLifecycleService(t, pool, messages, lifecycles)
	registerACPPool(service, pool)
	runtime := &agenttest.LifecycleAdmitter{Admission: lifecycleTestAdmission()}
	service.sessionRuntime = runtime
	return service, runtime
}

// runScheduledExternal drives one scheduled fire through the entry, the way
// the schedule service does, so a test observes the run rather than the loop
// that used to execute it. driver is registered for the session's runtime type.
func runScheduledExternal(ctx context.Context, t *testing.T, service *Service, driver external.Driver, payload schedule.TriggerPayload) (schedule.TriggerResult, error) {
	t.Helper()
	if driver != nil {
		// A session on an external runtime resolves the driver by the type the
		// session reports, which is what the entry looks up.
		registerExternalDrivers(service, scheduleSessionDriver(driver))
	}
	if service.sessionRuntime == nil {
		// The entry writes the run's terminal state, which needs an admitted
		// run: a schedule fire never runs without one.
		service.sessionRuntime = &agenttest.LifecycleAdmitter{Admission: lifecycleTestAdmission()}
	}
	return service.TriggerSchedule(ctx, agenttest.LifecycleBotID, payload, "")
}

// runScheduledACP drives one ACP schedule fire through the entry.
func runScheduledACP(ctx context.Context, service *Service, payload schedule.TriggerPayload) (schedule.TriggerResult, error) {
	return service.TriggerSchedule(ctx, agenttest.LifecycleBotID, payload, "")
}

// historyRows is what survives in history: the rows written, less the ones
// removed.
func historyRows(messages *agenttest.RecordingMessages) int {
	deleted := 0
	for _, ids := range messages.Deleted {
		deleted += len(ids)
	}
	return len(messages.Persisted) - deleted
}

// scheduleSessionDriver registers a test driver under the runtime type the
// ACP session resolves to, which is the key the entry looks up.
func scheduleSessionDriver(driver external.Driver) external.Driver {
	return runtimeTypeAlias{Driver: driver}
}

type runtimeTypeAlias struct {
	external.Driver
}

func (runtimeTypeAlias) RuntimeType() string { return session.RuntimeACPAgent }

var (
	_ = sessionruntime.RunStatusCompleted
	_ = slices.Contains[[]string]
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
	spec := runSpec{
		Run: admittedRun{Handle: sessionruntime.RunHandle{BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID, RunID: agenttest.LifecycleRunID, TurnID: "turn-9", FencingToken: 1}, TurnID: "turn-9", TurnPosition: &position},
		Req: runreq.ChatRequest{BotID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID, RunID: agenttest.LifecycleRunID},
	}
	setup := service.scheduleSetup(spec, schedule.TriggerPayload{
		FireID: "fire-1", ID: "schedule-1", SessionID: agenttest.LifecycleSessionID, Command: "inspect", OwnerUserID: "user-1",
	}, agenttest.LifecycleBotID, "", runtimeDispatch{kind: dispatchExternal, driver: driver})

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
	service := newACPLifecycleService(t, &recordingACPPrompter{}, messages, &agenttest.RecordingLifecycleStore{})
	var rolesAtPrompt []string
	driver := noticeTestDriver{kind: "codex", prompt: func(_ context.Context, _ external.PromptInput) (external.PromptResult, error) {
		for _, input := range messages.Persisted {
			rolesAtPrompt = append(rolesAtPrompt, input.Role)
		}
		return external.PromptResult{TurnCompleted: true}, nil
	}}

	result, err := runScheduledExternal(context.Background(), t, service, driver, schedule.TriggerPayload{
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
	service := newACPLifecycleService(t, &recordingACPPrompter{}, messages, &agenttest.RecordingLifecycleStore{})
	driver := noticeTestDriver{kind: "codex", prompt: func(context.Context, external.PromptInput) (external.PromptResult, error) {
		return external.PromptResult{TurnCompleted: true}, nil
	}}

	_, _ = runScheduledExternal(context.Background(), t, service, driver, schedule.TriggerPayload{
		FireID: "fire-1", ID: "schedule-1",
		SessionID: agenttest.LifecycleSessionID, Command: "inspect", OwnerUserID: "user-1",
	})

	for _, deleted := range messages.Deleted {
		if len(deleted) != 0 {
			t.Fatalf("deleted rows = %v, want the leading user message kept after a failed round write", messages.Deleted)
		}
	}
}
