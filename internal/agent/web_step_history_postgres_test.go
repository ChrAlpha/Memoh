package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

func assertWSStepHistory(t *testing.T, got wsStepHistoryRun, wantRun [3]string, wantHistory, wantTurns []string) {
	t.Helper()
	if got.sessionRun != wantRun {
		t.Errorf("session_runs = %q, want %q", got.sessionRun, wantRun)
	}
	assertStrings(t, "history rows", got.history, wantHistory)
	assertStrings(t, "history turns", got.turns, wantTurns)
}

func TestPostgresWSStepHistorySuccess(t *testing.T) {
	h := newWSStepHistoryHarness(t, wsStepHistorySuccess)
	got := h.run(t, nil)
	assertWSStepHistory(t, got,
		[3]string{"completed", "", ""},
		[]string{
			`user`,
			`assistant "` + wsStepHistoryPartialText + `"`,
		},
		[]string{
			`user "` + directLifecyclePrompt + `"`,
			`assistant [text "` + wsStepHistoryPartialText + `"]`,
		},
	)
}

func TestPostgresWSStepHistoryStop(t *testing.T) {
	h := newWSStepHistoryHarness(t, wsStepHistoryBlock)
	got := h.run(t, func(ctx context.Context, runID string) {
		if _, err := h.manager.AbortControl(ctx, h.botID, h.sessionID, runID, "stop-1"); err != nil {
			t.Errorf("AbortControl() error = %v", err)
		}
	})
	assertWSStepHistory(t, got,
		[3]string{"aborted", "", ""},
		[]string{
			`user`,
			`assistant "` + wsStepHistoryPartialText + `" interrupted`,
		},
		[]string{
			`user "` + directLifecyclePrompt + `"`,
			`assistant [text "` + wsStepHistoryPartialText + `"]`,
		},
	)
	// A stop is not a failure: the interrupted row carries no code.
	want := []map[string]any{{messagepkg.AgentStepInterruptedMetadataKey: true}}
	if !reflect.DeepEqual(got.failureMeta, want) {
		t.Errorf("failure metadata = %#v, want %#v", got.failureMeta, want)
	}
}

// A run that fails before any step commits still records the send: the user
// message and an empty assistant row carrying the run's failure code, the
// rows Writer.FailureRow writes for a turn without step commits. The row's
// code is the one session_runs records.
func TestPostgresWSStepHistoryFailureWithoutSteps(t *testing.T) {
	h := newWSStepHistoryHarness(t, wsStepHistoryAuthFailure)
	got := h.run(t, nil)
	assertWSStepHistory(t, got,
		[3]string{"failed", "agent.provider_auth_failed", ""},
		[]string{
			`user`,
			`assistant "" interrupted error_code=agent.provider_auth_failed`,
		},
		[]string{
			`user "` + directLifecyclePrompt + `"`,
			`assistant [error agent.provider_auth_failed]`,
		},
	)
}

// A run the loop guard ends has failed with agent.loop_detected: the live
// frame, session_runs and history all carry that code. The streamed text is
// kept as an interrupted step with the code.
func TestPostgresWSStepHistoryUncodedFailure(t *testing.T) {
	const code = "agent.loop_detected"
	h := newWSStepHistoryHarness(t, wsStepHistoryTextLoop)
	got := h.run(t, nil)
	assertStrings(t, "failure frames", got.failureFrames, []string{"error " + code, "agent_abort " + code})
	if !strings.HasPrefix(got.text, wsStepHistoryPartialText+wsStepHistoryLoopText) {
		t.Fatalf("streamed text = %q, want the partial text and the repeated chunk", got.text)
	}
	assertWSStepHistory(t, got,
		[3]string{"failed", code, ""},
		[]string{
			`user`,
			`assistant "` + got.text + `" interrupted error_code=` + code,
		},
		[]string{
			`user "` + directLifecyclePrompt + `"`,
			`assistant [text "` + got.text + `"] [error ` + code + `]`,
		},
	)
}

// Text streamed by a model call that then fails is in no committed step. It
// is written as an interrupted step that carries the run's failure code, the
// same code session_runs records.
func TestPostgresWSStepHistoryFailureAfterPartialOutput(t *testing.T) {
	h := newWSStepHistoryHarness(t, wsStepHistoryBrokenStream)
	got := h.run(t, nil)
	assertOrdered(t, got.events, "text_delta", "error", "agent_abort")
	assertWSStepHistory(t, got,
		[3]string{"failed", "agent.response_interrupted", ""},
		[]string{
			`user`,
			`assistant "` + wsStepHistoryPartialText + `" interrupted error_code=agent.response_interrupted`,
		},
		[]string{
			`user "` + directLifecyclePrompt + `"`,
			`assistant [text "` + wsStepHistoryPartialText + `"] [error agent.response_interrupted]`,
		},
	)
}

// A failure after a committed step keeps that step and adds the text the
// failed model call streamed as an interrupted step with the run's code.
func TestPostgresWSStepHistoryFailureAfterCommittedStep(t *testing.T) {
	h := newWSStepHistoryHarness(t, wsStepHistoryToolThenBrokenStream)
	got := h.run(t, nil)
	assertOrdered(t, got.events, "tool_call_start", "step_end", "text_delta", "error", "agent_abort")
	assertWSStepHistory(t, got,
		[3]string{"failed", "agent.response_interrupted", ""},
		[]string{
			`user`,
			`assistant ""`,
			`tool ""`,
			`assistant "` + wsStepHistoryPartialText + `" interrupted error_code=agent.response_interrupted`,
		},
		[]string{
			`user "` + directLifecyclePrompt + `"`,
			`assistant [tool] [text "` + wsStepHistoryPartialText + `"] [error agent.response_interrupted]`,
		},
	)
	// The failure leaves exactly the interrupted mark and the run's code on the
	// row; no error text.
	want := []map[string]any{{
		messagepkg.AgentStepInterruptedMetadataKey: true,
		messagepkg.HistoryErrorCodeMetadataKey:     "agent.response_interrupted",
	}}
	if !reflect.DeepEqual(got.failureMeta, want) {
		t.Errorf("failure metadata = %#v, want %#v", got.failureMeta, want)
	}
}

// A failure after a committed step whose model call streamed nothing still
// adds a row with the run's code, so the turn shows why it ended.
func TestPostgresWSStepHistoryFailureAfterCommittedStepWithoutText(t *testing.T) {
	h := newWSStepHistoryHarness(t, wsStepHistoryToolThenAuthFailure)
	got := h.run(t, nil)
	assertOrdered(t, got.events, "tool_call_start", "step_end", "error")
	assertWSStepHistory(t, got,
		[3]string{"failed", "agent.provider_auth_failed", ""},
		[]string{
			`user`,
			`assistant ""`,
			`tool ""`,
			`assistant "" interrupted error_code=agent.provider_auth_failed`,
		},
		[]string{
			`user "` + directLifecyclePrompt + `"`,
			`assistant [tool] [error agent.provider_auth_failed]`,
		},
	)
}

// An idle timeout cancels the model call, so the runtime checkpoints its text
// itself before the run ends; that checkpoint is the run's only row for the
// call, carries the code session_runs records, and shows as one error block.
func TestPostgresWSStepHistoryIdleTimeout(t *testing.T) {
	h := newWSStepHistoryHarness(t, wsStepHistoryBlock, wsStepHistoryFixtureOptions(withFixtureOptions(withDrive(func(d *drive.Deps) {
		d.StreamIdleTimeout = 300 * time.Millisecond
		d.StreamIdleTimeoutMax = 300 * time.Millisecond
	}))))
	got := h.run(t, nil)
	assertWSStepHistory(t, got,
		[3]string{"failed", "agent.response_timeout", ""},
		[]string{
			`user`,
			`assistant "` + wsStepHistoryPartialText + `" interrupted error_code=agent.response_timeout`,
		},
		[]string{
			`user "` + directLifecyclePrompt + `"`,
			`assistant [text "` + wsStepHistoryPartialText + `"] [error agent.response_timeout]`,
		},
	)
	want := []map[string]any{{
		messagepkg.AgentStepInterruptedMetadataKey: true,
		messagepkg.HistoryErrorCodeMetadataKey:     "agent.response_timeout",
	}}
	if !reflect.DeepEqual(got.failureMeta, want) {
		t.Errorf("failure metadata = %#v, want %#v", got.failureMeta, want)
	}
}

// A message injected into a running turn is written by the step that carries
// it, between the tool exchange that preceded it and the answer that follows.
func TestPostgresWSStepHistoryInjectedMessageIsWrittenBetweenTheSteps(t *testing.T) {
	const marker = "INJECTED_BETWEEN_STEPS"
	h := newWSStepHistoryHarness(t, wsStepHistoryToolThenText)
	injectCh := make(chan turn.InjectMessage, 1)
	injectCh <- turn.InjectMessage{Text: marker, HeaderifiedText: marker}
	got := h.runRequest(t, nil, func(req *runreq.ChatRequest) { req.InjectCh = injectCh })
	if got.sessionRun[0] != "completed" {
		t.Fatalf("session_runs = %q, want completed", got.sessionRun)
	}
	rows, err := h.messages.ListBySession(context.Background(), h.sessionID)
	if err != nil {
		t.Fatalf("list history: %v", err)
	}
	var roles []string
	injectedAt := -1
	for i, row := range rows {
		roles = append(roles, row.Role)
		if row.Role == "user" && strings.Contains(string(row.Content), marker) {
			if injectedAt >= 0 {
				t.Fatalf("the injected message is written twice: rows %d and %d", injectedAt, i)
			}
			injectedAt = i
		}
	}
	assertStrings(t, "history roles", roles, []string{"user", "assistant", "tool", "user", "assistant"})
	if injectedAt != 3 {
		t.Fatalf("injected message is row %d, want 3: after the tool result, before the answer", injectedAt)
	}
}
