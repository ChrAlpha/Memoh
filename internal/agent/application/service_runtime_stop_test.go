package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/schedule"
)

// stopDuringSetupDriver behaves like a driver the user stops while it is still
// setting up (starting its process, resuming its native session): it reports the
// cancellation as the runtime-unavailable failure that stage wraps errors in.
type stopDuringSetupDriver struct{ started chan struct{} }

func (stopDuringSetupDriver) RuntimeType() string { return "codex" }

func (d stopDuringSetupDriver) Prompt(ctx context.Context, _ external.PromptInput) (external.PromptResult, error) {
	close(d.started)
	<-ctx.Done()
	return external.PromptResult{}, external.Unavailable(ctx.Err())
}

// ignoreOutcome keeps the error of a WebSocket stream call.
func ignoreOutcome(_ outcome.RunOutcome, err error) error {
	return err
}

// A driver that stops answering while still setting up reports the idle
// cancellation as runtime-unavailable. The turn failed on the idle timeout, not
// on configuration, so the round is kept with agent.response_timeout.
func TestIdleTimeoutDuringDriverSetupKeepsAFailedRound(t *testing.T) {
	messages := &agenttest.RecordingMessages{}
	service := newACPLifecycleService(t, &recordingACPPrompter{}, messages, &agenttest.RecordingLifecycleStore{})
	service.streamIdleTimeout = 20 * time.Millisecond
	driver := stopDuringSetupDriver{started: make(chan struct{})}
	events := make(chan runreq.WSStreamEvent, 16)
	err := ignoreOutcome(service.streamExternalWebSend(context.Background(), driver, runreq.ChatRequest{
		BotID:    lifecycleTestBotID,
		ThreadID: lifecycleTestSessionID,
		RunID:    lifecycleTestRunID,
		Query:    "inspect",
	}, events, make(chan struct{})))
	if apperror.CodeOf(err) == apperror.CodeAgentResponseTimeout {
		t.Fatalf("the idle timeout was answered as a configuration failure: %v", err)
	}
	if len(messages.Deleted) != 0 || len(messages.Persisted) == 0 || messages.Persisted[0].Role != "user" {
		t.Fatalf("persisted = %+v deleted = %v, want the user message kept", messages.Persisted, messages.Deleted)
	}
	var failure string
	for _, ev := range drainAgentEvents(t, events) {
		if ev.Type == native.EventError {
			failure = ev.Code
		}
	}
	if failure != string(apperror.CodeAgentResponseTimeout) {
		t.Fatalf("failure event code = %q, want %s", failure, apperror.CodeAgentResponseTimeout)
	}
}

// The scheduled path keeps the same round for the same reason.
func TestScheduledIdleTimeoutDuringDriverSetupKeepsAFailedRound(t *testing.T) {
	messages := &agenttest.RecordingMessages{}
	service := newACPLifecycleService(t, &recordingACPPrompter{}, messages, &agenttest.RecordingLifecycleStore{})
	service.streamIdleTimeout = 20 * time.Millisecond
	driver := stopDuringSetupDriver{started: make(chan struct{})}
	_, err := runScheduledExternal(context.Background(), t, service, driver, schedule.TriggerPayload{
		FireID: "fire-1", ID: "schedule-1",
		SessionID: lifecycleTestSessionID, Command: "inspect", OwnerUserID: "user-1",
	})
	if apperror.CodeOf(err) != apperror.CodeAgentResponseTimeout {
		t.Fatalf("schedule error = %v, want %s", err, apperror.CodeAgentResponseTimeout)
	}
	if len(messages.Deleted) != 0 || len(messages.Persisted) == 0 {
		t.Fatalf("persisted = %+v deleted = %v, want the failed round kept", messages.Persisted, messages.Deleted)
	}
}

// A scheduled run whose runtime could not start leaves no round in history;
// a failure the runtime reports after it started keeps one. What matters is
// the history that survives, not whether a row was briefly written and then
// removed on the way there.
func TestScheduledRuntimeConfigurationFailureLeavesNoRound(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		code      apperror.Code
		keepRound bool
	}{
		{"runtime unavailable", external.Unavailable(errors.New("bridge down")), apperror.CodeExternalRuntimeUnavailable, false},
		{"dependency missing", &external.DependencyMissingError{DependencyID: "codex"}, apperror.CodeAgentDependencyMissing, false},
		{"control failed", external.Fail(external.FailureControlFailed, errors.New("set mode")), apperror.CodeRuntimeControlFailed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := &agenttest.RecordingMessages{}
			service := newACPLifecycleService(t, &recordingACPPrompter{}, messages, &agenttest.RecordingLifecycleStore{})
			driver := noticeTestDriver{kind: "codex", prompt: func(context.Context, external.PromptInput) (external.PromptResult, error) {
				return external.PromptResult{}, tc.err
			}}
			_, err := runScheduledExternal(context.Background(), t, service, driver, schedule.TriggerPayload{
				FireID: "fire-1", ID: "schedule-1",
				SessionID: lifecycleTestSessionID, Command: "inspect", OwnerUserID: "user-1",
			})
			if got := apperror.CodeOf(err); got != tc.code {
				t.Fatalf("schedule error code = %q, want %q: %v", got, tc.code, err)
			}
			if got := historyRows(messages); (got > 0) != tc.keepRound {
				t.Fatalf("surviving history rows = %d (persisted = %+v deleted = %v), want round kept = %v",
					got, messages.Persisted, messages.Deleted, tc.keepRound)
			}
		})
	}
}

// A scheduled run that hits its execution budget failed on the budget, even
// when the runtime reported the stop as being unavailable; the round is kept.
func TestScheduledExecutionTimeoutIsNotAConfigurationFailure(t *testing.T) {
	messages := &agenttest.RecordingMessages{}
	service := newACPLifecycleService(t, &recordingACPPrompter{}, messages, &agenttest.RecordingLifecycleStore{})
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	driver := noticeTestDriver{kind: "codex", prompt: func(context.Context, external.PromptInput) (external.PromptResult, error) {
		cancel(schedule.ErrExecutionTimeout)
		return external.PromptResult{TurnCompleted: true}, external.Unavailable(context.Canceled)
	}}
	_, err := runScheduledExternal(ctx, t, service, driver, schedule.TriggerPayload{
		FireID: "fire-1", ID: "schedule-1",
		SessionID: lifecycleTestSessionID, Command: "inspect", OwnerUserID: "user-1",
	})
	if got := apperror.CodeOf(err); got != apperror.CodeScheduleExecutionTimeout {
		t.Fatalf("schedule error code = %q, want %q: %v", got, apperror.CodeScheduleExecutionTimeout, err)
	}
	if len(messages.Deleted) != 0 || len(messages.Persisted) == 0 {
		t.Fatalf("persisted = %+v deleted = %v, want the failed round kept", messages.Persisted, messages.Deleted)
	}
}
