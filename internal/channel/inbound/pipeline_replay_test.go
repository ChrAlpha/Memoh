package inbound

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/chat/timeline"
	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

const replayTestSessionID = "66666666-6666-6666-6666-666666666666"

// replayQueries serves one persisted message to a replay and holds every
// replay at its first query until released, counting how many started.
type replayQueries struct {
	dbstore.Queries
	row     sqlc.ListSessionEventsBySessionPageBeforeWithinBytesRow
	started atomic.Int32
	release chan struct{}
}

func (q *replayQueries) GetActiveHistoryCheckpoint(context.Context, pgtype.UUID) (sqlc.BotHistoryCheckpoint, error) {
	q.started.Add(1)
	<-q.release
	return sqlc.BotHistoryCheckpoint{}, pgx.ErrNoRows
}

func (q *replayQueries) ListSessionEventsBySessionPageBeforeWithinBytes(context.Context, sqlc.ListSessionEventsBySessionPageBeforeWithinBytesParams) ([]sqlc.ListSessionEventsBySessionPageBeforeWithinBytesRow, error) {
	return []sqlc.ListSessionEventsBySessionPageBeforeWithinBytesRow{q.row}, nil
}

func (*replayQueries) CountSessionEvents(context.Context, pgtype.UUID) (int64, error) {
	return 1, nil
}

func newReplayQueries(t *testing.T) *replayQueries {
	t.Helper()
	data, err := json.Marshal(timeline.MessageEvent{
		SessionID: replayTestSessionID, MessageID: "m1", ReceivedAtMs: 1,
		Content: []timeline.ContentNode{{Type: "text", Text: "persisted"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := dbpkg.ParseUUID("55555555-5555-5555-5555-555555555555")
	if err != nil {
		t.Fatal(err)
	}
	return &replayQueries{
		row: sqlc.ListSessionEventsBySessionPageBeforeWithinBytesRow{
			ID: id, EventKind: string(timeline.EventMessage), EventData: data,
			ReceivedAtMs: 1, PayloadBytes: int64(len(data)), CumulativeBytes: int64(len(data)),
		},
		release: make(chan struct{}),
	}
}

// Messages that reach a cold session together load its history once: the
// ones that arrive while the replay runs wait for it, and the ones that
// arrive after it finished find the session loaded.
func TestReplayPipelineSessionLoadsAColdSessionOnce(t *testing.T) {
	t.Parallel()
	queries := newReplayQueries(t)
	pipeline := timeline.NewPipeline(timeline.RenderParams{})
	processor := &ChannelInboundProcessor{logger: slog.New(slog.DiscardHandler)}
	processor.SetPipeline(pipeline, timeline.NewEventStore(slog.New(slog.DiscardHandler), queries), nil)

	const callers = 8
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			processor.replayPipelineSession(context.Background(), "bot-1", replayTestSessionID)
		})
	}
	close(queries.release)
	wg.Wait()
	processor.replayPipelineSession(context.Background(), "bot-1", replayTestSessionID)

	if got := queries.started.Load(); got != 1 {
		t.Fatalf("replays started = %d, want 1", got)
	}
	if !pipeline.HasSession(replayTestSessionID) {
		t.Fatal("session not loaded after replay")
	}
}

// A replay that starts after the session is loaded would replace it with
// the persisted events and drop what was pushed since; it does not start.
func TestReplayPipelineSessionKeepsALoadedSession(t *testing.T) {
	t.Parallel()
	queries := newReplayQueries(t)
	close(queries.release)
	pipeline := timeline.NewPipeline(timeline.RenderParams{})
	processor := &ChannelInboundProcessor{logger: slog.New(slog.DiscardHandler)}
	processor.SetPipeline(pipeline, timeline.NewEventStore(slog.New(slog.DiscardHandler), queries), nil)

	pipeline.PushEvent(replayTestSessionID, timeline.MessageEvent{
		SessionID: replayTestSessionID, MessageID: "m2", ReceivedAtMs: 2,
		Content: []timeline.ContentNode{{Type: "text", Text: "pushed"}},
	})
	processor.replayPipelineSession(context.Background(), "bot-1", replayTestSessionID)

	if got := queries.started.Load(); got != 0 {
		t.Fatalf("replays started = %d, want 0", got)
	}
	if rc := pipeline.GetRC(replayTestSessionID); len(rc) != 1 || rc[0].MessageID != "m2" {
		t.Fatalf("session context = %+v, want only the pushed message", rc)
	}
}
