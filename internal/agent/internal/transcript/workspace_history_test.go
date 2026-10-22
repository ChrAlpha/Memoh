package transcript

import (
	"context"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agentapi"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	session "github.com/felinics/memoh/internal/chat/thread"
)

func TestInjectWorkspaceTransitionRecordsMarksComputerChanges(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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

func TestInjectWorkspaceTransitionRecordsGovernsEveryRun(t *testing.T) {
	t.Parallel()

	raw := []historyfrag.HistoryRecord{
		workspaceHistoryRecord("user", "on a", "computer-a", "remote", "Computer A", "/a"),
		workspaceHistoryRecord("user", "to b", "computer-b", "remote", "Computer B", "/b"),
	}
	retained := injectWorkspaceTransitionRecords(raw)
	if len(retained) != 4 {
		t.Fatalf("retained = %d, want two markers and two messages", len(retained))
	}
	assertGovernedWorkspaceRuns(t, retained)
}

// A request's own row that the load left out is pulled back with the window
// after it; every Computer that window touches still gets its marker.
func TestAssembleDerivesMarkersForRequiredPullbacks(t *testing.T) {
	t.Parallel()

	location := func(id, name string) map[string]any {
		return map[string]any{"execution_location": map[string]any{"target_id": id, "kind": "remote", "name": name}}
	}
	required := agenttest.AssembleRow(t, "u-a", sdk.UserMessage("required a question"), agenttest.AssembleBase, location("computer-a", "Computer A"))
	middle := agenttest.AssembleRow(t, "a-a", sdk.AssistantMessage("answer on a"), agenttest.AssembleBase.Add(time.Minute), location("computer-a", "Computer A"))
	current := agenttest.AssembleRow(t, "u-b", sdk.UserMessage("current on b"), agenttest.AssembleBase.Add(2*time.Minute), location("computer-b", "Computer B"))
	source := &agenttest.AssembleMessages{
		After:   []messagepkg.Message{current},
		Visible: []messagepkg.Message{required, middle, current},
	}

	assembled, err := newAssembleService(source).Assemble(context.Background(),
		assembleRequest(PurposeRun, runreq.ChatRequest{RequiredHistoryMessageID: "u-a"}))
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	keptRequired := false
	for _, record := range assembled.Records {
		keptRequired = keptRequired || (record.Required && record.DBMessageID == "u-a")
	}
	if !keptRequired {
		t.Fatalf("required record dropped: %#v", recordTexts(assembled.Records))
	}
	if len(assembled.Records) != 5 {
		t.Fatalf("records = %#v, want two markers and three messages", recordTexts(assembled.Records))
	}
	assertGovernedWorkspaceRuns(t, assembled.Records)
}

func TestWorkspaceTransitionRendererMatchesLiveAndReplay(t *testing.T) {
	t.Parallel()

	current := &agentapi.WorkspaceTarget{TargetID: "computer-b", Kind: "remote", Name: "Computer B"}
	reader := New(Deps{})
	live := reader.CurrentWorkspaceMessage(t.Context(), runreq.ChatRequest{WorkspaceTarget: current})
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

func TestCurrentWorkspaceMessageOmitsUnchangedTarget(t *testing.T) {
	t.Parallel()

	current := &agentapi.WorkspaceTarget{TargetID: "computer-b", Kind: "remote", Name: "Computer B"}
	reader := New(Deps{Sessions: workspaceSessions(current)})

	if got := reader.CurrentWorkspaceMessage(t.Context(), runreq.ChatRequest{
		ThreadID:        "session-1",
		WorkspaceTarget: current,
	}); got != nil {
		t.Fatalf("unchanged workspace marker = %q, want nil", got.TextContent())
	}
}

func TestWorkspaceTransitionRendererMatchesLiveAndReplayAfterChange(t *testing.T) {
	t.Parallel()

	previous := &agentapi.WorkspaceTarget{TargetID: "computer-a", Kind: "remote", Name: "Computer A"}
	current := &agentapi.WorkspaceTarget{TargetID: "computer-b", Kind: "remote", Name: "Computer B"}
	reader := New(Deps{Sessions: workspaceSessions(previous)})
	live := reader.CurrentWorkspaceMessage(t.Context(), runreq.ChatRequest{
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

func workspaceSessions(target *agentapi.WorkspaceTarget) *agenttest.Sessions {
	return &agenttest.Sessions{
		GetFn: func(context.Context, string) (session.Thread, error) {
			return session.Thread{Metadata: map[string]any{
				"workspace_target": map[string]any{
					"target_id": target.TargetID,
					"kind":      target.Kind,
					"name":      target.Name,
				},
			}}, nil
		},
	}
}

func workspaceHistoryRecord(role, text, targetID, kind, name, path string) historyfrag.HistoryRecord {
	return historyfrag.HistoryRecord{
		ModelMessage: runreq.ModelMessage{Role: role, Content: runreq.NewTextContent(text)},
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

// assertGovernedWorkspaceRuns fails when any kept message carrying an
// execution location is not preceded by a marker for that location.
func assertGovernedWorkspaceRuns(t *testing.T, retained []historyfrag.HistoryRecord) {
	t.Helper()
	for index, record := range retained {
		if record.Synthetic {
			continue
		}
		target := workspaceTargetFromMetadata(record.Metadata)
		if target == nil {
			continue
		}
		governed := false
		for _, earlier := range retained[:index] {
			if earlier.Synthetic && strings.Contains(earlier.ModelMessage.TextContent(), "target_id=\""+target.TargetID+"\"") {
				governed = true
			}
		}
		if !governed {
			t.Fatalf("record %d (%s) is not governed by a marker for %s: %#v", index, record.ModelMessage.TextContent(), target.TargetID, recordTexts(retained))
		}
	}
}
