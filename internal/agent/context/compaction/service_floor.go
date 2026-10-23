package compaction

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// errNothingOmitted marks a floor whose turns hold no candidate by the time
// it claims them.
var errNothingOmitted = errors.New("compaction: no row left under the floor")

// placeFloor stands the session's history on a floor when no active summary
// stands on one and the candidates hold more than its requests could still
// read: a thread whose backlog predates rolling summaries. The turns before
// what its last request read, or before the history budget back from the
// newest turn when no request has been answered, are left out of the request
// without being summarized, and the summary the floor commits says so, so the
// passes after it fold only what a request could have rendered and what came
// after it. Every summary that folds the floor stands on it; a rewind to
// before it retires it, and the next event sets another one when the history
// before the rewind needs it. It reports false when the session needs no
// floor.
func (s *Service) placeFloor(ctx context.Context, botUUID, sessionUUID pgtype.UUID, cfg TriggerConfig) (Pass, bool, error) {
	frontier, err := NewArtifactProjection(s.queries).LoadActiveSession(ctx, ArtifactOwner{BotID: cfg.BotID, SessionID: cfg.SessionID, SessionIDKnown: true})
	if err != nil {
		return Pass{}, false, err
	}
	if slices.ContainsFunc(frontier.Artifacts, func(artifact Artifact) bool { return artifact.FloorMs > 0 }) {
		return Pass{}, false, nil
	}
	createdBefore := pgtype.Timestamptz{Time: cfg.CandidatesBefore, Valid: !cfg.CandidatesBefore.IsZero()}
	protectedIDs, protectedExternalIDs := sourceIDs(append(slices.Clone(cfg.ProtectedSources), cfg.Anchors...))
	floor, err := s.queries.GetCompactionFloorTurn(ctx, sqlc.GetCompactionFloorTurnParams{
		SessionID:                   sessionUUID,
		CreatedBefore:               createdBefore,
		HistoryBudgetTokens:         int64(cfg.HistoryBudgetTokens),
		BytesPerToken:               contextfrag.EstimateBytesPerToken,
		ProtectedMessageIds:         protectedIDs,
		ProtectedExternalMessageIds: protectedExternalIDs,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Pass{}, false, nil
	}
	if err != nil {
		return Pass{}, false, err
	}
	session, err := s.queries.GetSessionByID(ctx, sessionUUID)
	if err != nil {
		return Pass{}, false, err
	}
	var logID pgtype.UUID
	var omitted sqlc.MarkMessagesOmittedRow
	var artifact artifactMetadata
	fenced, err := s.inSession(ctx, cfg, func(q dbstore.Queries) error {
		logRow, err := q.CreateCompactionLog(ctx, sqlc.CreateCompactionLogParams{
			BotID:         botUUID,
			SessionID:     sessionUUID,
			ExpectedEpoch: session.CompactionEpoch,
		})
		if err != nil {
			return err
		}
		logID = logRow.ID
		omitted, err = q.MarkMessagesOmitted(ctx, sqlc.MarkMessagesOmittedParams{
			CompactID:     logID,
			TurnPosition:  floor.FloorTurnPosition,
			CreatedBefore: createdBefore,
		})
		if err != nil {
			return err
		}
		if omitted.OmittedCount == 0 {
			return errNothingOmitted
		}
		artifact = floorArtifact(omitted)
		return s.completeLog(ctx, q, logID, "ok", artifact.FloorNotice, "", int(omitted.OmittedCount), nil, pgtype.UUID{}, &artifact)
	})
	if err != nil {
		if logID.Valid && !fenced {
			_ = s.completeLog(context.WithoutCancel(ctx), s.queries, logID, "error", "", err.Error(), 0, nil, pgtype.UUID{}, nil)
		}
		if errors.Is(err, errNothingOmitted) {
			return Pass{}, false, nil
		}
		return Pass{}, false, staleSource(err)
	}
	s.logger.InfoContext(ctx, "compaction: left the history under a floor out",
		slog.String("session_id", cfg.SessionID),
		slog.Int64("omitted_messages", omitted.OmittedCount),
		slog.Int64("floor_ms", artifact.FloorMs),
		slog.Int64("kept_tokens", floor.KeptTokens),
	)
	return Pass{ArtifactID: formatUUID(logID), Summary: artifact.FloorNotice, More: true}, true, nil
}

// floorArtifact is what a floor commits: no coverage, anchors that span the
// rows it left out, and the notice that tells the model so.
func floorArtifact(omitted sqlc.MarkMessagesOmittedRow) artifactMetadata {
	first, last := omitted.FirstCreatedAt.Time, omitted.LastCreatedAt.Time
	return artifactMetadata{
		Coverage:      []byte("[]"),
		AnchorStartMs: first.UnixMilli(),
		AnchorEndMs:   last.UnixMilli(),
		FloorMs:       last.UnixMilli(),
		FloorNotice:   floorNotice(omitted.OmittedCount, first, last),
	}
}

// floorNotice tells the model what a floor left out of its context.
func floorNotice(count int64, first, last time.Time) string {
	const layout = "2006-01-02 15:04 UTC"
	from, to := first.UTC().Format(layout), last.UTC().Format(layout)
	span := "from " + from
	if to != from {
		span += " to " + to
	}
	if count == 1 {
		return fmt.Sprintf("1 earlier message of this conversation, %s, is left out of this context and was not summarized.", span)
	}
	return fmt.Sprintf("%d earlier messages of this conversation, %s, are left out of this context and were not summarized.", count, span)
}
