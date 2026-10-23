package application

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

type horizonRecordingMessageService struct {
	recordingMessageService
	sessionSince  []time.Time
	responseSince []time.Time
	botSince      []time.Time
}

func (s *horizonRecordingMessageService) ListActiveSinceBySessionWithinBytes(_ context.Context, _ string, since time.Time, _ int64, _ bool) ([]messagepkg.Message, error) {
	s.sessionSince = append(s.sessionSince, since)
	return nil, nil
}

func (s *horizonRecordingMessageService) ListTurnResponseSourcesSinceBySessionWithinBytes(_ context.Context, _ string, since time.Time, _ int64) ([]messagepkg.Message, error) {
	s.responseSince = append(s.responseSince, since)
	return nil, nil
}

func (s *horizonRecordingMessageService) ListActiveSinceWithinBytes(_ context.Context, _ string, since time.Time, _ int64) ([]messagepkg.Message, error) {
	s.botSince = append(s.botSince, since)
	return nil, nil
}

type unsummarizedFromQueries struct {
	dbstore.Queries
	from     pgtype.Timestamptz
	err      error
	horizons []time.Time
}

func (q *unsummarizedFromQueries) GetSessionUnsummarizedFrom(_ context.Context, arg sqlc.GetSessionUnsummarizedFromParams) (pgtype.Timestamptz, error) {
	q.horizons = append(q.horizons, arg.Horizon.Time)
	return q.from, q.err
}

func horizonSessionService(horizon time.Time) *fakeBackgroundSessionService {
	return &fakeBackgroundSessionService{getFn: func(_ context.Context, sessionID string) (session.Thread, error) {
		return session.Thread{ID: sessionID, HistoryHorizonAt: horizon}, nil
	}}
}

func TestThreadHistoryLoadsBeginAtTheHorizonNotAWindow(t *testing.T) {
	t.Parallel()

	month := time.Now().UTC().Add(-30 * 24 * time.Hour)
	for _, tc := range []struct {
		name    string
		horizon time.Time
	}{
		{"a thread with no horizon loads the whole thread", time.Time{}},
		{"a thread loads from its horizon however old", month},
	} {
		messages := &horizonRecordingMessageService{}
		svc := &Service{messageService: messages, sessionService: horizonSessionService(tc.horizon), logger: slog.New(slog.DiscardHandler)}

		if _, err := svc.loadHistoryRecords(t.Context(), historyfrag.ScopeFallback{}, "session-1", 1000, true, false); err != nil {
			t.Fatalf("%s: history: %v", tc.name, err)
		}
		if _, err := svc.loadTurnResponses(t.Context(), "session-1", 1000); err != nil {
			t.Fatalf("%s: turn responses: %v", tc.name, err)
		}
		if len(messages.sessionSince) != 1 || !messages.sessionSince[0].Equal(tc.horizon) {
			t.Fatalf("%s: history since=%v, want %v", tc.name, messages.sessionSince, tc.horizon)
		}
		if len(messages.responseSince) != 1 || !messages.responseSince[0].Equal(tc.horizon) {
			t.Fatalf("%s: turn responses since=%v, want %v", tc.name, messages.responseSince, tc.horizon)
		}
	}
}

func TestSessionlessHistoryKeepsItsWindow(t *testing.T) {
	t.Parallel()

	messages := &horizonRecordingMessageService{}
	svc := &Service{messageService: messages, logger: slog.New(slog.DiscardHandler)}
	before := time.Now().UTC()
	if _, err := svc.loadHistoryRecords(t.Context(), historyfrag.ScopeFallback{ChatID: "bot-1"}, "", 1000, false, false); err != nil {
		t.Fatal(err)
	}
	if len(messages.botSince) != 1 || messages.botSince[0].Before(before.Add(-sessionlessHistoryWindow)) || messages.botSince[0].After(time.Now().UTC().Add(-sessionlessHistoryWindow)) {
		t.Fatalf("bot-scoped since=%v, want %v before now", messages.botSince, sessionlessHistoryWindow)
	}
	if len(messages.sessionSince) != 0 {
		t.Fatalf("a sessionless request read thread history: %v", messages.sessionSince)
	}
}

func TestThreadHistoryFailsWhenTheHorizonDoesNotLoad(t *testing.T) {
	t.Parallel()

	messages := &horizonRecordingMessageService{}
	svc := &Service{
		messageService: messages,
		sessionService: &fakeBackgroundSessionService{getFn: func(context.Context, string) (session.Thread, error) {
			return session.Thread{}, errors.New("session store unavailable")
		}},
		logger: slog.New(slog.DiscardHandler),
	}
	if _, err := svc.loadHistoryRecords(t.Context(), historyfrag.ScopeFallback{}, "session-1", 1000, true, false); err == nil {
		t.Fatal("history loaded without its horizon")
	}
	if _, err := svc.loadTurnResponses(t.Context(), "session-1", 1000); err == nil {
		t.Fatal("turn responses loaded without their horizon")
	}
	if len(messages.sessionSince) != 0 || len(messages.responseSince) != 0 {
		t.Fatalf("loads ran unbounded: history=%v responses=%v", messages.sessionSince, messages.responseSince)
	}
}

func TestThreadHistoryLoadsStartAtTheOldestUnsummarizedRow(t *testing.T) {
	t.Parallel()

	const sessionID = "00000000-0000-0000-0000-0000000d0001"
	horizon := time.Now().UTC().Add(-30 * 24 * time.Hour)
	unsummarized := time.Now().UTC().Add(-2 * time.Hour)
	queries := &unsummarizedFromQueries{from: pgtype.Timestamptz{Time: unsummarized, Valid: true}}
	messages := &horizonRecordingMessageService{}
	svc := &Service{messageService: messages, sessionService: horizonSessionService(horizon), queries: queries, logger: slog.New(slog.DiscardHandler)}

	if _, err := svc.loadHistoryRecords(t.Context(), historyfrag.ScopeFallback{}, sessionID, 1000, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.loadTurnResponses(t.Context(), sessionID, 1000); err != nil {
		t.Fatal(err)
	}
	if len(queries.horizons) != 2 || !queries.horizons[0].Equal(horizon) {
		t.Fatalf("unsummarized start asked from %v, want the horizon %v", queries.horizons, horizon)
	}
	if len(messages.sessionSince) != 1 || !messages.sessionSince[0].Equal(unsummarized) || len(messages.responseSince) != 1 || !messages.responseSince[0].Equal(unsummarized) {
		t.Fatalf("history since=%v responses since=%v, want both at the oldest unsummarized row %v", messages.sessionSince, messages.responseSince, unsummarized)
	}

	// A cutoff can split a summary, whose rows before it must then render
	// raw: the load reads from the horizon.
	if _, err := svc.loadHistoryRecords(t.Context(), historyfrag.ScopeFallback{}, sessionID, 1000, false, true); err != nil {
		t.Fatal(err)
	}
	if len(queries.horizons) != 2 || !messages.sessionSince[1].Equal(horizon) {
		t.Fatalf("cut history since=%v asks=%d, want the horizon without asking", messages.sessionSince[1], len(queries.horizons))
	}

	// A thread without rows has no start to report.
	queries.from = pgtype.Timestamptz{}
	if _, err := svc.loadHistoryRecords(t.Context(), historyfrag.ScopeFallback{}, sessionID, 1000, true, false); err != nil {
		t.Fatal(err)
	}
	if !messages.sessionSince[2].Equal(horizon) {
		t.Fatalf("empty thread since=%v, want the horizon", messages.sessionSince[2])
	}

	queries.err = errors.New("database unavailable")
	if _, err := svc.loadHistoryRecords(t.Context(), historyfrag.ScopeFallback{}, sessionID, 1000, true, false); err == nil || len(messages.sessionSince) != 3 {
		t.Fatalf("err=%v loads=%d, want the load to fail rather than read the whole thread", err, len(messages.sessionSince))
	}
}
