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
