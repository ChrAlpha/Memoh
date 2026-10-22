package agent

import (
	"context"
	"log/slog"
	"slices"
	"testing"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/lifecycle"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/turn"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
	"github.com/felinics/memoh/internal/settings"
)

// A turn an External Agent runtime runs records what the runtime was given in
// its context lifecycle, whichever entry started it: the manifest of the
// context sections, the memory recall trace, and the assistant row the lifecycle
// belongs to. Every entry resolves its plan through one function, and this is
// what keeps the lifecycle of a runtime's turn from depending on the door.
func TestExternalRunRecordsItsContextLifecycleFromEveryEntry(t *testing.T) {
	entries := []struct {
		name string
		run  func(t *testing.T, service *Service)
	}{
		{name: "web_send", run: func(t *testing.T, service *Service) {
			t.Helper()
			if _, err := streamWebSend(context.Background(), t, service, runreq.ChatRequest{
				BotID: agenttest.LifecycleBotID, ChatID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID,
				Query: directLifecyclePrompt, RawQuery: directLifecyclePrompt, UserVisibleText: directLifecyclePrompt,
				CurrentChannel: "local",
			}); err != nil {
				t.Fatalf("Web send failed: %v", err)
			}
		}},
		{name: "im_start_turn", run: func(t *testing.T, service *Service) {
			t.Helper()
			handle, err := service.StartTurn(context.Background(), turn.StartTurnCommand{
				SchemaVersion: 1, TeamID: driveTeamID, Mode: turn.ModeChat,
				BotID: agenttest.LifecycleBotID, ChatID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID,
				Query: directLifecyclePrompt, UserVisibleText: directLifecyclePrompt,
				ExternalMessageID: "im-message-1", SourceChannelIdentityID: "im-sender-1", UserID: "user-1",
				DisplayName: "IM Sender", CurrentChannel: "telegram", Channels: []string{"telegram"},
				ReplyTarget: "im-chat-1", ConversationType: "private",
			})
			if err != nil {
				t.Fatalf("StartTurn() error = %v", err)
			}
			drainTurn(t, handle)
		}},
		{name: "schedule", run: func(t *testing.T, service *Service) {
			t.Helper()
			if result, err := service.TriggerSchedule(context.Background(), agenttest.LifecycleBotID, driveSchedulePayload("user-1"), ""); err != nil || result.Status != "ok" {
				t.Fatalf("TriggerSchedule() = %q, %v, want ok", result.Status, err)
			}
		}},
	}
	for _, entry := range entries {
		t.Run(entry.name, func(t *testing.T) {
			messages := &agenttest.RecordingMessages{}
			lifecycles := &agenttest.RecordingLifecycleStore{}
			memory := &agenttest.MemoryProvider{
				BeforeChat: &memprovider.BeforeChatResult{
					ContextText: "remembered fact", RetrievalMode: "graph", ResultCount: 1, ResultRefs: []string{"memory-1"},
				},
				AfterChat: make(chan memprovider.AfterChatRequest, 1),
			}
			registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
			registry.Register(agenttest.MemoryProviderID, memory)
			service := newACPLifecycleService(t, &recordingACPPrompter{}, messages, lifecycles,
				withDeps(func(d *Deps) {
					d.Settings = settings.NewService(slog.New(slog.DiscardHandler), &agenttest.MemorySettingsQueries{}, nil, nil)
					d.Memory = registry
				}),
				withDrivers(&drivePromptRecorder{}))

			entry.run(t, service)

			_, snapshot := requireACPLifecycle(t, lifecycles, agenttest.LifecycleRunID, lifecycle.StatusCompleted)
			if snapshot.View != contextfrag.ViewExternalAgentPrompt || snapshot.Counts.Fragments == 0 || len(snapshot.SelectionDecisions) == 0 {
				t.Fatalf("lifecycle snapshot = %#v, want the runtime's populated context manifest", snapshot)
			}
			if snapshot.AssistantMessageID == "" {
				t.Fatalf("lifecycle snapshot = %#v, want the assistant row it belongs to", snapshot)
			}
			if snapshot.MemoryRecall == nil || !slices.Equal(snapshot.MemoryRecall.Result.Refs, []string{"memory-1"}) {
				t.Fatalf("lifecycle memory recall = %#v, want the recall trace of the run", snapshot.MemoryRecall)
			}
		})
	}
}
