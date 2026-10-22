package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	chatview "github.com/felinics/memoh/internal/agent/view"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/schedule"
)

func TestRuntimeNoticesSurviveTerminalAndHistory(t *testing.T) {
	for _, runtime := range []string{"codex", "claude-code", "grok-build", "acp_agent"} {
		for _, outcome := range []string{"completed", "failed", "configuration_failure"} {
			t.Run(runtime+"/"+outcome, func(t *testing.T) {
				messages := &agenttest.RecordingMessages{}
				driver := noticeTestDriver{kind: runtime, prompt: func(_ context.Context, input external.PromptInput) (external.PromptResult, error) {
					notice := event.StreamEvent{
						Type: event.RuntimeNotice, NoticeKind: event.NoticeToolsUnavailable, Delta: "public runtime notice",
						Metadata: map[string]any{"dep_id": "codex", "diagnostic": map[string]any{"private": "SECRET"}}, Error: "SECRET",
					}
					// The application must collect notices even before a driver has
					// created any transcript, and coalesce repeated delivery.
					input.Sink.EmitStreamEvent(notice)
					input.Sink.EmitStreamEvent(notice)
					if outcome == "configuration_failure" {
						return external.PromptResult{}, external.Unavailable(errors.New("SECRET bridge down"))
					}
					result := external.PromptResult{Output: []sdk.Message{sdk.AssistantMessage("first"), sdk.AssistantMessage("last")}, TurnCompleted: outcome == "completed"}
					if outcome == "failed" {
						return result, errors.New("SECRET")
					}
					return result, nil
				}}
				service := newACPLifecycleService(t, &recordingACPPrompter{}, messages, &agenttest.RecordingLifecycleStore{}, withExternalDriver(driver))
				ch := make(chan runreq.WSStreamEvent, 64)
				if _, err := streamExternalWebSend(context.Background(), service, runreq.ChatRequest{BotID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID, RunID: agenttest.LifecycleRunID, Query: "test"}, ch, make(chan struct{})); err != nil {
					t.Fatal(err)
				}
				if len(messages.Deleted) != 0 {
					t.Fatalf("a notice-bearing round was removed: %v", messages.Deleted)
				}
				assertPersistedRuntimeNotice(t, messages)
				liveNotices, terminalNotices := 0, 0
				for _, ev := range drainAgentEvents(t, ch) {
					if ev.Type == event.RuntimeNotice {
						liveNotices++
					}
					if ev.IsTerminal() {
						terminalNotices += len(event.NoticesFromMetadata(ev.Metadata))
						if strings.Contains(string(ev.Messages), "public runtime notice") {
							t.Fatal("notice entered the model transcript")
						}
					}
				}
				if liveNotices != 1 || terminalNotices != 1 {
					t.Fatalf("live notices=%d terminal notices=%d, want one each", liveNotices, terminalNotices)
				}
			})
		}
	}
}

func assertPersistedRuntimeNotice(t *testing.T, messages *agenttest.RecordingMessages) {
	t.Helper()
	var rows []messagepkg.Message
	count := 0
	for i, input := range messages.Persisted {
		metadata, err := json.Marshal(input.Metadata)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(metadata), "SECRET") || strings.Contains(string(input.Content), "public runtime notice") {
			t.Fatal("private diagnostics persisted or notice leaked into model content")
		}
		count += len(event.NoticesFromMetadata(input.Metadata))
		// Exercise the lazy RawMetadata path used by the PostgreSQL history API.
		rows = append(rows, messagepkg.Message{ID: fmt.Sprintf("message-%d", i), TurnID: agenttest.LifecycleRunID, Role: input.Role, Content: input.Content, RawMetadata: metadata})
	}
	if count != 1 {
		t.Fatalf("persisted notices=%d, want exactly one across all assistant rows", count)
	}
	count = 0
	for _, turn := range chatview.ConvertMessagesToUITurns(rows) {
		for _, block := range turn.Messages {
			if block.Type == chatview.UIMessageNotice && block.Name == "tools_unavailable" && block.Content == "public runtime notice" {
				count++
			}
		}
	}
	if count != 1 {
		t.Fatalf("reloaded UI notices=%d, want one", count)
	}
}

func TestScheduledRuntimeNoticeIsPersistedWithoutInteractiveSubscriber(t *testing.T) {
	messages := &agenttest.RecordingMessages{}
	driver := noticeTestDriver{kind: "codex", prompt: func(_ context.Context, input external.PromptInput) (external.PromptResult, error) {
		input.Sink.EmitStreamEvent(event.StreamEvent{Type: event.RuntimeNotice, NoticeKind: event.NoticeToolsUnavailable, Delta: "public runtime notice"})
		return external.PromptResult{Text: "done", Output: []sdk.Message{sdk.AssistantMessage("done")}, TurnCompleted: true}, nil
	}}
	service := newACPLifecycleService(t, &recordingACPPrompter{}, messages, &agenttest.RecordingLifecycleStore{}, withScheduleDriver(driver))
	_, err := runScheduledExternal(context.Background(), service, schedule.TriggerPayload{FireID: "fire-1", ID: "schedule-1", SessionID: agenttest.LifecycleSessionID, Command: "test", OwnerUserID: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	assertPersistedRuntimeNotice(t, messages)
}
