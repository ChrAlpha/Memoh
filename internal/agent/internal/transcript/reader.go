// Package transcript reads a conversation back from the database for the
// model: it assembles the stored rows into context records, swaps compacted
// spans for their summaries, applies the token window and admission filters,
// and finds the rows a continuation or a forked subagent starts from.
package transcript

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// Messages reads the stored message rows.
type Messages interface {
	ListRecentUserBySession(ctx context.Context, sessionID string, limit int) ([]messagepkg.Message, error)
	ListActiveAfterCompactionBySessionWithinBytes(ctx context.Context, sessionID string, maxBytes int64) (messagepkg.SessionHistoryAfterCompaction, error)
	ListVisibleFromForModelBySession(ctx context.Context, sessionID string, messageID string) ([]messagepkg.Message, error)
	GetByIDBySession(ctx context.Context, sessionID string, messageID string) (messagepkg.Message, error)
	// GetRunTurnRequest finds the request row a run answers, or reports
	// messagepkg.ErrRunRequestNotFound.
	GetRunTurnRequest(ctx context.Context, sessionID, runID, rootTurnID string) (messagepkg.RunTurnRequest, error)
}

// Queries is the part of the database the compaction artifacts are read from.
type Queries interface {
	GetCompactionLogByID(context.Context, pgtype.UUID) (sqlc.BotHistoryMessageCompact, error)
	ListCompactionArtifactLineageBySession(context.Context, pgtype.UUID) ([]sqlc.BotHistoryMessageCompact, error)
	ListCompactionArtifactParentIDsBySuccessor(context.Context, sqlc.ListCompactionArtifactParentIDsBySuccessorParams) ([]pgtype.UUID, error)
	ListMessageRefsByCompactID(context.Context, pgtype.UUID) ([]sqlc.ListMessageRefsByCompactIDRow, error)
}

// Sessions reads session metadata.
type Sessions interface {
	Get(ctx context.Context, sessionID string) (session.Thread, error)
}

// Deps is everything New needs.
type Deps struct {
	// Logger receives the component's own records.
	Logger *slog.Logger
	// Messages reads the stored rows.
	Messages Messages
	// Queries reads compaction artifacts and the rows they cover.
	Queries Queries
	// Sessions reads session metadata.
	Sessions Sessions
	// FallbackTokens is the context budget for models without a configured
	// window; zero keeps the shared default.
	FallbackTokens int
}

// Reader assembles the model's view of a stored conversation.
type Reader struct {
	logger         *slog.Logger
	messages       Messages
	queries        Queries
	sessions       Sessions
	fallbackTokens int
}

// New returns a Reader.
func New(d Deps) *Reader {
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Reader{
		logger:         logger,
		messages:       d.Messages,
		queries:        d.Queries,
		sessions:       d.Sessions,
		fallbackTokens: d.FallbackTokens,
	}
}
