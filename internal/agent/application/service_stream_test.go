package application

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
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
			failure := agentStreamFailure(tc.event)
			if got := apperror.CodeOf(failure); got != tc.code {
				t.Fatalf("code = %q, want %q", got, tc.code)
			}
			if cause := apperror.CauseOf(failure); !errors.Is(cause, tc.event.Cause) {
				t.Fatalf("cause = %v, want the event's cause %v", cause, tc.event.Cause)
			}
		})
	}
	if err := agentStreamFailure(native.StreamEvent{Type: native.EventTextDelta}); err != nil {
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
		event := publicAgentStreamEvent(native.StreamEvent{Type: native.EventError, Cause: cause})
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
		event := agentFailureStreamEvent(cause)
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
		event := agentFailureStreamEvent(errors.New("SECRET provider response"))
		if event.Code != string(apperror.CodeRuntimeRunFailed) {
			t.Fatalf("fallback code = %q", event.Code)
		}
		if strings.Contains(event.Error, "SECRET") {
			t.Fatalf("private failure cause leaked: %q", event.Error)
		}
	})
}

type recordingMessageService struct {
	persisted               []messagepkg.PersistInput
	persistErr              error
	roundPersistErr         error
	roundOptions            []messagepkg.RoundPersistenceOptions
	replaced                int
	replacementTurnID       string
	replacementTurnPosition *int64
	deleted                 [][]string
}

func (s *recordingMessageService) Persist(_ context.Context, input messagepkg.PersistInput) (messagepkg.Message, error) {
	if s.persistErr != nil {
		return messagepkg.Message{}, s.persistErr
	}
	s.persisted = append(s.persisted, input)
	return messagepkg.Message{ID: "message-id", SessionID: input.SessionID, Role: input.Role, Content: input.Content, DisplayContent: input.DisplayText}, nil
}

func (s *recordingMessageService) PersistRound(_ context.Context, inputs []messagepkg.PersistInput, options messagepkg.RoundPersistenceOptions) ([]messagepkg.Message, bool, error) {
	s.roundOptions = append(s.roundOptions, options)
	if s.roundPersistErr != nil {
		return nil, true, s.roundPersistErr
	}
	if s.persistErr != nil {
		return nil, true, s.persistErr
	}
	persisted := make([]messagepkg.Message, 0, len(inputs))
	for _, input := range inputs {
		s.persisted = append(s.persisted, input)
		persisted = append(persisted, messagepkg.Message{
			ID:             "message-id",
			SessionID:      input.SessionID,
			Role:           input.Role,
			Content:        input.Content,
			DisplayContent: input.DisplayText,
		})
	}
	return persisted, true, nil
}

func (*recordingMessageService) List(context.Context, string) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*recordingMessageService) ListSince(context.Context, string, time.Time) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*recordingMessageService) ListActiveSince(context.Context, string, time.Time) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*recordingMessageService) ListLatest(context.Context, string, int32) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*recordingMessageService) ListBefore(context.Context, string, time.Time, int32) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*recordingMessageService) ListBySession(context.Context, string) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*recordingMessageService) ListSinceBySession(context.Context, string, time.Time) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*recordingMessageService) ListActiveSinceBySession(context.Context, string, time.Time) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*recordingMessageService) ListActiveAfterCompactionBySessionWithinBytes(context.Context, string, int64) (messagepkg.SessionHistoryAfterCompaction, error) {
	return messagepkg.SessionHistoryAfterCompaction{}, nil
}

func (*recordingMessageService) ListRecentUserBySession(context.Context, string, int) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*recordingMessageService) ListTurnResponseSourcesSinceBySessionWithinBytes(context.Context, string, time.Time, int64) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*recordingMessageService) MeasureActiveBySession(context.Context, string, time.Time) (messagepkg.ActiveMessagesMeasure, error) {
	return messagepkg.ActiveMessagesMeasure{}, nil
}

func (*recordingMessageService) ListLatestBySession(context.Context, string, int32) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*recordingMessageService) ListBeforeBySession(context.Context, string, time.Time, int32) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*recordingMessageService) ListBeforeMessageBySession(context.Context, string, string, int32) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*recordingMessageService) LocateByExternalIDBySession(context.Context, string, string, int32, int32) (messagepkg.LocateResult, error) {
	return messagepkg.LocateResult{}, nil
}

func (s *recordingMessageService) GetByIDBySession(_ context.Context, sessionID, messageID string) (messagepkg.Message, error) {
	for _, input := range s.persisted {
		if input.SessionID == sessionID && input.Role == "user" {
			return messagepkg.Message{
				ID: messageID, SessionID: sessionID, Role: input.Role,
				Content: input.Content, DisplayContent: input.DisplayText,
			}, nil
		}
	}
	return messagepkg.Message{}, errors.New("message not found")
}

func (*recordingMessageService) ListVisibleFromBySession(context.Context, string, string) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*recordingMessageService) ListVisibleFromForModelBySession(context.Context, string, string) ([]messagepkg.Message, error) {
	return nil, nil
}

func (*recordingMessageService) GetVisibleTurnByMessage(context.Context, string, string) (messagepkg.HistoryTurn, error) {
	return messagepkg.HistoryTurn{}, nil
}

func (*recordingMessageService) GetLatestVisibleTurnBySession(context.Context, string) (messagepkg.HistoryTurn, error) {
	return messagepkg.HistoryTurn{}, nil
}

func (s *recordingMessageService) ReplaceTurn(_ context.Context, _, _, replacementTurnID string, replacementTurnPosition *int64, _, _, _ string) (messagepkg.HistoryTurn, error) {
	s.replaced++
	s.replacementTurnID = replacementTurnID
	s.replacementTurnPosition = replacementTurnPosition
	return messagepkg.HistoryTurn{}, nil
}

func (s *recordingMessageService) DeleteByIDs(_ context.Context, ids []string) error {
	s.deleted = append(s.deleted, append([]string(nil), ids...))
	return nil
}

func (*recordingMessageService) DeleteByBot(context.Context, string) error {
	return nil
}

func (*recordingMessageService) DeleteBySession(context.Context, string) error {
	return nil
}

func (*recordingMessageService) LinkAssets(context.Context, string, []messagepkg.AssetRef) error {
	return nil
}

// A runtime that owns its conversation context has no rewind, so a retry of
// one of its turns is refused before the runtime is asked anything.
func TestWebRetryRejectsTurnReplacementForACP(t *testing.T) {
	t.Parallel()

	pool := &recordingACPPrompter{}
	resolver := &Service{
		sessionService: acpRuntimeSessionServiceForTest("user-1"),
		messageService: driveReplaceableHistory(t),
		sessionRuntime: newScriptedAdmitter(),
		logger:         slog.New(slog.DiscardHandler),
	}
	resolver.SetACPSessionPool(pool)
	admission, err := resolver.AdmitWebRun(context.Background(), WebAdmitInput{
		BotID: "bot-1", SessionID: "session-1", InvocationID: "retry-1", Payload: []byte(`{"kind":"retry_message"}`),
	})
	if err != nil {
		t.Fatalf("AdmitWebRun() error = %v", err)
	}
	delivery := newWebTestDelivery()
	resolver.RunWebRetry(admission, RetryLatestMessageInput{
		BotID: "bot-1", SessionID: "session-1", TargetTurnID: driveReplacedTurnID,
	}, delivery)

	if _, ended := delivery.wait(t); ended.ErrorCode != string(apperror.CodeExternalAgentTurnReplacementUnsupported) {
		t.Fatalf("ended = %+v, want %q", ended, apperror.CodeExternalAgentTurnReplacementUnsupported)
	}
	if pool.calls != 0 {
		t.Fatalf("ACP pool calls = %d, want none for a refused replacement", pool.calls)
	}
}

func TestPersistPartialResultDoesNotStoreUserOnlyFailure(t *testing.T) {
	t.Parallel()

	messages := &recordingMessageService{}
	resolver := &Service{
		messageService: messages,
		logger:         slog.New(slog.DiscardHandler),
	}

	resolver.persistPartialResult(
		context.Background(),
		ChatRequest{
			BotID:    "bot-1",
			ThreadID: "session-1",
			Query:    "hello",
		},
		resolvedContext{},
		nil,
		nil,
		0,
		false,
		true,
		"",
		nil,
	)

	if len(messages.persisted) != 0 {
		t.Fatalf("expected failed stream not to persist user-only history, got %#v", messages.persisted)
	}
}

func TestPersistTerminalSnapshotSkipsUserOnlySnapshot(t *testing.T) {
	t.Parallel()

	messages := &recordingMessageService{}
	resolver := &Service{
		messageService: messages,
		logger:         slog.New(slog.DiscardHandler),
	}

	if err := resolver.persistTerminalSnapshot(
		context.Background(),
		ChatRequest{
			BotID:    "bot-1",
			ThreadID: "session-1",
			Query:    "hello",
		},
		resolvedContext{},
		terminalSnapshot{
			sdkMessages: []sdk.Message{sdk.UserMessage("hello")},
		},
	); err != nil {
		t.Fatalf("persistTerminalSnapshot returned error: %v", err)
	}

	if len(messages.persisted) != 0 {
		t.Fatalf("expected user-only terminal snapshot not to persist, got %#v", messages.persisted)
	}
}

func TestPersistTerminalSnapshotSkipsEmptyAssistantSnapshot(t *testing.T) {
	t.Parallel()

	messages := &recordingMessageService{}
	resolver := &Service{
		messageService: messages,
		logger:         slog.New(slog.DiscardHandler),
	}

	if err := resolver.persistTerminalSnapshot(
		context.Background(),
		ChatRequest{
			BotID:    "bot-1",
			ThreadID: "session-1",
			Query:    "hello",
		},
		resolvedContext{},
		terminalSnapshot{
			sdkMessages: []sdk.Message{sdk.AssistantMessage("")},
		},
	); err != nil {
		t.Fatalf("persistTerminalSnapshot returned error: %v", err)
	}

	if len(messages.persisted) != 0 {
		t.Fatalf("expected empty assistant terminal snapshot not to persist, got %#v", messages.persisted)
	}
}

func TestPersistTerminalSnapshotSkipsAbortedSnapshotBeforeVisibleOutput(t *testing.T) {
	t.Parallel()

	messages := &recordingMessageService{}
	resolver := &Service{
		messageService: messages,
		logger:         slog.New(slog.DiscardHandler),
	}

	if err := resolver.persistTerminalSnapshot(
		context.Background(),
		ChatRequest{
			BotID:    "bot-1",
			ThreadID: "session-1",
			Query:    "hello",
		},
		resolvedContext{},
		terminalSnapshot{
			sdkMessages: []sdk.Message{sdk.AssistantMessage("partial answer")},
			aborted:     true,
		},
	); err != nil {
		t.Fatalf("persistTerminalSnapshot returned error: %v", err)
	}

	if len(messages.persisted) != 0 {
		t.Fatalf("expected pre-output abort not to persist, got %#v", messages.persisted)
	}
}

// A Web send the user-message hook refused leaves its two rows under the turn
// admission named, both marked with the hook origin.
func TestHookFailureRowUsesAdmittedIdentity(t *testing.T) {
	t.Parallel()

	messages := &recordingMessageService{}
	service := &Service{messageService: messages, logger: slog.New(slog.DiscardHandler)}
	position := int64(7)
	_, err := service.persistFailureRow(context.Background(), ChatRequest{
		BotID:        "bot-1",
		ThreadID:     "session-1",
		RunID:        "run-1",
		TurnID:       "turn-1",
		TurnPosition: &position,
		Query:        "hello",
	}, resolvedContext{}, RunFailure{Code: apperror.CodeHookUserMessageFailed}, failureOrigin(true))
	if err != nil {
		t.Fatalf("persistFailureRow: %v", err)
	}
	if len(messages.persisted) != 2 {
		t.Fatalf("persisted %d messages, want user + assistant", len(messages.persisted))
	}
	for i, input := range messages.persisted {
		if i == 0 && (input.TurnID != "turn-1" || input.TurnPosition == nil || *input.TurnPosition != position) {
			t.Fatalf("user identity = (%q, %v), want (turn-1, %d)", input.TurnID, input.TurnPosition, position)
		}
		if input.Metadata[messagepkg.HistoryFailureOriginMetadataKey] != messagepkg.HistoryFailureOriginUserMessageHook {
			t.Fatalf("message %d metadata = %#v, want Hook failure origin", i, input.Metadata)
		}
		if i == 1 && input.Metadata[messagepkg.HistoryErrorCodeMetadataKey] != string(apperror.CodeHookUserMessageFailed) {
			t.Fatalf("assistant metadata = %#v, want Hook failure code", input.Metadata)
		}
	}
}

func TestPersistTerminalSnapshotStoresTimeoutBeforeVisibleOutput(t *testing.T) {
	t.Parallel()

	messages := &recordingMessageService{}
	resolver := &Service{
		messageService: messages,
		logger:         slog.New(slog.DiscardHandler),
	}

	if err := resolver.persistTerminalSnapshot(
		context.Background(),
		ChatRequest{
			BotID:    "bot-1",
			ThreadID: "session-1",
			Query:    "hello",
		},
		resolvedContext{},
		terminalSnapshot{
			aborted:     true,
			failureCode: apperror.CodeAgentResponseTimeout,
		},
	); err != nil {
		t.Fatalf("persistTerminalSnapshot returned error: %v", err)
	}

	if len(messages.persisted) != 2 {
		t.Fatalf("expected user + timeout assistant, got %#v", messages.persisted)
	}
	if messages.persisted[0].Role != "user" || messages.persisted[1].Role != "assistant" {
		t.Fatalf("persisted roles = %s/%s", messages.persisted[0].Role, messages.persisted[1].Role)
	}
	if got, _ := messages.persisted[1].Metadata[messagepkg.HistoryErrorCodeMetadataKey].(string); got != string(apperror.CodeAgentResponseTimeout) {
		t.Fatalf("error_code = %q, want %q", got, apperror.CodeAgentResponseTimeout)
	}
	if messages.persisted[1].Metadata[messagepkg.AgentStepInterruptedMetadataKey] != true {
		t.Fatalf("expected interrupted metadata on timeout assistant")
	}
}

func TestPersistTurnFailureSkipsRetryReplacement(t *testing.T) {
	t.Parallel()

	messages := &recordingMessageService{}
	resolver := &Service{
		messageService: messages,
		logger:         slog.New(slog.DiscardHandler),
	}

	persisted, err := resolver.persistTurnFailure(
		context.Background(),
		ChatRequest{
			BotID:           "bot-1",
			ThreadID:        "session-1",
			Query:           "hello",
			SkipHistoryTurn: true,
		},
		resolvedContext{},
		apperror.CodeAgentResponseTimeout,
	)
	if err != nil {
		t.Fatalf("persistTurnFailure returned error: %v", err)
	}
	if persisted != nil || len(messages.persisted) != 0 {
		t.Fatalf("expected retry replacement not to persist a failure turn, got %#v", messages.persisted)
	}
}

func TestPersistPartialResultStoresTimeoutWithoutSnapshot(t *testing.T) {
	t.Parallel()

	messages := &recordingMessageService{}
	resolver := &Service{
		messageService: messages,
		logger:         slog.New(slog.DiscardHandler),
	}

	persisted := resolver.persistPartialResult(
		context.Background(),
		ChatRequest{
			BotID:    "bot-1",
			ThreadID: "session-1",
			Query:    "hello",
		},
		resolvedContext{},
		nil,
		nil,
		0,
		true,
		false,
		apperror.CodeAgentResponseTimeout,
		nil,
	)
	if len(persisted) == 0 || len(messages.persisted) != 2 {
		t.Fatalf("expected timeout without snapshot to persist a turn failure, got %#v", messages.persisted)
	}
}

func TestPersistTerminalSnapshotStoresAssistantOutput(t *testing.T) {
	t.Parallel()

	messages := &recordingMessageService{}
	resolver := &Service{
		messageService: messages,
		logger:         slog.New(slog.DiscardHandler),
	}

	if err := resolver.persistTerminalSnapshot(
		context.Background(),
		ChatRequest{
			BotID:    "bot-1",
			ThreadID: "session-1",
			Query:    "hello",
		},
		resolvedContext{},
		terminalSnapshot{
			sdkMessages: []sdk.Message{sdk.AssistantMessage("partial answer")},
		},
	); err != nil {
		t.Fatalf("persistTerminalSnapshot returned error: %v", err)
	}

	if len(messages.persisted) != 2 {
		t.Fatalf("expected user and assistant messages to persist, got %#v", messages.persisted)
	}
	if messages.persisted[0].Role != "user" || messages.persisted[1].Role != "assistant" {
		t.Fatalf("unexpected persisted roles: %q, %q", messages.persisted[0].Role, messages.persisted[1].Role)
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
		if hasVisibleAgentStreamOutput(event) {
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
		if !hasVisibleAgentStreamOutput(event) {
			t.Fatalf("event %q unexpectedly counted as invisible", event.Type)
		}
	}
}

func TestPersistTerminalSnapshotPersistsUserWhenPipelineContextContainsCurrentMessage(t *testing.T) {
	t.Parallel()

	messages := &recordingMessageService{}
	resolver := &Service{
		messageService: messages,
		logger:         slog.New(slog.DiscardHandler),
	}

	if err := resolver.persistTerminalSnapshot(
		context.Background(),
		ChatRequest{
			BotID:    "bot-1",
			ThreadID: "session-1",
			Query:    "---\nmessage-id: tg-1\nchannel: telegram\n---\n@memoh1bot ping",
		},
		resolvedContext{},
		terminalSnapshot{
			sdkMessages: []sdk.Message{sdk.AssistantMessage("pong")},
		},
	); err != nil {
		t.Fatalf("persistTerminalSnapshot returned error: %v", err)
	}

	if len(messages.persisted) != 2 {
		t.Fatalf("expected user and assistant output to persist, got %#v", messages.persisted)
	}
	if messages.persisted[0].Role != "user" {
		t.Fatalf("unexpected first persisted role: %q", messages.persisted[0].Role)
	}
	if messages.persisted[1].Role != "assistant" {
		t.Fatalf("unexpected second persisted role: %q", messages.persisted[1].Role)
	}
}

func TestPersistTerminalSnapshotHonorsSkipMemoryExtraction(t *testing.T) {
	t.Parallel()

	memory := &storeRoundMemoryProvider{afterChat: make(chan memprovider.AfterChatRequest, 2)}
	registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
	registry.Register(storeRoundMemoryProviderID, memory)
	resolver := &Service{
		messageService:  &recordingMessageService{},
		memoryRegistry:  registry,
		settingsService: settings.NewService(slog.New(slog.DiscardHandler), &storeRoundSettingsQueries{}, nil, nil),
		logger:          slog.New(slog.DiscardHandler),
	}

	req := ChatRequest{
		BotID:    storeRoundBotID,
		ThreadID: "session-1",
		Query:    "hello",
	}
	if err := resolver.persistTerminalSnapshot(
		context.Background(),
		req,
		resolvedContext{},
		terminalSnapshot{
			sdkMessages:   []sdk.Message{sdk.AssistantMessage("pong")},
			visibleOutput: true,
		},
	); err != nil {
		t.Fatalf("persistTerminalSnapshot returned error: %v", err)
	}
	select {
	case <-memory.afterChat:
	case <-time.After(time.Second):
		t.Fatal("expected ordinary terminal snapshot to write memory")
	}

	req.SkipMemoryExtraction = true
	if err := resolver.persistTerminalSnapshot(
		context.Background(),
		req,
		resolvedContext{},
		terminalSnapshot{
			sdkMessages:   []sdk.Message{sdk.AssistantMessage("done")},
			visibleOutput: true,
		},
	); err != nil {
		t.Fatalf("persistTerminalSnapshot returned error with skip memory: %v", err)
	}
	select {
	case got := <-memory.afterChat:
		t.Fatalf("expected skip memory extraction to suppress memory write, got %#v", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPersistTerminalSnapshotSkillActivationWithoutPromptDoesNotStoreModelMarker(t *testing.T) {
	t.Parallel()

	messages := &recordingMessageService{}
	resolver := &Service{
		messageService: messages,
		logger:         slog.New(slog.DiscardHandler),
	}
	activation := &SkillActivation{
		Skills: []SkillActivationSkill{{Name: "alpha", DisplayName: "Alpha", State: "effective"}},
	}
	req := ChatRequest{
		BotID:                "bot-1",
		ThreadID:             "session-1",
		ModelQuery:           skillActivationModelQuery(activation),
		UserMessageKind:      UserMessageKindSkillActivation,
		SkillActivation:      activation,
		SkipMemoryExtraction: true,
	}

	if err := resolver.persistTerminalSnapshot(
		context.Background(),
		req,
		resolvedContext{},
		terminalSnapshot{
			sdkMessages:   []sdk.Message{sdk.AssistantMessage("done")},
			visibleOutput: true,
		},
	); err != nil {
		t.Fatalf("persistTerminalSnapshot returned error: %v", err)
	}

	if len(messages.persisted) != 2 {
		t.Fatalf("persisted messages = %d, want user + assistant", len(messages.persisted))
	}
	user := messages.persisted[0]
	if user.Role != "user" {
		t.Fatalf("first persisted role = %q, want user", user.Role)
	}
	if got := persistedTextContent(t, user.Content); got != "" {
		t.Fatalf("persisted user content = %q, want empty", got)
	}
	if user.DisplayText != "" {
		t.Fatalf("display text = %q, want empty", user.DisplayText)
	}
	if user.Metadata["user_message_kind"] != UserMessageKindSkillActivation {
		t.Fatalf("metadata kind = %#v, want skill_activation", user.Metadata["user_message_kind"])
	}
}

func TestReplacePersistedTurnErrorsWhenNoReplacementPersisted(t *testing.T) {
	t.Parallel()

	messages := &recordingMessageService{}
	resolver := &Service{
		messageService: messages,
		logger:         slog.New(slog.DiscardHandler),
	}

	err := resolver.replacePersistedTurn(
		context.Background(),
		ChatRequest{ThreadID: "session-1"},
		"turn-1",
		"request-1",
		"retry",
		nil,
	)
	if err == nil {
		t.Fatal("expected replacement error, got nil")
	}
	if messages.replaced != 0 {
		t.Fatalf("ReplaceTurn called %d times, want 0", messages.replaced)
	}
}
