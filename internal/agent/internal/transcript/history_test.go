package transcript

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/toolexec"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

type requiredHistoryMessageService struct {
	agenttest.RecordingMessages
	visible []messagepkg.Message
	byID    map[string]messagepkg.Message
}

func (s *requiredHistoryMessageService) ListVisibleFromForModelBySession(context.Context, string, string) ([]messagepkg.Message, error) {
	return append([]messagepkg.Message(nil), s.visible...), nil
}

func (s *requiredHistoryMessageService) GetByIDBySession(_ context.Context, _ string, messageID string) (messagepkg.Message, error) {
	msg, ok := s.byID[messageID]
	if !ok {
		return messagepkg.Message{}, errors.New("message not found")
	}
	return msg, nil
}

func TestEnsureRequiredHistoryMessageMergesVisibleWindowInOrder(t *testing.T) {
	t.Parallel()

	requiredUser := agenttest.PersistedHistoryMessage(t, "user-1", "user", "retry me")
	toolCallAssistant := agenttest.PersistedHistoryMessage(t, "assistant-tool-1", "assistant", "calling tool")
	toolResult := agenttest.PersistedHistoryMessage(t, "tool-1", "tool", "tool result")
	finalAssistant := agenttest.PersistedHistoryMessage(t, "assistant-final-1", "assistant", "old answer")
	service := &requiredHistoryMessageService{
		visible: []messagepkg.Message{requiredUser, toolCallAssistant, toolResult, finalAssistant},
		byID:    map[string]messagepkg.Message{"user-1": requiredUser},
	}
	resolver := New(Deps{Messages: service})
	existing := []historyfrag.HistoryRecord{
		agenttest.HistoryRecord("prior", runreq.ModelMessage{
			Role:    "assistant",
			Content: runreq.NewTextContent("prior context"),
		}, nil),
		agenttest.HistoryRecord("assistant-tool-1", runreq.ModelMessage{Role: "assistant", Content: runreq.NewTextContent("calling tool")}, nil),
		agenttest.HistoryRecord("tool-1", runreq.ModelMessage{Role: "tool", Content: runreq.NewTextContent("tool result")}, nil),
	}

	got, err := resolver.ensureRequiredHistoryMessage(context.Background(), existing, runreq.ChatRequest{
		ChatID:                       "bot-1",
		ThreadID:                     "session-1",
		RequiredHistoryMessageID:     "user-1",
		HistoryCutoffBeforeMessageID: "assistant-final-1",
	})
	if err != nil {
		t.Fatalf("ensureRequiredHistoryMessage() error = %v", err)
	}
	if gotIDs := historyRecordIDs(got); !equalStrings(gotIDs, []string{"prior", "user-1", "assistant-tool-1", "tool-1"}) {
		t.Fatalf("message ids = %v, want prior + ordered required window", gotIDs)
	}
	if !got[1].Required {
		t.Fatalf("required message was not marked")
	}
}

func TestFilterMessagesBeforeIDFailsClosedWhenCutoffIsMissing(t *testing.T) {
	t.Parallel()

	records := []historyfrag.HistoryRecord{
		agenttest.HistoryRecord("later-user", runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent("later")}, nil),
	}
	if got := filterMessagesBeforeID(records, "missing-cutoff"); len(got) != 0 {
		t.Fatalf("missing cutoff retained history: %#v", got)
	}
}

func TestLoadCompactionArtifactBoundaryFetchesCutoffOutsideLoadedWindow(t *testing.T) {
	t.Parallel()

	cutoff := agenttest.PersistedHistoryMessage(t, "old-cutoff", "assistant", "old answer")
	cutoff.CreatedAt = time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	resolver := New(Deps{Messages: &requiredHistoryMessageService{
		byID: map[string]messagepkg.Message{cutoff.ID: cutoff},
	}})

	boundary := resolver.LoadArtifactBoundary(context.Background(), nil, "session-1", cutoff.ID)
	if !boundary.HasCutoff || boundary.CutoffMs != cutoff.CreatedAt.UnixMilli() {
		t.Fatalf("boundary = %+v, want cutoff at %d", boundary, cutoff.CreatedAt.UnixMilli())
	}
}

func historyRecordIDs(messages []historyfrag.HistoryRecord) []string {
	out := make([]string, 0, len(messages))
	for _, msg := range messages {
		out = append(out, msg.DBMessageID)
	}
	return out
}

func equalStrings(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestEstimateMessageTokensCountsReplayedReasoningAndToolInput(t *testing.T) {
	t.Parallel()

	reasoning := strings.Repeat("r", 4000)
	input := strings.Repeat("i", 4000)
	message := runreq.SDKMessagesToModelMessages([]sdk.Message{{
		Role: sdk.MessageRoleAssistant,
		Content: []sdk.MessagePart{
			sdk.ReasoningPart{Text: reasoning, Format: sdk.ReasoningFormatOpenAIChat},
			sdk.TextPart{Text: "short answer"},
			sdk.ToolCallPart{ToolCallID: "call-1", ToolName: "write", Input: toolexec.ArgumentsFromValue(map[string]any{"body": input})},
		},
	}})[0]
	if got, floor := EstimateMessageTokens(message), contextfrag.TokensFromBytes(len(reasoning)+len(input)); got < floor {
		t.Fatalf("EstimateMessageTokens = %d, want at least %d for the replayed reasoning and tool input", got, floor)
	}
}
