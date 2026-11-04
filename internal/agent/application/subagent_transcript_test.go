package application

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/chat/checkpoint"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

const (
	subagentTranscriptBotID     = "00000000-0000-0000-0000-00000000b247"
	subagentTranscriptSessionID = "00000000-0000-0000-0000-00000000e247"
)

// newestWindowHistory serves what the history window query serves: the
// newest rows after the checkpoint whose content fits the byte budget, oldest
// first, and whether the budget left older ones out.
type newestWindowHistory struct {
	*recordingMessageService
	rows    []messagepkg.Message
	budgets []int64
	afters  []messagepkg.HistoryPosition
}

func (h *newestWindowHistory) ListHistoryWithinBytes(_ context.Context, _ string, window messagepkg.HistoryWindow) (messagepkg.HistoryLoad, error) {
	h.budgets = append(h.budgets, window.MaxBytes)
	h.afters = append(h.afters, window.After)
	start, used := len(h.rows), int64(0)
	for start > 0 && used+int64(len(h.rows[start-1].Content)) <= window.MaxBytes {
		start--
		used += int64(len(h.rows[start].Content))
	}
	return messagepkg.HistoryLoad{Messages: append([]messagepkg.Message(nil), h.rows[start:]...), Truncated: start > 0}, nil
}

func subagentTranscriptService(history *newestWindowHistory, queries *historyCheckpointQueries, capTokens int) *Service {
	svc := checkpointTestService(history, queries)
	svc.contextAbsoluteCapTokens = capTokens
	return svc
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

// A thread the subagent model's budget cannot hold compacts the rows before
// the task's turn first, keeping the task's request raw, and replays the
// newest rows after the fold: never the oldest ones, never a window that
// starts later than the checkpoint. A load still over the budget, or a
// compaction that fails, fails the task as it fails a direct turn.
func TestSubagentTranscriptCompactsWhatTheModelBudgetCannotHold(t *testing.T) {
	t.Parallel()

	const modelUUID = "00000000-0000-0000-0000-000000000451"
	for _, tc := range []struct {
		name  string
		fold  func(*newestWindowHistory) ([]compaction.Pass, error)
		fails bool
	}{
		{name: "folded", fold: func(h *newestWindowHistory) ([]compaction.Pass, error) {
			h.rows = h.rows[len(h.rows)-2:]
			return []compaction.Pass{{}}, nil
		}},
		{name: "still over", fails: true, fold: func(*newestWindowHistory) ([]compaction.Pass, error) {
			return []compaction.Pass{{}}, nil
		}},
		{name: "compaction failed", fails: true, fold: func(*newestWindowHistory) ([]compaction.Pass, error) {
			return nil, errors.New("summarizer down")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var rows []messagepkg.Message
			for i := range 40 {
				role := "user"
				if i%2 == 1 {
					role = "assistant"
				}
				rows = append(rows, subagentTranscriptRow(t, i, role, fmt.Sprintf("message %02d %s", i, strings.Repeat("x", 200))))
			}
			request := subagentTranscriptRow(t, 40, "user", "current task")
			rows = append(rows, request)
			history := &newestWindowHistory{recordingMessageService: &recordingMessageService{}, rows: rows}
			svc, _ := newControllerPolicyService(t)
			svc.messageService = history
			svc.checkpoints = emptyHistoryCheckpoints()
			svc.contextAbsoluteCapTokens = 100_000
			runner := &recoveryCompactionRunner{run: func() ([]compaction.Pass, error) { return tc.fold(history) }}
			svc.compactionService = runner
			turnPosition := int64(21)

			got, err := svc.LoadSubagentTranscript(context.Background(), tools.SubagentTranscriptRequest{
				BotID:              syncCompactBotID,
				SessionID:          subagentTranscriptSessionID,
				Query:              "current task",
				RequestMessageID:   request.ID,
				TurnPosition:       &turnPosition,
				ModelID:            modelUUID,
				ContextTokenBudget: 600,
			})
			if len(runner.configs) != 1 {
				t.Fatalf("compactions = %d, want one for the truncated load", len(runner.configs))
			}
			cfg := runner.configs[0]
			if !slices.Contains(cfg.Anchors, turn.ContextMessageSource{Kind: "history", ID: request.ID, Current: true}) ||
				cfg.CandidatesBefore == nil || *cfg.CandidatesBefore != (messagepkg.HistoryPosition{Turn: turnPosition}) ||
				cfg.HistoryBudgetTokens != 600 || cfg.CurrentRequest != "current task" || cfg.SessionID != subagentTranscriptSessionID {
				t.Fatalf("compaction = %+v, want the rows before the task's turn folded toward its request, kept raw", cfg)
			}
			if tc.fails {
				if apperror.CodeOf(err) != apperror.CodeContextBudgetUnsatisfied || got != nil {
					t.Fatalf("transcript = %d messages, err = %v, want the budget failure", len(got), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(history.budgets, []int64{svc.historyLoadMaxBytes(600), svc.historyLoadMaxBytes(600)}) {
				t.Fatalf("loads = %v, want two loads bounded by the model budget", history.budgets)
			}
			if texts := sdkMessageTexts(got); len(texts) != 1 || !strings.HasPrefix(texts[0], "assistant message 39") {
				t.Fatalf("transcript = %q, want the newest row without the task's request", texts)
			}
		})
	}
}

// The absolute cap bounds a model whose window is larger, and stands in for a
// model that reports none.
func TestSubagentTranscriptWindowNeverExceedsTheAbsoluteCap(t *testing.T) {
	t.Parallel()

	for _, budget := range []int{0, 1_000_000} {
		history := &newestWindowHistory{recordingMessageService: &recordingMessageService{}}
		svc := subagentTranscriptService(history, &historyCheckpointQueries{}, 2_000)
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
	svc := subagentTranscriptService(&newestWindowHistory{recordingMessageService: &recordingMessageService{}, rows: rows}, &historyCheckpointQueries{}, 0)
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
	svc := subagentTranscriptService(&newestWindowHistory{recordingMessageService: &recordingMessageService{}, rows: rows}, &historyCheckpointQueries{}, 0)
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

// A compacted agent thread replays its checkpoint, the summary in place of
// the rows it folded, and then the rows after the checkpoint's position.
func TestSubagentTranscriptReplaysItsHistoryCheckpoint(t *testing.T) {
	t.Parallel()

	queries := &historyCheckpointQueries{}
	summaryID := queries.summary(t, checkpointTestSummaryID, "the agent finished the old task")
	position := historyTestPosition(1)
	queries.activate(historyCheckpointRow(t, checkpointTestID, position, checkpoint.Item{SummaryID: summaryID}))
	history := &newestWindowHistory{
		recordingMessageService: &recordingMessageService{},
		rows: []messagepkg.Message{
			subagentTranscriptRow(t, 2, "user", "new task"),
			subagentTranscriptRow(t, 3, "assistant", "new report"),
		},
	}
	svc := subagentTranscriptService(history, queries, 0)

	got, err := svc.LoadSubagentTranscript(context.Background(), tools.SubagentTranscriptRequest{
		BotID: subagentTranscriptBotID, SessionID: subagentTranscriptSessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	texts := sdkMessageTexts(got)
	if len(texts) != 3 || !strings.HasPrefix(texts[0], "user ") || !strings.HasSuffix(texts[0], "<summary>\nthe agent finished the old task\n</summary>") ||
		!slices.Equal(texts[1:], []string{"user new task", "assistant new report"}) {
		t.Fatalf("transcript = %q, want the checkpoint's summary, then the rows after it", texts)
	}
	if !slices.Equal(history.afters, []messagepkg.HistoryPosition{position}) {
		t.Fatalf("rows loaded after %v, want after the checkpoint's position %v", history.afters, position)
	}
}
