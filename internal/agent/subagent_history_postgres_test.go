package agent

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	tools "github.com/felinics/memoh/internal/agent/tool"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

const subagentPartialText = "partial subagent answer"

// spawnTextProvider streams subagentPartialText and then either finishes the
// step or stalls until its request ends.
type spawnTextProvider struct {
	abortAlignmentProvider
	stall bool
}

func (p spawnTextProvider) DoStream(ctx context.Context, _ sdk.Request) (<-chan sdk.StreamPart, error) {
	parts := make(chan sdk.StreamPart, 16)
	go func() {
		defer close(parts)
		parts <- &sdk.StartPart{}
		parts <- &sdk.StartStepPart{}
		parts <- &sdk.TextStartPart{ID: "t"}
		parts <- &sdk.TextDeltaPart{ID: "t", Text: subagentPartialText}
		if p.stall {
			<-ctx.Done()
			return
		}
		parts <- &sdk.TextEndPart{ID: "t"}
		parts <- &sdk.FinishStepPart{FinishReason: sdk.FinishReasonStop}
		parts <- &sdk.FinishPart{FinishReason: sdk.FinishReasonStop}
	}()
	return parts, nil
}

type spawnHistoryRun struct {
	sessionRun [2]string // state, error_code
	rows       []messagepkg.Message
}

// coded is the history rows that carry a failure code.
func (r spawnHistoryRun) coded() []string {
	var codes []string
	for _, row := range r.rows {
		if code, _ := row.Metadata[messagepkg.HistoryErrorCodeMetadataKey].(string); code != "" {
			codes = append(codes, code)
		}
	}
	return codes
}

func (r spawnHistoryRun) lines() []string {
	var lines []string
	for _, row := range r.rows {
		line := row.Role
		if row.Role != "user" {
			line += " " + strconv.Quote(historyRowText(row))
		}
		if row.Metadata[messagepkg.AgentStepInterruptedMetadataKey] == true {
			line += " interrupted"
		}
		lines = append(lines, line)
	}
	return lines
}

type subagentEnding int

const (
	subagentCompletes subagentEnding = iota
	// subagentGoesSilent leaves the model call stalled until the spawn
	// watchdog ends the attempt.
	subagentGoesSilent
	subagentIsStopped
)

// runSubagent admits one spawned run on a real runtime and message store, runs
// one attempt of it through the spawn adapter, and ends it the way the spawn
// provider does.
func runSubagent(t *testing.T, ending subagentEnding) spawnHistoryRun {
	t.Helper()
	h := newWSStepHistoryHarness(t, wsStepHistoryBlock)
	ctx := context.Background()
	runCtx, admission, finish, err := h.service.AdmitSubagentRun(ctx, h.botID, h.sessionID,
		"subagent:history-"+strconv.Itoa(int(ending)), []byte(`{"task":"history"}`))
	if err != nil {
		t.Fatalf("admit subagent run: %v", err)
	}
	userContent, _ := historyfrag.MarshalStoredSDKMessage(sdk.UserMessage("task"))
	position := admission.TurnPosition
	request, err := h.messages.Persist(ctx, messagepkg.PersistInput{
		BotID: h.botID, SessionID: h.sessionID, Role: "user", Content: userContent,
		RunID: admission.RunID, TurnID: admission.TurnID, TurnPosition: &position,
	})
	if err != nil {
		t.Fatalf("persist task row: %v", err)
	}
	adapter := native.NewSpawnAdapter(native.New(native.Deps{}))
	adapter.SetStepCommitFactory(h.service.SubagentStepCommit)
	adapter.SetRunObserverFactory(h.service.SubagentRunObserver)

	attemptCtx, watchdog := tools.NewSubagentWatchdog(runCtx, 400*time.Millisecond, slog.New(slog.DiscardHandler))
	defer watchdog.Stop()
	cfg := tools.SpawnRunConfig{
		RunID:                admission.RunID,
		TurnRequestMessageID: request.ID,
		Model: &sdk.Model{
			ID:       "subagent-history-model",
			Provider: spawnTextProvider{stall: ending != subagentCompletes},
			Type:     sdk.ModelTypeChat,
		},
		Query: "task",
		Identity: tools.SpawnIdentity{
			BotID: h.botID, SessionID: h.sessionID, IsSubagent: true,
		},
	}
	if ending == subagentIsStopped {
		go func() {
			time.Sleep(150 * time.Millisecond)
			if _, err := h.manager.AbortControl(ctx, h.botID, h.sessionID, admission.RunID, "stop-1"); err != nil {
				t.Errorf("AbortControl() error = %v", err)
			}
		}()
	}
	_, runErr := adapter.GenerateWithWatchdog(attemptCtx, cfg, watchdog.Touch)
	terminal := tools.SubagentTerminal{OutcomeResolved: true, Outcome: tools.SpawnAttemptCompleted}
	switch ending {
	case subagentGoesSilent:
		if !errors.Is(runErr, tools.ErrWatchdogTimedOut) {
			t.Fatalf("attempt error = %v, want the watchdog's", runErr)
		}
		terminal = tools.SubagentTerminal{Cause: runErr, OutcomeResolved: true, Outcome: tools.SpawnAttemptFailure}
	case subagentIsStopped:
		terminal.Outcome = tools.SpawnAttemptAbort
	case subagentCompletes:
		if runErr != nil {
			t.Fatalf("attempt error = %v", runErr)
		}
	}
	finish(terminal)

	var out spawnHistoryRun
	if err := h.pool.QueryRow(ctx, `
		SELECT state, COALESCE(error_code, '') FROM session_runs WHERE run_id = $1
	`, admission.RunID).Scan(&out.sessionRun[0], &out.sessionRun[1]); err != nil {
		t.Fatalf("read session_runs: %v", err)
	}
	if out.rows, err = h.messages.ListBySession(ctx, h.sessionID); err != nil {
		t.Fatalf("list history: %v", err)
	}
	return out
}

// A spawned run that fails after its model call was checkpointed names the
// failure on that row: the code session_runs records, on exactly one row.
func TestPostgresSubagentCheckpointCarriesTheRunsFailureCode(t *testing.T) {
	got := runSubagent(t, subagentGoesSilent)
	if got.sessionRun[0] != "failed" || got.sessionRun[1] == "" {
		t.Fatalf("session_runs = %q, want a failed run with a code", got.sessionRun)
	}
	assertStrings(t, "history rows", got.lines(), []string{
		`user`,
		`assistant "` + subagentPartialText + `" interrupted`,
	})
	assertStrings(t, "coded rows", got.coded(), []string{got.sessionRun[1]})
}

// A stop is not a failure: the checkpoint is written without a code.
func TestPostgresSubagentStopLeavesTheCheckpointUncoded(t *testing.T) {
	got := runSubagent(t, subagentIsStopped)
	if got.sessionRun[0] != "aborted" || got.sessionRun[1] != "" {
		t.Fatalf("session_runs = %q, want an aborted run without a code", got.sessionRun)
	}
	assertStrings(t, "history rows", got.lines(), []string{
		`user`,
		`assistant "` + subagentPartialText + `" interrupted`,
	})
	assertStrings(t, "coded rows", got.coded(), nil)
}

func TestPostgresSubagentCompletionWritesTheStepRows(t *testing.T) {
	got := runSubagent(t, subagentCompletes)
	if got.sessionRun[0] != "completed" || got.sessionRun[1] != "" {
		t.Fatalf("session_runs = %q, want a completed run", got.sessionRun)
	}
	assertStrings(t, "history rows", got.lines(), []string{
		`user`,
		`assistant "` + subagentPartialText + `"`,
	})
	assertStrings(t, "coded rows", got.coded(), nil)
}
