package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/drive"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// wsUnfenced runs the stream without a runtime fence, so its steps are not
// committed as they land and the turn is written once, when it ends.
func wsUnfenced() wsStepHistoryOption {
	return func(s *wsStepHistorySetup) {
		s.fixture = append(s.fixture, withFixtureOptions(withTurnAdmitter(unfencedSessionRuntime{s.manager})))
	}
}

// unfencedSessionRuntime is the session runtime as a run that carries no
// persistence fence sees it: steps are not committed as they land, and the
// turn is written once, when it ends.
type unfencedSessionRuntime struct{ *sessionruntime.Manager }

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

// A run without step commits writes its turn when it ends, and records a
// failure the same way: the user message and a row with the run's code.
func TestPostgresWSStepHistoryUnfencedFailure(t *testing.T) {
	for _, tc := range []struct {
		mode wsStepHistoryModel
		code string
	}{
		{wsStepHistoryAuthFailure, "agent.provider_auth_failed"},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			h := newWSStepHistoryHarness(t, tc.mode, wsUnfenced())
			got := h.run(t, nil)
			assertWSStepHistory(t, got,
				[3]string{"failed", tc.code, ""},
				[]string{
					`user`,
					`assistant "" interrupted error_code=` + tc.code,
				},
				[]string{
					`user "` + directLifecyclePrompt + `"`,
					`assistant [error ` + tc.code + `]`,
				},
			)
		})
	}
}

// A run the user stops has not failed, so without step commits it writes no
// failure row either.
func TestPostgresWSStepHistoryUnfencedStop(t *testing.T) {
	h := newWSStepHistoryHarness(t, wsStepHistoryBlock, wsUnfenced())
	got := h.run(t, func(ctx context.Context, runID string) {
		if _, err := h.manager.AbortControl(ctx, h.botID, h.sessionID, runID, "stop-1"); err != nil {
			t.Errorf("AbortControl() error = %v", err)
		}
	})
	assertWSStepHistory(t, got,
		[3]string{"aborted", "", ""},
		nil,
		nil,
	)
}

// A run the loop guard ends has failed with agent.loop_detected: the live
// frame, session_runs and history all carry that code. With step commits the
// streamed text is kept as an interrupted step with the code.
//
// Without step commits the guard's abort carries no snapshot of the text it
// streamed. Current behavior: the ending writes the failure row under the
// guard's code and then reports the missing snapshot as a failed history
// write, so session_runs and the frame name session_runtime.history_inconsistent
// while the row names agent.loop_detected. A Web run is always fenced, so it
// commits steps and never takes this path.
func TestPostgresWSStepHistoryUncodedFailure(t *testing.T) {
	const code = "agent.loop_detected"
	t.Run("steps", func(t *testing.T) {
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
	})
	t.Run("unfenced_current_behavior", func(t *testing.T) {
		const historyCode = "session_runtime.history_inconsistent"
		h := newWSStepHistoryHarness(t, wsStepHistoryTextLoop, wsUnfenced())
		got := h.run(t, nil)
		assertStrings(t, "failure frames", got.failureFrames, []string{"error " + historyCode})
		assertWSStepHistory(t, got,
			[3]string{"failed", historyCode, ""},
			[]string{
				`user`,
				`assistant "" interrupted error_code=` + code,
			},
			[]string{
				`user "` + directLifecyclePrompt + `"`,
				`assistant [error ` + code + `]`,
			},
		)
	})
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

// snapshotOnlyMessages is the message store of a run that commits no steps: it
// writes whole rounds atomically but is not an AgentStepPersister, so the run's
// history is written from its terminal snapshot.
type snapshotOnlyMessages struct {
	messagepkg.Service
	rounds messagepkg.AtomicRoundPersister
}

func (m snapshotOnlyMessages) PersistRound(ctx context.Context, inputs []messagepkg.PersistInput, options messagepkg.RoundPersistenceOptions) ([]messagepkg.Message, bool, error) {
	return m.rounds.PersistRound(ctx, inputs, options)
}

func wsSnapshotOnly() wsStepHistoryOption {
	return wsStepHistoryMessages(func(db *messagepkg.DBService) messagepkg.Service {
		return snapshotOnlyMessages{Service: db, rounds: db}
	})
}

// A run that commits no steps writes its turn from the terminal snapshot when
// it ends. A failure after output stores that output with the run's code on
// the interrupted row, and the row's failure metadata is the one the step path
// writes for the same failure. No separate failure row is added.
func TestPostgresWSSnapshotFailureAfterOutputKeepsTheFailureCode(t *testing.T) {
	const code = "agent.response_interrupted"
	steps := newWSStepHistoryHarness(t, wsStepHistoryToolThenBrokenStream).run(t, nil)
	got := newWSStepHistoryHarness(t, wsStepHistoryToolThenBrokenStream, wsSnapshotOnly()).run(t, nil)
	assertWSStepHistory(t, got,
		[3]string{"failed", code, ""},
		[]string{
			`user`,
			`assistant "" interrupted error_code=` + code,
			`tool ""`,
		},
		[]string{
			`user "` + directLifecyclePrompt + `"`,
			`assistant [tool] [error ` + code + `]`,
		},
	)
	want := []map[string]any{{
		messagepkg.AgentStepInterruptedMetadataKey: true,
		messagepkg.HistoryErrorCodeMetadataKey:     code,
	}}
	if !reflect.DeepEqual(got.failureMeta, want) {
		t.Errorf("snapshot path failure metadata = %#v, want %#v", got.failureMeta, want)
	}
	if !reflect.DeepEqual(got.failureMeta, steps.failureMeta) {
		t.Errorf("snapshot path failure metadata = %#v, step path = %#v, want the same", got.failureMeta, steps.failureMeta)
	}
}

// A stop after partial output is not a failure: a run that commits no steps
// writes no failure row and no code.
func TestPostgresWSSnapshotStopAfterPartialOutputRecordsNoFailure(t *testing.T) {
	h := newWSStepHistoryHarness(t, wsStepHistoryBlock, wsSnapshotOnly())
	got := h.run(t, func(ctx context.Context, runID string) {
		if _, err := h.manager.AbortControl(ctx, h.botID, h.sessionID, runID, "stop-1"); err != nil {
			t.Errorf("AbortControl() error = %v", err)
		}
	})
	assertWSStepHistory(t, got, [3]string{"aborted", "", ""}, nil, nil)
	if len(got.failureMeta) != 0 {
		t.Errorf("failure metadata = %#v, want none", got.failureMeta)
	}
}

// The answer a run was streaming when its model call timed out is stored with
// the timeout's code when the run commits no steps.
func TestPostgresWSSnapshotTimeoutAfterPartialOutputKeepsTheAnswerAndCode(t *testing.T) {
	h := newWSStepHistoryHarness(t, wsStepHistoryBlock, wsSnapshotOnly(), wsStepHistoryFixtureOptions(withFixtureOptions(withDrive(func(d *drive.Deps) {
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
// call and no second one is written.
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
			`assistant "` + wsStepHistoryPartialText + `" interrupted`,
		},
		[]string{
			`user "` + directLifecyclePrompt + `"`,
			`assistant [text "` + wsStepHistoryPartialText + `"]`,
		},
	)
}
