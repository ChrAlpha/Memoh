package application

import (
	"context"
	"slices"
	"testing"

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
	lifecycles contextLifecycleStore,
) (*Service, *lifecycleTurnAdmitter) {
	t.Helper()
	service := newACPLifecycleService(t, pool, messages, lifecycles)
	service.SetACPSessionPool(pool)
	runtime := &lifecycleTurnAdmitter{admission: lifecycleTestAdmission()}
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
		service.SetExternalRuntimes(scheduleSessionDriver(driver))
	}
	if service.sessionRuntime == nil {
		// The entry writes the run's terminal state, which needs an admitted
		// run: a schedule fire never runs without one.
		service.sessionRuntime = &lifecycleTurnAdmitter{admission: lifecycleTestAdmission()}
	}
	return service.TriggerSchedule(ctx, lifecycleTestBotID, payload, "")
}

// runScheduledACP drives one ACP schedule fire through the entry.
func runScheduledACP(ctx context.Context, service *Service, payload schedule.TriggerPayload) (schedule.TriggerResult, error) {
	return service.TriggerSchedule(ctx, lifecycleTestBotID, payload, "")
}

// historyRows is what survives in history: the rows written, less the ones
// removed.
func historyRows(messages *recordingMessageService) int {
	deleted := 0
	for _, ids := range messages.deleted {
		deleted += len(ids)
	}
	return len(messages.persisted) - deleted
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
	service, _ := scheduleLifecycleService(t, pool, &recordingMessageService{}, &recordingContextLifecycleStore{})
	driver := &incompleteScheduleDriver{}
	position := int64(7)
	spec := runSpec{
		Run: admittedRun{Handle: sessionruntime.RunHandle{BotID: lifecycleTestBotID, SessionID: lifecycleTestSessionID, RunID: lifecycleTestRunID, TurnID: "turn-9", FencingToken: 1}, TurnID: "turn-9", TurnPosition: &position},
		Req: ChatRequest{BotID: lifecycleTestBotID, ThreadID: lifecycleTestSessionID, RunID: lifecycleTestRunID},
	}
	setup := service.scheduleSetup(spec, schedule.TriggerPayload{
		FireID: "fire-1", ID: "schedule-1", SessionID: lifecycleTestSessionID, Command: "inspect", OwnerUserID: "user-1",
	}, lifecycleTestBotID, "", runtimeDispatch{kind: dispatchExternal, driver: driver})

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
