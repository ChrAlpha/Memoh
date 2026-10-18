package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/web"
	"github.com/felinics/memoh/internal/apperror"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
	"github.com/felinics/memoh/internal/settings"
)

func TestAgentStreamFailureConversion(t *testing.T) {
	t.Parallel()

	overloaded := &sdk.APIError{Provider: "anthropic-messages", StatusCode: 529, Kind: sdk.KindServerError, Message: "SECRET overloaded"}
	for _, tc := range []struct {
		name  string
		event native.StreamEvent
		code  apperror.Code
	}{
		{
			name:  "catalogued code",
			event: native.StreamEvent{Type: native.EventError, Code: string(apperror.CodeContextBudgetUnsatisfied), Cause: overloaded},
			code:  apperror.CodeContextBudgetUnsatisfied,
		},
		{
			name:  "uncatalogued code",
			event: native.StreamEvent{Type: native.EventError, Code: "not.a.code", Cause: overloaded},
			code:  apperror.CodeAgentProviderOverloaded,
		},
		{
			name:  "provider answer",
			event: native.StreamEvent{Type: native.EventError, Cause: overloaded},
			code:  apperror.CodeAgentProviderOverloaded,
		},
		{
			name:  "runtime failure",
			event: native.StreamEvent{Type: native.EventError, Cause: errors.New("checkpoint steered model call: SECRET")},
			code:  apperror.CodeAgentResponseInterrupted,
		},
		{
			name:  "no cause",
			event: native.StreamEvent{Type: native.EventError},
			code:  apperror.CodeAgentResponseInterrupted,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			failure := outcome.AgentStreamFailure(tc.event)
			if got := apperror.CodeOf(failure); got != tc.code {
				t.Fatalf("code = %q, want %q", got, tc.code)
			}
			if cause := apperror.CauseOf(failure); !errors.Is(cause, tc.event.Cause) {
				t.Fatalf("cause = %v, want the event's cause %v", cause, tc.event.Cause)
			}
		})
	}
	if err := outcome.AgentStreamFailure(native.StreamEvent{Type: native.EventTextDelta}); err != nil {
		t.Fatalf("agentStreamFailure(text delta) = %v, want nil", err)
	}
}

// The cause of a failure never leaves the application: the published event
// carries the code and its catalog detail, and nothing of the cause.
func TestPublicAgentStreamEventRedactsPrivateFailure(t *testing.T) {
	for _, cause := range []error{
		errors.New("SECRET provider payload"),
		&sdk.APIError{Provider: "openai-completions", StatusCode: 401, Kind: sdk.KindAuthentication, Message: "Incorrect API key provided: SECRET", RequestID: "req_SECRET"},
		errors.New("SECRET window"),
	} {
		event := outcome.PublicAgentStreamEvent(native.StreamEvent{Type: native.EventError, Cause: cause})
		definition, ok := apperror.Lookup(apperror.Code(event.Code))
		if !ok || event.Error != definition.Detail || event.Cause != nil {
			t.Fatalf("public event = %#v, want a catalogued code with its detail", event)
		}
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal public event: %v", err)
		}
		if strings.Contains(string(data), "SECRET") {
			t.Fatalf("private detail leaked: %s", data)
		}
	}
}

func TestAgentFailureStreamEventExposesOnlyStablePublicContract(t *testing.T) {
	t.Parallel()

	t.Run("timeout", func(t *testing.T) {
		cause := apperror.Wrap(apperror.CodeAgentResponseTimeout, errors.New("SECRET provider timeout"), nil)
		event := outcome.AgentFailureStreamEvent(cause)
		if event.Type != native.EventError || event.Code != string(apperror.CodeAgentResponseTimeout) {
			t.Fatalf("timeout event = %#v", event)
		}
		definition, _ := apperror.Lookup(apperror.CodeAgentResponseTimeout)
		if event.Error != definition.Detail {
			t.Fatalf("timeout detail = %q, want %q", event.Error, definition.Detail)
		}
		if strings.Contains(event.Error, "SECRET") {
			t.Fatalf("private timeout cause leaked: %q", event.Error)
		}
	})

	t.Run("uncatalogued failure", func(t *testing.T) {
		event := outcome.AgentFailureStreamEvent(errors.New("SECRET provider response"))
		if event.Code != string(apperror.CodeRuntimeRunFailed) {
			t.Fatalf("fallback code = %q", event.Code)
		}
		if strings.Contains(event.Error, "SECRET") {
			t.Fatalf("private failure cause leaked: %q", event.Error)
		}
	})
}

// A runtime that owns its conversation context has no rewind, so a retry of
// one of its turns is refused before the runtime is asked anything.
func TestWebRetryRejectsTurnReplacementForACP(t *testing.T) {
	t.Parallel()

	pool := &recordingACPPrompter{}
	resolver := newTestService(t, Deps{
		Sessions: acpRuntimeSessionServiceForTest("user-1"),
		Messages: driveReplaceableHistory(t),
	}, withTurnAdmitter(agenttest.NewScriptedAdmitter()), withACPPool(pool))
	admission, err := resolver.AdmitWebRun(context.Background(), web.AdmitInput{
		BotID: "bot-1", SessionID: "session-1", InvocationID: "retry-1", Payload: []byte(`{"kind":"retry_message"}`),
	})
	if err != nil {
		t.Fatalf("AdmitWebRun() error = %v", err)
	}
	delivery := newWebTestDelivery()
	resolver.RunWebRetry(admission, web.RetryLatestMessageInput{
		BotID: "bot-1", SessionID: "session-1", TargetTurnID: driveReplacedTurnID,
	}, delivery)

	if _, ended := delivery.wait(t); ended.ErrorCode != string(apperror.CodeExternalAgentTurnReplacementUnsupported) {
		t.Fatalf("ended = %+v, want %q", ended, apperror.CodeExternalAgentTurnReplacementUnsupported)
	}
	if pool.calls != 0 {
		t.Fatalf("ACP pool calls = %d, want none for a refused replacement", pool.calls)
	}
}

func TestHasVisibleAgentStreamOutputIgnoresLifecycleOnlyEvents(t *testing.T) {
	t.Parallel()

	cases := []native.StreamEvent{
		{Type: native.EventTextStart},
		{Type: native.EventTextDelta, Delta: "  \n\t"},
		{Type: native.EventTextEnd},
		{Type: native.EventReasoningStart},
		{Type: native.EventReasoningDelta, Delta: ""},
		{Type: native.EventReasoningEnd},
		{Type: native.EventAttachment},
		{Type: native.EventAgentAbort},
	}
	for _, event := range cases {
		if outcome.HasVisibleAgentStreamOutput(event) {
			t.Fatalf("event %q unexpectedly counted as visible", event.Type)
		}
	}

	visible := []native.StreamEvent{
		{Type: native.EventTextDelta, Delta: "hello"},
		{Type: native.EventReasoningDelta, Delta: "thinking"},
		{Type: native.EventAttachment, Attachments: []native.FileAttachment{{Path: "/tmp/a.png"}}},
		{Type: native.EventToolCallStart},
		{Type: native.EventUserInputRequest},
	}
	for _, event := range visible {
		if !outcome.HasVisibleAgentStreamOutput(event) {
			t.Fatalf("event %q unexpectedly counted as invisible", event.Type)
		}
	}
}

func TestPersistTerminalSnapshotHonorsSkipMemoryExtraction(t *testing.T) {
	t.Parallel()

	memory := &agenttest.MemoryProvider{AfterChat: make(chan memprovider.AfterChatRequest, 2)}
	registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
	registry.Register(agenttest.MemoryProviderID, memory)
	resolver := newTestService(t, Deps{
		Messages: &agenttest.RecordingMessages{},
		Settings: settings.NewService(slog.New(slog.DiscardHandler), &agenttest.MemorySettingsQueries{}, nil, nil),
		Memory:   registry,
	})

	req := runreq.ChatRequest{
		BotID:    agenttest.MemoryBotID,
		ThreadID: "session-1",
		Query:    "hello",
	}
	if _, err := resolver.persist.TerminalSnapshotResult(
		context.Background(),
		req,
		runreq.ResolvedContext{},
		persist.TerminalSnapshot{
			SDKMessages:   []sdk.Message{sdk.AssistantMessage("pong")},
			VisibleOutput: true,
		},
	); err != nil {
		t.Fatalf("persistTerminalSnapshot returned error: %v", err)
	}
	select {
	case <-memory.AfterChat:
	case <-time.After(time.Second):
		t.Fatal("expected ordinary terminal snapshot to write memory")
	}

	req.SkipMemoryExtraction = true
	if _, err := resolver.persist.TerminalSnapshotResult(
		context.Background(),
		req,
		runreq.ResolvedContext{},
		persist.TerminalSnapshot{
			SDKMessages:   []sdk.Message{sdk.AssistantMessage("done")},
			VisibleOutput: true,
		},
	); err != nil {
		t.Fatalf("persistTerminalSnapshot returned error with skip memory: %v", err)
	}
	select {
	case got := <-memory.AfterChat:
		t.Fatalf("expected skip memory extraction to suppress memory write, got %#v", got)
	case <-time.After(100 * time.Millisecond):
	}
}
