package agenttest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	session "github.com/felinics/memoh/internal/chat/thread"
)

// AssembleMessages serves a session's context rows the way the byte budgeted
// loaders do and counts which loader a read used.
type AssembleMessages struct {
	RecordingMessages
	After   []messagepkg.Message
	Recent  []messagepkg.Message
	Visible []messagepkg.Message

	AfterCalls  int
	RecentCalls int
	MaxBytes    int64
}

func (s *AssembleMessages) ListActiveAfterCompactionBySessionWithinBytes(_ context.Context, _ string, maxBytes int64) (messagepkg.SessionHistoryAfterCompaction, error) {
	s.AfterCalls++
	s.MaxBytes = maxBytes
	return messagepkg.SessionHistoryAfterCompaction{Messages: append([]messagepkg.Message(nil), s.After...)}, nil
}

func (s *AssembleMessages) ListRecentUserBySession(context.Context, string, int) ([]messagepkg.Message, error) {
	s.RecentCalls++
	return append([]messagepkg.Message(nil), s.Recent...), nil
}

func (s *AssembleMessages) ListVisibleFromForModelBySession(context.Context, string, string) ([]messagepkg.Message, error) {
	return append([]messagepkg.Message(nil), s.Visible...), nil
}

func (*AssembleMessages) GetByIDBySession(context.Context, string, string) (messagepkg.Message, error) {
	return messagepkg.Message{}, nil
}

// AssembleBase is the clock the row fixtures count from.
var AssembleBase = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// AdmissionHistoryMessage mirrors the byte-budgeted loaders, which hand over
// metadata undecoded.
func AdmissionHistoryMessage(t *testing.T, id string, message sdk.Message, metadata map[string]any) messagepkg.Message {
	t.Helper()
	content, err := json.Marshal(runreq.SDKMessagesToModelMessages([]sdk.Message{message})[0])
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	return messagepkg.Message{ID: id, Role: string(message.Role), Content: content, RawMetadata: raw}
}

// AssembleRow is a stored row as the loaders hand it over: content as JSON,
// metadata undecoded.
func AssembleRow(t *testing.T, id string, message sdk.Message, at time.Time, metadata map[string]any) messagepkg.Message {
	t.Helper()
	row := AdmissionHistoryMessage(t, id, message, metadata)
	row.CreatedAt = at
	return row
}

// AssembleText is AssembleRow for a plain text row of a user or an assistant.
func AssembleText(t *testing.T, id, role, text string, at time.Time) messagepkg.Message {
	t.Helper()
	message := sdk.UserMessage(text)
	if role == "assistant" {
		message = sdk.AssistantMessage(text)
	}
	return AssembleRow(t, id, message, at, nil)
}

// MessageTexts reduces model messages to their trimmed text.
func MessageTexts(messages []runreq.ModelMessage) []string {
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		texts = append(texts, strings.TrimSpace(message.TextContent()))
	}
	return texts
}

// AssertTexts fails the test unless got carries exactly the texts in want.
func AssertTexts(t *testing.T, got []runreq.ModelMessage, want ...string) {
	t.Helper()
	texts := MessageTexts(got)
	if len(texts) != len(want) {
		t.Fatalf("messages = %q, want %q", texts, want)
	}
	for i := range want {
		if texts[i] != want[i] {
			t.Fatalf("messages = %q, want %q", texts, want)
		}
	}
}

// SubagentSessions extends Sessions with the subagent thread surface the
// direct-chat path probes for.
type SubagentSessions struct {
	Sessions
	Config      session.SubagentConfig
	ConfigErr   error
	ForkContext []session.SubagentForkContextMessage
}

func (f *SubagentSessions) GetSubagentConfig(context.Context, string) (session.SubagentConfig, error) {
	return f.Config, f.ConfigErr
}

func (f *SubagentSessions) ListSubagentForkContext(context.Context, string) ([]session.SubagentForkContextMessage, error) {
	return f.ForkContext, nil
}

// HistoryRecord is a persisted history record with a durable ref and the given
// message; mutate adjusts it.
func HistoryRecord(id string, msg runreq.ModelMessage, mutate func(*historyfrag.HistoryRecord)) historyfrag.HistoryRecord {
	record := historyfrag.HistoryRecord{
		Ref: contextfrag.ContextRef{
			Namespace:   "bot_history_message",
			ID:          id,
			Version:     1,
			HashAlgo:    contextfrag.HashAlgoSHA256,
			HashScope:   contextfrag.HashScopeSourcePayload,
			ContentHash: HistorySourceHash(id),
			Schema:      contextfrag.SchemaContextRef,
			Durability:  contextfrag.RefDurable,
		},
		Kind:         contextfrag.KindConversationEvent,
		SourceKind:   historyfrag.SourceDBMessage,
		Lifecycle:    historyfrag.LifecyclePersisted,
		ModelMessage: msg,
		DBMessageID:  id,
	}
	if mutate != nil {
		mutate(&record)
	}
	return record
}

// HistorySourceHash is the source hash HistoryRecord gives the row id.
func HistorySourceHash(id string) string {
	return "test-source-hash-" + id
}

// PersistedHistoryMessage is a stored text row of a user or an assistant.
func PersistedHistoryMessage(t *testing.T, id string, role string, text string) messagepkg.Message {
	t.Helper()
	content, err := json.Marshal(runreq.ModelMessage{
		Role:    role,
		Content: runreq.NewTextContent(text),
	})
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	return messagepkg.Message{
		ID:      id,
		Role:    role,
		Content: content,
	}
}

// GetRunTurnRequest reports no request row: the double stores none.
func (*AssembleMessages) GetRunTurnRequest(context.Context, string, string, string) (messagepkg.RunTurnRequest, error) {
	return messagepkg.RunTurnRequest{}, messagepkg.ErrRunRequestNotFound
}
