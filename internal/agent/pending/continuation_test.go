package pending

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
)

type busyContinuations struct{ err error }

func (busyContinuations) Owed(context.Context, sessionruntime.Key) (Continuation, bool, error) {
	return Continuation{}, true, nil
}

func (b busyContinuations) Start(context.Context, Continuation) (<-chan struct{}, error) {
	return nil, b.err
}

func (busyContinuations) Retire(context.Context, Continuation, error) error { return nil }

// A continuation that finds no free slot defers its session without a warning:
// it is the expected state of a cold start and is retried by the next trigger.
// Any other failure to start is a warning.
func TestFullContinuationSlotsDeferTheSessionWithoutAWarning(t *testing.T) {
	for name, tc := range map[string]struct {
		err      error
		wantWarn bool
	}{
		"no slot free":   {err: fmt.Errorf("wrapped: %w", ErrContinuationsBusy)},
		"another reason": {err: errors.New("model unavailable"), wantWarn: true},
	} {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			inputs := New(Deps{
				Logger:        slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
				Queries:       noDiscuss{},
				Continuations: busyContinuations{err: tc.err},
			})
			outcome, err := inputs.admitPending(t.Context(), "bot-1", "session-1")
			require.NoError(t, err)
			require.Equal(t, DiscussSkipped, outcome, "the session waits either way")
			if tc.wantWarn {
				require.Contains(t, logs.String(), "level=WARN")
			} else {
				require.NotContains(t, logs.String(), "level=WARN")
			}
		})
	}
}

// flakyContinuations is owed one continuation whose start fails with err until
// the test clears it, and counts what is retired.
type flakyContinuations struct {
	err     error
	retired int
	started int
}

func (f *flakyContinuations) Owed(context.Context, sessionruntime.Key) (Continuation, bool, error) {
	return Continuation{}, f.retired == 0, nil
}

func (f *flakyContinuations) Start(context.Context, Continuation) (<-chan struct{}, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.started++
	return make(chan struct{}), nil
}

func (f *flakyContinuations) Retire(context.Context, Continuation, error) error {
	f.retired++
	return nil
}

// A start that fails for a reason that may clear keeps the intent and defers
// the session; one marked unrecoverable retires the intent.
func TestStartFailureKeepsTheIntentUnlessItIsUnrecoverable(t *testing.T) {
	continuations := &flakyContinuations{err: errors.New("database unavailable")}
	inputs := New(Deps{Logger: slog.New(slog.DiscardHandler), Queries: noDiscuss{}, Continuations: continuations})
	started, deferred, err := inputs.admitContinuation(t.Context(), sessionruntime.Key{BotID: "bot-1", SessionID: "session-1"})
	require.NoError(t, err)
	require.False(t, started)
	require.True(t, deferred, "the session waits for the next trigger")
	require.Zero(t, continuations.retired, "the intent stays")

	continuations.err = nil
	started, deferred, err = inputs.admitContinuation(t.Context(), sessionruntime.Key{BotID: "bot-1", SessionID: "session-1"})
	require.NoError(t, err)
	require.True(t, started)
	require.False(t, deferred)

	continuations = &flakyContinuations{err: fmt.Errorf("%w: no request row", runreq.ErrResumeUnrecoverable)}
	inputs = New(Deps{Logger: slog.New(slog.DiscardHandler), Queries: noDiscuss{}, Continuations: continuations})
	started, deferred, err = inputs.admitContinuation(t.Context(), sessionruntime.Key{BotID: "bot-1", SessionID: "session-1"})
	require.NoError(t, err)
	require.False(t, started)
	require.False(t, deferred, "nothing is owed once the intent is retired")
	require.Equal(t, 1, continuations.retired)
}

type closedTurns struct{}

func (closedTurns) StartTurn(context.Context, turn.StartTurnCommand) (turn.RunHandle, error) {
	return nil, fmt.Errorf("start: %w", sessionruntime.ErrManagerClosed)
}

type releasingBatch struct {
	oneBatch
	released int
}

func (r *releasingBatch) ReleaseBatch(context.Context, sessionruntime.QueueBatch) error {
	r.released++
	return nil
}

// A process that is stopping answers every pending admission with
// ErrManagerClosed and writes no warning: the next process takes the session
// over. A queued batch it had claimed goes back to the queue.
func TestAdmissionAfterTheRuntimeClosedReportsItWithoutAWarning(t *testing.T) {
	newInputs := func(logs *bytes.Buffer, d Deps) *Inputs {
		d.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
		d.Queries = noDiscuss{}
		return New(d)
	}
	t.Run("continuation", func(t *testing.T) {
		var logs bytes.Buffer
		inputs := newInputs(&logs, Deps{Continuations: busyContinuations{err: fmt.Errorf("start: %w", sessionruntime.ErrManagerClosed)}})
		_, err := inputs.admitPending(t.Context(), "bot-1", "session-1")
		require.ErrorIs(t, err, sessionruntime.ErrManagerClosed)
		require.NotContains(t, logs.String(), "level=WARN")
	})
	t.Run("queued follow-up", func(t *testing.T) {
		var logs bytes.Buffer
		cmd := turn.StartTurnCommand{
			SchemaVersion: turnwire.StartTurnVersion, TeamID: "team-1", Mode: turn.ModeChat,
			BotID: "bot-1", ChatID: "bot-1", ThreadID: "session-1", IdempotencyKey: "m1", Query: "hello",
		}
		payload, err := turnwire.NewQueuedInput(cmd, nil, time.Now()).Marshal()
		require.NoError(t, err)
		queue := &releasingBatch{oneBatch: oneBatch{batch: sessionruntime.QueueBatch{ID: "b", Items: []sessionruntime.QueuedInput{{
			ID: "i", BotID: "bot-1", SessionID: "session-1", InvocationID: "m1", Payload: payload,
		}}}}}
		inputs := newInputs(&logs, Deps{Continuations: noContinuations{}, Queue: queue, Turns: closedTurns{}, Signer: admit.NewSigner("s")})
		_, err = inputs.admitPending(t.Context(), "bot-1", "session-1")
		require.ErrorIs(t, err, sessionruntime.ErrManagerClosed)
		require.NotContains(t, logs.String(), "level=WARN")
		require.Equal(t, 1, queue.released, "the claimed batch goes back to the queue")
	})
}
