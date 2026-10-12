package application

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/schedule"
	"github.com/felinics/memoh/internal/settings"
)

type compactingLifecycleQueries struct {
	noDecisionQueries
}

func (q compactingLifecycleQueries) GetSettingsByBotID(ctx context.Context, botID pgtype.UUID) (sqlc.GetSettingsByBotIDRow, error) {
	row, err := q.directLifecycleQueries.GetSettingsByBotID(ctx, botID)
	row.CompactionEnabled = true
	row.CompactionThreshold = 1000
	return row, err
}

// newRunEndFixture is a native turn over history past the compaction
// threshold whose provider call stays open until the run is canceled.
func newRunEndFixture(t *testing.T) (directLifecycleFixture, *gatedCompactionRunner) {
	t.Helper()
	fixture := newDirectLifecycleFixture(t, directLifecycleModelBlock)
	queries := compactingLifecycleQueries{noDecisionQueries{fixture.service.queries.(*directLifecycleQueries)}}
	fixture.service.queries = queries
	fixture.service.settingsService = settings.NewService(slog.New(slog.DiscardHandler), queries, nil, nil)
	fixture.service.messageService = &memoryQueryMessageService{messages: historyAtTokens(t, 2000)}
	runner := newGatedCompactionRunner()
	t.Cleanup(func() { close(runner.release) })
	fixture.service.compactionService = runner
	return fixture, runner
}

// assertPassFollowsRun checks that the pass a run schedules waits while the
// run is still inside its provider call, then starts once the run ends.
func assertPassFollowsRun(t *testing.T, fixture directLifecycleFixture, runner *gatedCompactionRunner, cancel context.CancelFunc, ended <-chan struct{}) {
	t.Helper()
	select {
	case <-fixture.started:
	case <-time.After(3 * time.Second):
		t.Fatal("provider call did not start")
	}
	runner.assertNoPass(t)
	cancel()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled run did not end")
	}
	if cfg := runner.awaitPass(t); cfg.SessionID != lifecycleTestSessionID {
		t.Fatalf("background pass = %+v, want the run's session", cfg)
	}
}

func runEndRequest() ChatRequest {
	return ChatRequest{
		BotID:                lifecycleTestBotID,
		ChatID:               lifecycleTestBotID,
		ThreadID:             lifecycleTestSessionID,
		Query:                directLifecyclePrompt,
		UserMessagePersisted: true,
	}
}

func TestStreamChatCompactsAfterTheRunEnds(t *testing.T) {
	fixture, runner := newRunEndFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chunks, errs := fixture.service.StreamChat(ctx, runEndRequest())
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		for range chunks {
		}
		for range errs {
		}
	}()
	assertPassFollowsRun(t, fixture, runner, cancel, ended)
}

func TestWebSocketStreamCompactsAfterTheRunEnds(t *testing.T) {
	fixture, runner := newRunEndFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan WSStreamEvent, 64)
	consumed := make(chan struct{})
	go func() {
		defer close(consumed)
		for range events {
		}
	}()
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_, _ = fixture.service.StreamChatWS(ctx, runEndRequest(), events, nil)
		close(events)
		<-consumed
	}()
	assertPassFollowsRun(t, fixture, runner, cancel, ended)
}

func TestScheduledTriggerCompactsAfterTheRunEnds(t *testing.T) {
	fixture, runner := newRunEndFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_, _ = fixture.service.TriggerSchedule(ctx, lifecycleTestBotID, schedule.TriggerPayload{
			FireID:      "run-end-fire",
			SessionID:   lifecycleTestSessionID,
			Command:     directLifecyclePrompt,
			OwnerUserID: "user-1",
		}, "")
	}()
	assertPassFollowsRun(t, fixture, runner, cancel, ended)
}
