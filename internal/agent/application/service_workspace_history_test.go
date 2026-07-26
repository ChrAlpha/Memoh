package application

import (
	"context"
	"strings"
	"testing"

	historyfrag "github.com/memohai/memoh/internal/agent/context/history"
	"github.com/memohai/memoh/internal/bots"
	sessionpkg "github.com/memohai/memoh/internal/chat/thread"
	"github.com/memohai/memoh/internal/workspace"
)

type workspaceRequestTargetService struct{}

func (workspaceRequestTargetService) ResolveWorkspaceTarget(_ context.Context, _ string, targetID string) (workspace.ResolvedWorkspaceTarget, error) {
	return workspace.ResolvedWorkspaceTarget{
		TargetID: strings.TrimSpace(targetID),
		Kind:     workspace.WorkspaceTargetRemote,
		Name:     "Computer B",
	}, nil
}

type workspaceRequestPermission bool

func (allowed workspaceRequestPermission) HasBotPermission(_ context.Context, _, _, permission string) (bool, error) {
	return bool(allowed) && permission == bots.PermissionWorkspaceRead, nil
}

func TestPrepareWorkspaceRequestRequiresWorkspaceRead(t *testing.T) {
	base := ChatRequest{BotID: "bot-1", WorkspaceTargetID: "computer-b"}

	resolver := &Service{workspaceTargets: workspaceRequestTargetService{}}
	if _, _, err := resolver.prepareWorkspaceRequest(t.Context(), base); err == nil || !strings.Contains(err.Error(), "user id") {
		t.Fatalf("missing user error = %v", err)
	}

	base.UserID = "user-1"
	if _, _, err := resolver.prepareWorkspaceRequest(t.Context(), base); err == nil || !strings.Contains(err.Error(), "permission checker") {
		t.Fatalf("missing checker error = %v", err)
	}

	resolver.botPermissions = workspaceRequestPermission(false)
	if _, _, err := resolver.prepareWorkspaceRequest(t.Context(), base); err == nil || !strings.Contains(err.Error(), "workspace_read") {
		t.Fatalf("denied permission error = %v", err)
	}

	resolver.botPermissions = workspaceRequestPermission(true)
	ctx, got, err := resolver.prepareWorkspaceRequest(t.Context(), base)
	if err != nil {
		t.Fatalf("prepare allowed request: %v", err)
	}
	if got.WorkspaceTarget == nil || got.WorkspaceTarget.TargetID != "computer-b" {
		t.Fatalf("workspace snapshot = %#v", got.WorkspaceTarget)
	}
	if targetID := workspace.WorkspaceTargetFromContext(ctx); targetID != "computer-b" {
		t.Fatalf("context target = %q, want computer-b", targetID)
	}
}

func TestInjectWorkspaceTransitionRecordsMarksComputerChanges(t *testing.T) {
	records := []historyfrag.HistoryRecord{
		workspaceHistoryRecord("user", "first", "computer-b", "remote", "Computer B", "/work/b"),
		workspaceHistoryRecord("assistant", "done", "computer-b", "remote", "Computer B", "/work/b"),
		workspaceHistoryRecord("user", "continue", "native", "native", "Server Workspace", "/data"),
	}

	got := injectWorkspaceTransitionRecords(records)
	if len(got) != 5 {
		t.Fatalf("record count = %d, want 5", len(got))
	}
	if got[0].ModelMessage.Role != "system" || !strings.Contains(got[0].ModelMessage.TextContent(), "Computer B") {
		t.Fatalf("initial marker = %#v", got[0].ModelMessage)
	}
	if got[3].ModelMessage.Role != "system" {
		t.Fatalf("switch marker role = %q, want system", got[3].ModelMessage.Role)
	}
	switchText := got[3].ModelMessage.TextContent()
	for _, want := range []string{"Computer B", "Server Workspace", "do not transfer"} {
		if !strings.Contains(switchText, want) {
			t.Fatalf("switch marker %q does not contain %q", switchText, want)
		}
	}
}

func TestInjectWorkspaceTransitionRecordsIgnoresLegacyStartingFolderChanges(t *testing.T) {
	records := []historyfrag.HistoryRecord{
		workspaceHistoryRecord("user", "first", "computer-a", "remote", "Computer A", "/work/one"),
		workspaceHistoryRecord("user", "second", "computer-a", "remote", "Computer A", "/work/two"),
	}

	got := injectWorkspaceTransitionRecords(records)
	if len(got) != 3 {
		t.Fatalf("record count = %d, want 3", len(got))
	}
	if strings.Contains(got[0].ModelMessage.TextContent(), "starting_folder") {
		t.Fatalf("legacy workspace_path leaked into marker: %#v", got[0].ModelMessage)
	}
}

func TestWorkspaceTransitionRendererMatchesLiveAndReplay(t *testing.T) {
	current := &WorkspaceTarget{TargetID: "computer-b", Kind: "remote", Name: "Computer B"}
	service := &Service{}
	live := service.currentWorkspaceContextMessage(t.Context(), ChatRequest{WorkspaceTarget: current})
	if live == nil {
		t.Fatal("initial live context must include a workspace snapshot")
	}

	replayed := injectWorkspaceTransitionRecords([]historyfrag.HistoryRecord{
		workspaceHistoryRecord("user", "first", current.TargetID, current.Kind, current.Name, "/work/b"),
	})
	if len(replayed) != 2 {
		t.Fatalf("replayed records = %d, want marker + message", len(replayed))
	}
	if got, want := live.TextContent(), replayed[0].ModelMessage.TextContent(); got != want {
		t.Fatalf("initial live/replay marker mismatch:\nlive:   %s\nreplay: %s", got, want)
	}
}

func TestCurrentWorkspaceContextMessageOmitsUnchangedTarget(t *testing.T) {
	current := &WorkspaceTarget{TargetID: "computer-b", Kind: "remote", Name: "Computer B"}
	service := &Service{
		sessionService: &fakeBackgroundSessionService{
			getFn: func(context.Context, string) (sessionpkg.Thread, error) {
				return sessionpkg.Thread{Metadata: map[string]any{
					"workspace_target": map[string]any{
						"target_id": current.TargetID,
						"kind":      current.Kind,
						"name":      current.Name,
					},
				}}, nil
			},
		},
	}

	if got := service.currentWorkspaceContextMessage(t.Context(), ChatRequest{
		ThreadID:        "session-1",
		WorkspaceTarget: current,
	}); got != nil {
		t.Fatalf("unchanged workspace marker = %q, want nil", got.TextContent())
	}
}

func TestWorkspaceTransitionRendererMatchesLiveAndReplayAfterChange(t *testing.T) {
	previous := &WorkspaceTarget{TargetID: "computer-a", Kind: "remote", Name: "Computer A"}
	current := &WorkspaceTarget{TargetID: "computer-b", Kind: "remote", Name: "Computer B"}
	service := &Service{
		sessionService: &fakeBackgroundSessionService{
			getFn: func(context.Context, string) (sessionpkg.Thread, error) {
				return sessionpkg.Thread{Metadata: map[string]any{
					"workspace_target": map[string]any{
						"target_id": previous.TargetID,
						"kind":      previous.Kind,
						"name":      previous.Name,
					},
				}}, nil
			},
		},
	}
	live := service.currentWorkspaceContextMessage(t.Context(), ChatRequest{
		ThreadID:        "session-1",
		WorkspaceTarget: current,
	})
	if live == nil {
		t.Fatal("changed live context must include a workspace transition")
	}

	replayed := injectWorkspaceTransitionRecords([]historyfrag.HistoryRecord{
		workspaceHistoryRecord("user", "first", previous.TargetID, previous.Kind, previous.Name, "/work/a"),
		workspaceHistoryRecord("user", "second", current.TargetID, current.Kind, current.Name, "/work/b"),
	})
	if len(replayed) != 4 {
		t.Fatalf("replayed records = %d, want two markers + two messages", len(replayed))
	}
	if got, want := live.TextContent(), replayed[2].ModelMessage.TextContent(); got != want {
		t.Fatalf("changed live/replay marker mismatch:\nlive:   %s\nreplay: %s", got, want)
	}
}

func workspaceHistoryRecord(role, text, targetID, kind, name, path string) historyfrag.HistoryRecord {
	return historyfrag.HistoryRecord{
		ModelMessage: ModelMessage{Role: role, Content: newTextContent(text)},
		Metadata: map[string]any{
			"execution_location": map[string]any{
				"target_id":      targetID,
				"kind":           kind,
				"name":           name,
				"workspace_path": path,
			},
		},
	}
}

func TestTrimKeepsWorkspaceMarkersAtomicUnderBudget(t *testing.T) {
	t.Parallel()

	records := injectWorkspaceTransitionRecords([]historyfrag.HistoryRecord{
		workspaceHistoryRecord("user", strings.Repeat("work on a ", 30), "computer-a", "remote", "Computer A", "/a"),
		workspaceHistoryRecord("user", strings.Repeat("switch to b ", 30), "computer-b", "remote", "Computer B", "/b"),
		workspaceHistoryRecord("user", "current question", "computer-b", "remote", "Computer B", "/b"),
	})

	// Budget sized so a naive cutoff would drop Computer B's marker while
	// keeping its message: the trim must drop the pair instead, because a
	// message without its marker runs under the wrong implied workspace and
	// re-adding markers after the trim would break the hard budget.
	budget := 0
	for _, record := range records[3:] {
		budget += estimateMessageTokens(record.ModelMessage)
	}
	budget += estimateMessageTokens(records[2].ModelMessage) / 2

	messages, retained, estimated := trimMessagesAndRecordsByTokens(nil, records, budget)
	for index, record := range retained {
		if !record.Synthetic {
			continue
		}
		if index+1 >= len(retained) || retained[index+1].Synthetic {
			t.Fatalf("marker at %d is not followed by its message: %#v", index, recordTexts(retained))
		}
	}
	if len(retained) > 0 && retained[0].Synthetic && len(retained) == 1 {
		t.Fatalf("retained a marker without any message: %#v", recordTexts(retained))
	}
	for _, record := range retained {
		if record.Synthetic {
			continue
		}
		if strings.Contains(record.ModelMessage.TextContent(), "switch to b") {
			// If B's message survived, its marker must be directly before it.
			found := false
			for index, kept := range retained {
				if kept.ModelMessage.TextContent() == record.ModelMessage.TextContent() && index > 0 && retained[index-1].Synthetic {
					found = true
				}
			}
			if !found {
				t.Fatalf("kept Computer B message without its marker: %#v", recordTexts(retained))
			}
		}
	}
	if estimated > budget {
		t.Fatalf("estimated = %d, want a hard bound at budget %d", estimated, budget)
	}
	if len(messages) == 0 || messages[0].Role != "system" || !strings.Contains(messages[0].TextContent(), "trimmed") {
		t.Fatalf("trim notice missing after a real cut: %#v", messages)
	}
}

func TestTrimWithoutBudgetKeepsMarkersAndMessages(t *testing.T) {
	t.Parallel()

	records := injectWorkspaceTransitionRecords([]historyfrag.HistoryRecord{
		workspaceHistoryRecord("user", "on a", "computer-a", "remote", "Computer A", "/a"),
		workspaceHistoryRecord("user", "to b", "computer-b", "remote", "Computer B", "/b"),
	})
	messages, retained, _ := trimMessagesAndRecordsByTokens(nil, records, 0)
	if len(retained) != 4 || len(messages) != 4 {
		t.Fatalf("retained/messages = %d/%d, want markers and messages intact", len(retained), len(messages))
	}
}
