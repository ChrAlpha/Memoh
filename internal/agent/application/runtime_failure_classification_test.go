package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/turn"
	chatview "github.com/felinics/memoh/internal/agent/view"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/errs"
	"github.com/felinics/memoh/internal/schedule"
)

// A failed External Agent round records the code the executor names its cause
// with, and nothing beside it.
func TestPersistExternalRoundFailureCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		cause    error
		wantCode string
		wantArgs map[string]any
	}{
		{name: "catalogued code", cause: apperror.Wrap(apperror.CodeAgentResponseTimeout, errors.New("SECRET"), nil), wantCode: "agent.response_timeout"},
		{name: "external agent code", cause: fmt.Errorf("prompt: %w", apperror.New(apperror.CodeACPRuntimeBusy, nil)), wantCode: "acp_runtime_busy"},
		{name: "plain error", cause: errors.New("SECRET driver exit"), wantCode: "runtime_prompt_failed"},
		{
			name:     "missing dependency",
			cause:    &external.DependencyMissingError{DependencyID: "codex", TaskID: "task-1"},
			wantCode: "agent_dependency_missing",
			wantArgs: map[string]any{"dep_id": "codex", "install_task_id": "task-1", "operation_in_progress": "false"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			messages := &recordingMessageService{}
			service := &Service{messageService: messages, logger: slog.New(slog.DiscardHandler)}
			if err := service.persistExternalRound(
				context.Background(),
				ChatRequest{BotID: "bot-1", ThreadID: "session-1", Query: "run"},
				"codex",
				"/data/app",
				external.PromptResult{},
				externalRoundFailure(tc.cause),
				false,
				nil,
				nil,
			); err != nil {
				t.Fatalf("persistExternalRound() error = %v", err)
			}
			if len(messages.persisted) != 2 {
				t.Fatalf("persisted %d messages, want user + assistant", len(messages.persisted))
			}
			meta := messages.persisted[1].Metadata
			if got, _ := meta["error_code"].(string); got != tc.wantCode {
				t.Fatalf("error_code = %q, want %q", got, tc.wantCode)
			}
			stored, _ := json.Marshal(meta[messagepkg.HistoryErrorArgsMetadataKey])
			var gotArgs map[string]any
			_ = json.Unmarshal(stored, &gotArgs)
			if !reflect.DeepEqual(gotArgs, tc.wantArgs) {
				t.Fatalf("error_args = %s, want %v", stored, tc.wantArgs)
			}
			for _, key := range []string{"error_reason", "i18n_key"} {
				if got, found := meta[key]; found {
					t.Fatalf("%s = %#v, want it absent", key, got)
				}
			}
		})
	}
}

// X5: an External Agent turn that ran and then failed with an External Agent
// code. The live failure event, the terminal event and the history marker all
// carry that code, and the outcome names it for the run's terminal write.
func TestStreamRuntimeExternalAgentFailureCarriesOneCode(t *testing.T) {
	messages := &recordingMessageService{}
	service := newACPLifecycleService(t, &recordingACPPrompter{}, messages, &recordingContextLifecycleStore{})
	driver := noticeTestDriver{kind: "codex", prompt: func(context.Context, external.PromptInput) (external.PromptResult, error) {
		return external.PromptResult{Output: []sdk.Message{sdk.AssistantMessage("partial")}},
			apperror.New(apperror.CodeACPRuntimeBusy, nil)
	}}
	ch := make(chan WSStreamEvent, 64)
	outcome, err := service.streamExternalWebSend(context.Background(), driver, ChatRequest{
		BotID: lifecycleTestBotID, ThreadID: lifecycleTestSessionID, RunID: lifecycleTestRunID, Query: "test",
	}, ch, make(chan struct{}))
	if err != nil {
		t.Fatalf("streamExternalWebSend() error = %v", err)
	}
	var failureCode, terminalCode string
	for _, ev := range drainAgentEvents(t, ch) {
		switch {
		case ev.Type == native.EventError:
			failureCode = ev.Code
		case ev.IsTerminal():
			terminalCode = ev.Code
		}
	}
	const want = "acp_runtime_busy"
	if failureCode != want || terminalCode != want {
		t.Fatalf("failure event code = %q, terminal event code = %q, want %q", failureCode, terminalCode, want)
	}
	if outcome.Status != "errored" || outcome.ErrorCode() != want {
		t.Fatalf("outcome = %q / %q, want errored / %q", outcome.Status, outcome.ErrorCode(), want)
	}
	if got, _ := messages.persisted[len(messages.persisted)-1].Metadata["error_code"].(string); got != want {
		t.Fatalf("history error_code = %q, want %q", got, want)
	}
}

// The cause an External Agent turn hands to its result record names the
// runtime that failed, so one query over the records finds every External
// Agent failure whatever code it took.
func TestStreamRuntimeFailureNamesItsRuntime(t *testing.T) {
	service := newACPLifecycleService(t, &recordingACPPrompter{}, &recordingMessageService{}, &recordingContextLifecycleStore{})
	driver := noticeTestDriver{kind: "codex", prompt: func(context.Context, external.PromptInput) (external.PromptResult, error) {
		return external.PromptResult{Output: []sdk.Message{sdk.AssistantMessage("partial")}}, errors.New("driver exit")
	}}
	outcome, err := service.streamExternalWebSend(context.Background(), driver, ChatRequest{
		BotID: lifecycleTestBotID, ThreadID: lifecycleTestSessionID, RunID: lifecycleTestRunID, Query: "test",
	}, make(chan WSStreamEvent, 64), make(chan struct{}))
	if err != nil {
		t.Fatalf("streamExternalWebSend() error = %v", err)
	}
	attrs := errs.Analyze(context.Background(), outcome.Cause).Attrs
	if !slices.ContainsFunc(attrs, func(attr slog.Attr) bool { return attr.Key == "runtime" && attr.Value.String() == "codex" }) {
		t.Fatalf("result record attrs = %v, want runtime=codex", attrs)
	}
}

// A turn that failed after some output keeps that output as it was, with the
// failure block after it. The failure reaches the stream as one error event
// and adds no text to the round.
func TestStreamRuntimeFailureKeepsPartialOutputUnchanged(t *testing.T) {
	messages := &recordingMessageService{}
	service := newACPLifecycleService(t, &recordingACPPrompter{}, messages, &recordingContextLifecycleStore{})
	driver := noticeTestDriver{kind: "codex", prompt: func(context.Context, external.PromptInput) (external.PromptResult, error) {
		return external.PromptResult{Output: []sdk.Message{sdk.AssistantMessage("partial answer")}},
			apperror.New(apperror.CodeACPRuntimeBusy, nil)
	}}
	ch := make(chan WSStreamEvent, 64)
	if _, err := service.streamExternalWebSend(context.Background(), driver, ChatRequest{
		BotID: lifecycleTestBotID, ThreadID: lifecycleTestSessionID, RunID: lifecycleTestRunID, Query: "test",
	}, ch, make(chan struct{})); err != nil {
		t.Fatalf("streamExternalWebSend() error = %v", err)
	}
	var failures []string
	for _, ev := range drainAgentEvents(t, ch) {
		switch ev.Type {
		case native.EventTextDelta:
			t.Fatalf("text delta %q, want the failure sent only as its error event", ev.Delta)
		case native.EventError:
			failures = append(failures, ev.Code)
		case native.EventAbort:
			if got := terminalAssistantText(t, ev); got != "partial answer" {
				t.Fatalf("terminal assistant text = %q, want the partial output alone", got)
			}
		}
	}
	const want = "acp_runtime_busy"
	if !slices.Equal(failures, []string{want}) {
		t.Fatalf("failure events = %q, want one %q", failures, want)
	}
	var texts []string
	var historyCode string
	for _, turn := range historyTurns(t, messages.persisted) {
		if turn.Role != "assistant" {
			continue
		}
		for _, block := range turn.Messages {
			switch block.Type {
			case chatview.UIMessageText:
				texts = append(texts, block.Content)
			case chatview.UIMessageError:
				historyCode = block.Code
			}
		}
	}
	if !slices.Equal(texts, []string{"partial answer"}) || historyCode != want {
		t.Fatalf("history assistant texts = %q, failure code = %q, want [partial answer] / %q", texts, historyCode, want)
	}
}

// A failed scheduled round is stored the same way: the output the runtime
// produced, or an empty assistant row, carrying the failure code.
func TestScheduledRuntimeFailureRoundHasNoFailureText(t *testing.T) {
	for _, tc := range []struct {
		name     string
		output   []sdk.Message
		wantText string
	}{
		{name: "no output"},
		{name: "partial output", output: []sdk.Message{sdk.AssistantMessage("partial answer")}, wantText: "partial answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := &recordingMessageService{}
			service := newACPLifecycleService(t, &recordingACPPrompter{}, messages, &recordingContextLifecycleStore{})
			driver := noticeTestDriver{kind: "codex", prompt: func(context.Context, external.PromptInput) (external.PromptResult, error) {
				return external.PromptResult{Output: tc.output}, apperror.New(apperror.CodeACPRuntimeBusy, nil)
			}}
			if _, err := runScheduledExternal(context.Background(), t, service, driver, schedule.TriggerPayload{
				FireID: "fire-1", ID: "schedule-1",
				SessionID: lifecycleTestSessionID, Command: "inspect", OwnerUserID: "user-1",
			}); apperror.CodeOf(err) != apperror.CodeACPRuntimeBusy {
				t.Fatalf("schedule error = %v, want %s", err, apperror.CodeACPRuntimeBusy)
			}
			last := messages.persisted[len(messages.persisted)-1]
			if last.Role != "assistant" || persistedText(t, last.Content) != tc.wantText {
				t.Fatalf("last persisted = %s %q, want assistant %q", last.Role, persistedText(t, last.Content), tc.wantText)
			}
			if got, _ := last.Metadata["error_code"].(string); got != string(apperror.CodeACPRuntimeBusy) {
				t.Fatalf("error_code = %#v, want %s", last.Metadata["error_code"], apperror.CodeACPRuntimeBusy)
			}
		})
	}
}

// The schedule log stores the text of the error a fire returns, and the API
// serves it. A scheduled External Agent failure therefore reads as its code
// alone, while the agent's own words stay behind it for the result record.
func TestScheduledRuntimeFailureTextIsItsCode(t *testing.T) {
	service := newACPLifecycleService(t, &recordingACPPrompter{}, &recordingMessageService{}, &recordingContextLifecycleStore{})
	driver := noticeTestDriver{kind: "codex", prompt: func(context.Context, external.PromptInput) (external.PromptResult, error) {
		return external.PromptResult{Output: []sdk.Message{sdk.AssistantMessage("partial")}}, errors.New("SECRET stderr tail")
	}}
	_, err := runScheduledExternal(context.Background(), t, service, driver, schedule.TriggerPayload{
		FireID: "fire-1", ID: "schedule-1",
		SessionID: lifecycleTestSessionID, Command: "inspect", OwnerUserID: "user-1",
	})
	if err == nil || err.Error() != string(apperror.CodeRuntimePromptFailed) {
		t.Fatalf("schedule error text = %q, want exactly %q", err, apperror.CodeRuntimePromptFailed)
	}
	if text := errs.Text(err); !strings.Contains(text, "SECRET") {
		t.Fatalf("result record text = %q, want it to keep the agent's words", text)
	}
}

// A Web send the runtime refuses before it starts is handed back: the draft
// still belongs to the composer, so the user's message is removed again and no
// failure row is written. The failure reaches the client as its error frame
// under the runtime's own code, and the frame carries no text.
func TestWebSendRefusedByRuntimeHandsTheDraftBack(t *testing.T) {
	messages := &recordingMessageService{}
	service := newACPLifecycleService(t, &recordingACPPrompter{}, messages, &recordingContextLifecycleStore{})
	driver := noticeTestDriver{kind: "codex", prompt: func(context.Context, external.PromptInput) (external.PromptResult, error) {
		return external.PromptResult{}, &external.DependencyMissingError{DependencyID: "codex"}
	}}
	ch := make(chan WSStreamEvent, 64)
	outcome, err := service.streamExternalWebSend(context.Background(), driver, ChatRequest{
		BotID: lifecycleTestBotID, ThreadID: lifecycleTestSessionID, RunID: lifecycleTestRunID, Query: "inspect",
	}, ch, make(chan struct{}))
	if err != nil {
		t.Fatalf("streamExternalWebSend() error = %v", err)
	}
	const want = string(apperror.CodeAgentDependencyMissing)
	if outcome.ErrorCode() != want {
		t.Fatalf("outcome code = %q, want %q", outcome.ErrorCode(), want)
	}
	var frameCode string
	for _, ev := range drainAgentEvents(t, ch) {
		switch ev.Type {
		case native.EventTextDelta:
			t.Fatalf("text delta %q, want the failure sent only as its error frame", ev.Delta)
		case native.EventError:
			frameCode = ev.Code
		}
	}
	if frameCode != want {
		t.Fatalf("frame code = %q, want %q", frameCode, want)
	}
	if len(messages.persisted) != 1 || messages.persisted[0].Role != "user" || len(messages.deleted) != 1 {
		t.Fatalf("persisted = %+v deleted = %v, want only the user message, then removed", messages.persisted, messages.deleted)
	}
}

// A send an IM channel started has left the sender's hands, so the same
// refusal keeps the user's message and records the failure under its code.
func TestStartTurnRefusedByRuntimeKeepsTheSendWithItsFailure(t *testing.T) {
	messages := &recordingMessageService{}
	service := newACPLifecycleService(t, &recordingACPPrompter{}, messages, &recordingContextLifecycleStore{})
	service.sessionRuntime = &lifecycleTurnAdmitter{admission: lifecycleTestAdmission()}
	service.SetAllowedTeam(driveTeamID)
	service.SetExternalRuntimes(noticeTestDriver{kind: session.RuntimeACPAgent, prompt: func(context.Context, external.PromptInput) (external.PromptResult, error) {
		return external.PromptResult{}, &external.DependencyMissingError{DependencyID: "codex"}
	}})
	handle, err := service.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion:     1,
		TeamID:            driveTeamID,
		Mode:              turn.ModeChat,
		BotID:             lifecycleTestBotID,
		ChatID:            lifecycleTestBotID,
		ThreadID:          lifecycleTestSessionID,
		Query:             "inspect",
		ExternalMessageID: "im-message-1",
		CurrentChannel:    "telegram",
		ReplyTarget:       "im-chat-1",
		ConversationType:  "private",
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	drainDriveHandle(t, handle)

	if len(messages.deleted) != 0 {
		t.Fatalf("deleted = %v, want the user's message kept", messages.deleted)
	}
	if len(messages.persisted) != 2 || messages.persisted[0].Role != "user" {
		t.Fatalf("persisted = %+v, want the user turn and its failure row", messages.persisted)
	}
	if got, _ := messages.persisted[1].Metadata["error_code"].(string); got != string(apperror.CodeAgentDependencyMissing) {
		t.Fatalf("history error_code = %q, want %q", got, apperror.CodeAgentDependencyMissing)
	}
}

// historyTurns is the session history a client reloads, read from the rows
// persisted so far.
func historyTurns(t *testing.T, persisted []messagepkg.PersistInput) []chatview.UITurn {
	t.Helper()
	rows := make([]messagepkg.Message, 0, len(persisted))
	for i, input := range persisted {
		metadata, err := json.Marshal(input.Metadata)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, messagepkg.Message{ID: fmt.Sprintf("message-%d", i), TurnID: lifecycleTestRunID, Role: input.Role, Content: input.Content, DisplayContent: input.DisplayText, RawMetadata: metadata})
	}
	return chatview.ConvertMessagesToUITurns(rows)
}
