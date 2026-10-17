package application

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"testing"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
	"github.com/felinics/memoh/internal/settings"
)

// A scheduled run has nobody to answer a decision, but an interactive External
// Agent turn does: the flag says whether the run can deliver a question, and a
// run that keeps it false never offers the runtime its ask_user tools.
func TestExternalPromptInputCarriesWhetherTheRunCanAskTheUser(t *testing.T) {
	t.Parallel()

	newExecutor := func(unattended bool, hasInputPort bool) *externalExecutor {
		svc := &Service{}
		if hasInputPort {
			svc.userInput = &fakeUserInputService{}
		}
		return &externalExecutor{service: svc, unattended: unattended}
	}

	cases := []struct {
		name         string
		unattended   bool
		hasInputPort bool
		want         bool
	}{
		{name: "interactive with a port", unattended: false, hasInputPort: true, want: true},
		{name: "interactive without a port", unattended: false, hasInputPort: false, want: false},
		{name: "unattended with a port", unattended: true, hasInputPort: true, want: false},
		{name: "unattended without a port", unattended: true, hasInputPort: false, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			exec := newExecutor(tc.unattended, tc.hasInputPort)
			got := exec.promptInput(context.Background(), func(native.StreamEvent) {}).CanRequestUserInput
			if got != tc.want {
				t.Fatalf("CanRequestUserInput = %v, want %v", got, tc.want)
			}
		})
	}
}

// The run's own cleanup and the background reconciliation of an unknown round
// both ask for the turn's decision rows to be removed, from different
// goroutines. The rows are removed once, and the callers do not race on that.
func TestExternalProjectionCleanupRunsOnceAcrossGoroutines(t *testing.T) {
	t.Parallel()

	messages := &recordingMessageService{}
	exec := &externalExecutor{service: &Service{logger: slog.New(slog.DiscardHandler), messageService: messages}}
	exec.projections.record("call-1")
	exec.projections.complete("call-1", &messagepkg.Message{ID: "projection-1"})

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			exec.cleanupProjections(context.Background())
		}()
	}
	wg.Wait()

	if len(messages.deleted) != 1 || len(messages.deleted[0]) != 1 || messages.deleted[0][0] != "projection-1" {
		t.Fatalf("deleted = %v, want the projected row removed once", messages.deleted)
	}
}

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
			if _, err := streamWebSend(context.Background(), t, service, ChatRequest{
				BotID: lifecycleTestBotID, ChatID: lifecycleTestBotID, ThreadID: lifecycleTestSessionID,
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
				BotID: lifecycleTestBotID, ChatID: lifecycleTestBotID, ThreadID: lifecycleTestSessionID,
				Query: directLifecyclePrompt, UserVisibleText: directLifecyclePrompt,
				ExternalMessageID: "im-message-1", SourceChannelIdentityID: "im-sender-1", UserID: "user-1",
				DisplayName: "IM Sender", CurrentChannel: "telegram", Channels: []string{"telegram"},
				ReplyTarget: "im-chat-1", ConversationType: "private",
			})
			if err != nil {
				t.Fatalf("StartTurn() error = %v", err)
			}
			drainDriveHandle(t, handle)
		}},
		{name: "schedule", run: func(t *testing.T, service *Service) {
			t.Helper()
			if result, err := service.TriggerSchedule(context.Background(), lifecycleTestBotID, driveSchedulePayload("user-1"), ""); err != nil || result.Status != "ok" {
				t.Fatalf("TriggerSchedule() = %q, %v, want ok", result.Status, err)
			}
		}},
	}
	for _, entry := range entries {
		t.Run(entry.name, func(t *testing.T) {
			messages := &recordingMessageService{}
			lifecycles := &recordingContextLifecycleStore{}
			memory := &storeRoundMemoryProvider{
				beforeChat: &memprovider.BeforeChatResult{
					ContextText: "remembered fact", RetrievalMode: "graph", ResultCount: 1, ResultRefs: []string{"memory-1"},
				},
				afterChat: make(chan memprovider.AfterChatRequest, 1),
			}
			registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
			registry.Register(storeRoundMemoryProviderID, memory)
			service := newACPLifecycleService(t, &recordingACPPrompter{}, messages, lifecycles)
			service.memoryRegistry = registry
			service.settingsService = settings.NewService(slog.New(slog.DiscardHandler), &storeRoundSettingsQueries{}, nil, nil)
			service.sessionRuntime = &lifecycleTurnAdmitter{admission: lifecycleTestAdmission()}
			registerExternalDrivers(service, &drivePromptRecorder{})

			entry.run(t, service)

			_, snapshot := requireACPLifecycle(t, lifecycles, lifecycleTestRunID, contextLifecycleStatusCompleted)
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

var _ = external.PromptInput{}
