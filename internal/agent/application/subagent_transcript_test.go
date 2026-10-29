package application

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	tools "github.com/felinics/memoh/internal/agent/tool"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

const (
	subagentTranscriptBotID     = "00000000-0000-0000-0000-00000000b247"
	subagentTranscriptSessionID = "00000000-0000-0000-0000-00000000e247"
)

// newestWindowHistory serves what the visible-history query serves: the
// newest rows whose content fits the byte budget, oldest first, and the
// folded placement rows instead of the raw ones when asked to fold.
type newestWindowHistory struct {
	*recordingMessageService
	rows, folded []messagepkg.Message
	budgets      []int64
	folds        []bool
}

func (h *newestWindowHistory) ListActiveSinceBySessionWithinBytes(_ context.Context, _ string, _ time.Time, budget int64, fold bool) ([]messagepkg.Message, error) {
	h.budgets = append(h.budgets, budget)
	h.folds = append(h.folds, fold)
	rows := h.rows
	if fold && h.folded != nil {
		rows = h.folded
	}
	start, used := len(rows), int64(0)
	for start > 0 && used+int64(len(rows[start-1].Content)) <= budget {
		start--
		used += int64(len(rows[start].Content))
	}
	return append([]messagepkg.Message(nil), rows[start:]...), nil
}

func subagentTranscriptRow(t *testing.T, index int, role, text string) messagepkg.Message {
	t.Helper()
	row := persistedHistoryMessage(t, fmt.Sprintf("row-%03d", index), role, text)
	row.BotID = subagentTranscriptBotID
	row.SessionID = subagentTranscriptSessionID
	row.CreatedAt = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(index) * time.Minute)
	return row
}

func sdkMessageTexts(messages []sdk.Message) []string {
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		var b strings.Builder
		for _, part := range message.Content {
			switch part := part.(type) {
			case sdk.TextPart:
				b.WriteString(part.Text)
			case sdk.ToolCallPart:
				b.WriteString("call:" + part.ToolCallID)
			case sdk.ToolResultPart:
				b.WriteString("result:" + part.ToolCallID)
			}
		}
		texts = append(texts, string(message.Role)+" "+b.String())
	}
	return texts
}

// A long-lived agent thread replays its newest rows inside the subagent
// model's window, never the oldest ones and never the whole thread.
func TestSubagentTranscriptKeepsTheNewestRowsInsideTheModelBudget(t *testing.T) {
	t.Parallel()

	var rows []messagepkg.Message
	for i := range 40 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		rows = append(rows, subagentTranscriptRow(t, i, role, fmt.Sprintf("message %02d %s", i, strings.Repeat("x", 200))))
	}
	history := &newestWindowHistory{recordingMessageService: &recordingMessageService{}, rows: rows}
	svc := &Service{messageService: history, queries: &recordingCompactionLogQueries{}, contextAbsoluteCapTokens: 100_000}

	got, err := svc.LoadSubagentTranscript(context.Background(), tools.SubagentTranscriptRequest{
		BotID:              subagentTranscriptBotID,
		SessionID:          subagentTranscriptSessionID,
		ContextTokenBudget: 600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(history.folds, []bool{true}) || !slices.Equal(history.budgets, []int64{svc.historyLoadMaxBytes(600)}) {
		t.Fatalf("window folds=%v budgets=%v, want one folded load bounded by the model budget", history.folds, history.budgets)
	}
	texts := sdkMessageTexts(got)
	if len(texts) == 0 || len(texts) >= len(rows) {
		t.Fatalf("transcript has %d of %d rows, want a bounded window", len(texts), len(rows))
	}
	if !strings.HasPrefix(texts[len(texts)-1], "assistant message 39") {
		t.Fatalf("transcript ends with %q, want the newest row", texts[len(texts)-1])
	}
	for _, text := range texts {
		if strings.Contains(text, "message 00") {
			t.Fatalf("transcript kept the oldest row: %q", texts)
		}
	}
}

// The absolute cap bounds a model whose window is larger, and stands in for a
// model that reports none.
func TestSubagentTranscriptWindowNeverExceedsTheAbsoluteCap(t *testing.T) {
	t.Parallel()

	for _, budget := range []int{0, 1_000_000} {
		history := &newestWindowHistory{recordingMessageService: &recordingMessageService{}}
		svc := &Service{messageService: history, queries: &recordingCompactionLogQueries{}, contextAbsoluteCapTokens: 2_000}
		if _, err := svc.LoadSubagentTranscript(context.Background(), tools.SubagentTranscriptRequest{
			BotID: subagentTranscriptBotID, SessionID: subagentTranscriptSessionID, ContextTokenBudget: budget,
		}); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(history.budgets, []int64{svc.historyLoadMaxBytes(2_000)}) {
			t.Fatalf("budget %d loaded %v bytes, want the absolute cap", budget, history.budgets)
		}
	}
}

// The run sends its task as the query, so the task's persisted row is left
// out by identity; an earlier task with the same words is history.
func TestSubagentTranscriptLeavesOutOnlyTheRunsOwnRequest(t *testing.T) {
	t.Parallel()

	rows := []messagepkg.Message{
		subagentTranscriptRow(t, 0, "user", "repeat"),
		subagentTranscriptRow(t, 1, "assistant", "done once"),
		subagentTranscriptRow(t, 2, "user", "repeat"),
	}
	svc := &Service{
		messageService: &newestWindowHistory{recordingMessageService: &recordingMessageService{}, rows: rows},
		queries:        &recordingCompactionLogQueries{},
	}
	got, err := svc.LoadSubagentTranscript(context.Background(), tools.SubagentTranscriptRequest{
		BotID: subagentTranscriptBotID, SessionID: subagentTranscriptSessionID, RequestMessageID: rows[2].ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if texts, want := sdkMessageTexts(got), []string{"user repeat", "assistant done once"}; !slices.Equal(texts, want) {
		t.Fatalf("transcript = %q, want %q", texts, want)
	}
}

// A window can start after a tool call and end before its result. The
// transcript still pairs every call with a result, as a direct turn does.
func TestSubagentTranscriptClosesToolCallsTheWindowCuts(t *testing.T) {
	t.Parallel()

	at := time.Now().Add(-time.Hour)
	first, second := persistedToolTurn(t, 1, at), persistedToolTurn(t, 2, at)
	// first[2] answers call-1, which the window starts after; second[1]
	// calls call-2, whose result was never written.
	rows := []messagepkg.Message{first[2], first[3], second[0], second[1]}
	svc := &Service{
		messageService: &newestWindowHistory{recordingMessageService: &recordingMessageService{}, rows: rows},
		queries:        &recordingCompactionLogQueries{},
	}
	got, err := svc.LoadSubagentTranscript(context.Background(), tools.SubagentTranscriptRequest{
		BotID: lifecycleTestBotID, SessionID: lifecycleTestSessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"assistant answer 1", "user question 2", "assistant call:call-2", "tool result:call-2"}
	if texts := sdkMessageTexts(got); !slices.Equal(texts, want) {
		t.Fatalf("transcript = %q, want %q", texts, want)
	}
}

// A compacted agent thread replays its summary in place of the rows the
// summary replaced, which the folded window never loads.
func TestSubagentTranscriptReplaysCompactionSummaries(t *testing.T) {
	t.Parallel()

	const compactID = "00000000-0000-0000-0000-00000000c247"
	covered := []messagepkg.Message{
		subagentTranscriptRow(t, 0, "user", "old task"),
		subagentTranscriptRow(t, 1, "assistant", "old report"),
	}
	coverage := make([]compaction.CoveredSource, 0, len(covered))
	for i := range covered {
		covered[i].CompactID = compactID
		record, err := historyfrag.FromDBMessage(covered[i], historyfrag.ScopeFallback{})
		if err != nil {
			t.Fatal(err)
		}
		coverage = append(coverage, compaction.CoveredSource{Ref: record.Ref, CreatedAtMs: covered[i].CreatedAt.UnixMilli()})
	}
	rawCoverage, err := json.Marshal(coverage)
	if err != nil {
		t.Fatal(err)
	}
	placement := covered[1]
	placement.Summarized = true
	latest := []messagepkg.Message{
		subagentTranscriptRow(t, 2, "user", "new task"),
		subagentTranscriptRow(t, 3, "assistant", "new report"),
	}
	history := &newestWindowHistory{
		recordingMessageService: &recordingMessageService{},
		rows:                    append(append([]messagepkg.Message(nil), covered...), latest...),
		folded:                  append([]messagepkg.Message{placement}, latest...),
	}
	svc := &Service{
		messageService: history,
		queries: &recordingCompactionLogQueries{logs: []sqlc.BotHistoryMessageCompact{{
			ID:            mustPGUUID(t, compactID),
			BotID:         mustPGUUID(t, subagentTranscriptBotID),
			SessionID:     mustPGUUID(t, subagentTranscriptSessionID),
			Status:        "ok",
			Summary:       "the agent finished the old task",
			Coverage:      rawCoverage,
			AnchorStartMs: covered[0].CreatedAt.UnixMilli(),
			AnchorEndMs:   covered[1].CreatedAt.UnixMilli(),
		}}},
	}

	got, err := svc.LoadSubagentTranscript(context.Background(), tools.SubagentTranscriptRequest{
		BotID: subagentTranscriptBotID, SessionID: subagentTranscriptSessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"user <summary>\nthe agent finished the old task\n</summary>", "user new task", "assistant new report"}
	if texts := sdkMessageTexts(got); !slices.Equal(texts, want) || !slices.Equal(history.folds, []bool{true}) {
		t.Fatalf("transcript = %q folds=%v, want %q from one folded load", texts, history.folds, want)
	}
}
