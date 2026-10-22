package agent

import (
	"context"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/lifecycle"
	acpagent "github.com/felinics/memoh/internal/agent/runtime/acp"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/schedule"
)

// scheduleLifecycleService is the ACP schedule fixture the driver needs: the
// runtime's driver registered where the entry resolves it, and an admitted run
// for the terminal write.
func scheduleLifecycleService(
	t *testing.T,
	pool acpagent.Prompter,
	messages messagepkg.Service,
	lifecycles lifecycle.Store,
	opts ...testOption,
) (*Service, *agenttest.LifecycleAdmitter) {
	t.Helper()
	runtime := &agenttest.LifecycleAdmitter{Admission: lifecycleTestAdmission()}
	service := newACPLifecycleService(t, pool, messages, lifecycles, append([]testOption{withTurnAdmitter(runtime)}, opts...)...)
	return service, runtime
}

// runScheduledExternal drives one scheduled fire through the entry, the way
// the schedule service does, so a test observes the run rather than the loop
// that used to execute it. The service names its driver with withScheduleDriver.
func runScheduledExternal(ctx context.Context, service *Service, payload schedule.TriggerPayload) (schedule.TriggerResult, error) {
	return service.TriggerSchedule(ctx, agenttest.LifecycleBotID, payload, "")
}

// withScheduleDriver registers driver under the runtime type the ACP session
// resolves to, which is the key the schedule entry looks up.
func withScheduleDriver(driver external.Driver) testOption {
	return withDrivers(runtimeTypeAlias{Driver: driver})
}

type runtimeTypeAlias struct {
	external.Driver
}

func (runtimeTypeAlias) RuntimeType() string { return session.RuntimeACPAgent }

type incompleteScheduleDriver struct {
	input external.PromptInput
}

func (*incompleteScheduleDriver) RuntimeType() string { return "codex" }

func (d *incompleteScheduleDriver) Prompt(_ context.Context, input external.PromptInput) (external.PromptResult, error) {
	d.input = input
	return external.PromptResult{Text: "partial", Output: []sdk.Message{sdk.AssistantMessage("partial")}}, nil
}
