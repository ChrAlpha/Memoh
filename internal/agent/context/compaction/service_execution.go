package compaction

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/models/modelretry"
)

func (s *Service) doCompaction(ctx context.Context, botUUID pgtype.UUID, sessionUUID pgtype.UUID, cfg TriggerConfig) (Result, error) {
	if cfg.HistoryBudgetTokens <= 0 || cfg.SummaryWindowTokens <= 0 {
		return Result{}, fmt.Errorf("%w: history_budget=%d window=%d", errCompactionUnsized, cfg.HistoryBudgetTokens, cfg.SummaryWindowTokens)
	}
	// The summarizer may write as much as the model declares it can answer;
	// a model that declares nothing gets the output its runs get.
	capability := cfg.MaxOutputTokens
	if capability <= 0 {
		capability = models.ResolveGenerationLimits(models.ClientType(cfg.ClientType), nil, cfg.SummaryWindowTokens, 0).MaxOutputTokens
	}
	promptWindow := cfg.SummaryWindowTokens - estimateBytesAsTokens(systemPrompt) - compactionPromptFramingTokens
	if promptWindow <= 0 {
		return Result{}, fmt.Errorf("%w: window=%d", ErrSummaryWindowTooSmall, cfg.SummaryWindowTokens)
	}

	frontier, err := NewArtifactProjection(s.queries).LoadActiveSession(ctx, ArtifactOwner{BotID: cfg.BotID, SessionID: cfg.SessionID, SessionIDKnown: true})
	if err != nil {
		return Result{}, err
	}
	for _, issue := range frontier.Issues {
		s.logger.WarnContext(ctx, "compaction: ignored invalid artifact lineage", slog.String("issue", issue.Error()))
	}
	parents := frontier.Artifacts
	createdFrom := pgtype.Timestamptz{Time: cfg.CandidatesFrom, Valid: !cfg.CandidatesFrom.IsZero()}
	createdBefore := pgtype.Timestamptz{Time: cfg.CandidatesBefore, Valid: !cfg.CandidatesBefore.IsZero()}
	measure, err := s.queries.MeasureUncompactedMessagesBySession(ctx, sqlc.MeasureUncompactedMessagesBySessionParams{
		SessionID:     sessionUUID,
		CreatedFrom:   createdFrom,
		CreatedBefore: createdBefore,
	})
	if err != nil {
		return Result{}, err
	}
	if measure.CandidateCount == 0 && len(parents) == 0 {
		return Result{Status: StatusNoop}, nil
	}
	readMaxBytes := compactionReadMaxBytes(promptWindow)
	boundedRows, err := s.queries.ListUncompactedMessagesBySessionWithinBytes(ctx, sqlc.ListUncompactedMessagesBySessionWithinBytesParams{
		SessionID:     sessionUUID,
		CreatedFrom:   createdFrom,
		CreatedBefore: createdBefore,
		MaxBytes:      readMaxBytes,
	})
	if err != nil {
		return Result{}, err
	}
	rows := uncompactedRowsFromBounded(boundedRows)
	// Use the count captured by the same SQL statement as the payload rows:
	// concurrent claims may change eligibility between the two statements.
	truncatedRead := false
	if len(boundedRows) > 0 {
		truncatedRead = boundedRows[0].CandidateCount > int64(len(rows))
		s.logger.InfoContext(ctx, "compaction: bounded candidate read",
			slog.String("session_id", cfg.SessionID),
			slog.Int("loaded_messages", len(rows)),
			slog.Int64("candidate_count", boundedRows[0].CandidateCount),
			slog.Int64("candidate_bytes", boundedRows[0].CandidateBytes),
			slog.Int64("loaded_bytes", boundedRows[len(boundedRows)-1].CumulativeBytes),
			slog.Int64("read_max_bytes", readMaxBytes),
			slog.Bool("truncated", truncatedRead))
	} else if measure.CandidateCount > 0 {
		return Result{}, fmt.Errorf("%w: largest_candidate_bytes=%d read_max_bytes=%d", errCompactionInputOverflow, measure.LargestCandidateBytes, readMaxBytes)
	}

	messages, barrierCount := candidatesFromRows(rows)
	protectCurrentSources(messages, cfg.ProtectedSources)
	if barrierCount > 0 {
		s.logger.WarnContext(ctx, "compaction: kept unparseable history rows as span barriers",
			slog.Int("barrier_count", barrierCount),
			slog.String("session_id", cfg.SessionID),
		)
	}
	var backfills []sqlc.BackfillCompactionArtifactCoverageParams
	if len(parents) > 0 {
		if parents, backfills, err = s.backfillLegacyCoverage(ctx, parents); err != nil {
			return Result{}, err
		}
	}
	if !cfg.CandidatesBefore.IsZero() {
		for _, parent := range parents {
			if parent.AnchorEndMs <= 0 || parent.AnchorEndMs >= cfg.CandidatesBefore.UnixMilli() {
				return Result{}, fmt.Errorf("%w: artifact_id=%s", errSummaryPastCutoff, parent.ID)
			}
		}
	}
	earlier := make([]string, len(parents))
	earlierTokens := 0
	for i, parent := range parents {
		earlier[i] = parent.Summary
		earlierTokens += estimateSummaryReplayTokens(parent.Summary)
	}

	// The summary replaces the earlier summaries and the rows it folds; what
	// the request keeps beside it bounds how long it may be.
	room := summaryRoom{HistoryTokens: cfg.HistoryBudgetTokens, KeptTokens: keptReplayTokens(messages, cfg.ProtectedSources)}
	summaryLimit := summaryOutputLimit(room.HistoryTokens-room.KeptTokens, capability)
	if summaryLimit <= 0 {
		return Result{}, fmt.Errorf("%w: history_budget=%d kept=%d", errNoSummaryRoom, room.HistoryTokens, room.KeptTokens)
	}
	current := currentRequestEntries(messages, cfg.ProtectedSources, cfg.CurrentRequest)
	readWindow := promptWindow - entriesPromptCost(current)
	if readWindow <= 0 {
		return Result{}, fmt.Errorf("%w: window=%d current_request_tokens=%d", ErrSummaryWindowTooSmall, cfg.SummaryWindowTokens, entriesPromptCost(current))
	}
	foldable := foldableItems(messages)
	earlierCost := earlierSummariesCost(earlier)
	// The window holds what a pass reads and the summary written from it. A
	// summary is shorter than what it replaces, so reading r tokens may need
	// min(summaryLimit, r) beside it, and later groups are read only while that
	// fits. No pass progresses without the earlier summaries and the first
	// group, so they need only some room left beside them.
	required := earlierCost + leadingGroupCost(foldable)
	inputTokens := readWindow - max(min(summaryLimit, readWindow/2, readWindow-required), 1)

	toCompact := trimCompactMessages(foldable, inputTokens-earlierCost)
	entries, compactedMessageIDs := buildEntriesAndIDs(toCompact)
	if len(compactedMessageIDs) == 0 {
		if _, pending := buildEntriesAndIDs(foldable); len(pending) > 0 {
			return Result{}, fmt.Errorf("%w: earlier_summary_tokens=%d input_tokens=%d", errCompactionInputOverflow, earlierTokens, inputTokens)
		}
		// Nothing new to fold: the summaries are refolded only when they are
		// themselves what no longer fits.
		if len(parents) == 0 || earlierTokens <= summaryLimit {
			return Result{Status: StatusNoop}, nil
		}
	}
	readTokens := entriesPromptCost(entries) + earlierCost
	if readTokens > inputTokens {
		return Result{}, fmt.Errorf("%w: entries=%d entry_tokens=%d input_tokens=%d", errCompactionInputOverflow, len(entries), readTokens, inputTokens)
	}
	_, remaining := buildEntriesAndIDs(withoutCompacted(foldable, compactedMessageIDs))
	// A summary must be shorter than what it replaces, so the bound the
	// prompt states never exceeds that.
	replacedTokens := max(entriesPromptCost(entries), compactedReplayTokens(messages, compactedMessageIDs)) + earlierTokens
	maxTokens := min(capability, readWindow-readTokens)
	room.SummaryTokens = min(summaryLimit, replacedTokens-1, maxTokens)
	userPrompt := buildUserPrompt(earlier, entries, current, room)
	if request := contextfrag.ProviderEnvelopeTokens(systemPrompt, []sdk.Message{sdk.UserMessage(userPrompt)}, nil); request+maxTokens > cfg.SummaryWindowTokens {
		return Result{}, fmt.Errorf("%w: request_tokens=%d max_tokens=%d window=%d", errCompactionInputOverflow, request, maxTokens, cfg.SummaryWindowTokens)
	}

	expectedCompactIDs, err := expectedCompactionClaims(rows, compactedMessageIDs)
	if err != nil {
		return Result{}, err
	}
	// Claim the exact selected row versions before loading assets. Asset upserts
	// lock the same message row, so either their mutation is visible below or
	// they invalidate this attempt's epoch before it can complete.
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	var epoch int64
	if len(rows) > 0 {
		epoch = rows[0].CompactionEpoch
	} else {
		session, err := s.queries.GetSessionByID(ctx, sessionUUID)
		if err != nil {
			return Result{}, err
		}
		epoch = session.CompactionEpoch
	}
	persistCtx := context.WithoutCancel(ctx)
	logRow, err := s.queries.CreateCompactionLog(persistCtx, sqlc.CreateCompactionLogParams{
		BotID:         botUUID,
		SessionID:     sessionUUID,
		ExpectedEpoch: epoch,
	})
	if err != nil {
		return Result{}, err
	}
	logID := logRow.ID
	var assetRows []sqlc.ListMessageAssetsBatchRow
	if len(compactedMessageIDs) > 0 {
		marked, err := s.queries.MarkMessagesCompacted(persistCtx, sqlc.MarkMessagesCompactedParams{
			CompactID:          logID,
			MessageIds:         compactedMessageIDs,
			ExpectedCompactIds: expectedCompactIDs,
		})
		if err != nil {
			_ = s.completeLog(persistCtx, logID, "error", "", err.Error(), 0, nil, pgtype.UUID{}, nil)
			return Result{}, err
		}
		if marked != int64(len(compactedMessageIDs)) {
			err = fmt.Errorf("marked %d of %d compaction source rows", marked, len(compactedMessageIDs))
			_ = s.completeLog(persistCtx, logID, "error", "", err.Error(), 0, nil, pgtype.UUID{}, nil)
			return Result{}, err
		}

		assetRows, err = s.queries.ListMessageAssetsBatch(persistCtx, compactedMessageIDs)
		if err != nil {
			err = fmt.Errorf("load compaction message assets: %w", err)
			_ = s.completeLog(persistCtx, logID, "error", "", err.Error(), 0, nil, pgtype.UUID{}, nil)
			return Result{}, err
		}
	}

	toCompact, err = candidatesWithAssets(toCompact, rows, assetRows)
	if err != nil {
		_ = s.completeLog(persistCtx, logID, "error", "", err.Error(), 0, nil, pgtype.UUID{}, nil)
		return Result{}, err
	}
	artifact, err := artifactMetadataFor(toCompact, compactedMessageIDs)
	if err != nil {
		_ = s.completeLog(persistCtx, logID, "error", "", err.Error(), 0, nil, pgtype.UUID{}, nil)
		return Result{}, err
	}
	if len(parents) > 0 {
		artifact, err = rollupArtifactMetadata(parents, artifact)
		if err != nil {
			_ = s.completeLog(persistCtx, logID, "error", "", err.Error(), 0, nil, pgtype.UUID{}, nil)
			return Result{}, err
		}
	}

	model := models.NewSDKChatModel(models.SDKModelConfig{
		ClientType:            cfg.ClientType,
		BaseURL:               cfg.BaseURL,
		APIKey:                cfg.APIKey,
		CodexAccountID:        cfg.CodexAccountID,
		ModelID:               cfg.ModelID,
		ChatCompletionsCompat: cfg.ChatCompletionsCompat,
		HTTPClient:            cfg.HTTPClient,
	})

	systemPromptDecorated, sdkMessages, _ := models.ApplyPromptCache(
		model, cfg.PromptCacheTTL,
		systemPrompt, []sdk.Message{sdk.UserMessage(userPrompt)}, nil,
	)

	request := sdk.Request{
		System:    systemPromptDecorated,
		Messages:  sdkMessages,
		MaxTokens: &maxTokens,
	}
	result, err := modelretry.Do(models.WithModelSession(ctx, cfg.SessionID), s.logger, "agent.compaction", modelretry.Config{}, modelretry.Retryable,
		func(ctx context.Context) (sdk.ModelResult, error) { return model.Generate(ctx, request) })
	if err != nil {
		_ = s.completeLog(persistCtx, logID, "error", "", err.Error(), 0, nil, pgtype.UUID{}, nil)
		return Result{}, err
	}

	summary := strings.TrimSpace(result.Text)
	if summary == "" {
		_ = s.completeLog(persistCtx, logID, "error", "", errEmptySummary.Error(), 0, nil, pgtype.UUID{}, nil)
		return Result{}, errEmptySummary
	}
	if result.FinishReason != sdk.FinishReasonStop {
		err = fmt.Errorf("%w: finish_reason=%s", errIncompleteSummary, result.FinishReason)
		_ = s.completeLog(persistCtx, logID, "error", "", err.Error(), 0, nil, pgtype.UUID{}, nil)
		return Result{}, err
	}
	if summaryTokens := estimateSummaryReplayTokens(summary); summaryTokens >= replacedTokens {
		err = fmt.Errorf("%w: summary_tokens=%d replaced_tokens=%d", errIneffectiveSummary, summaryTokens, replacedTokens)
		_ = s.completeLog(persistCtx, logID, "error", "", err.Error(), 0, nil, pgtype.UUID{}, nil)
		return Result{}, err
	}
	if replay := summaryProviderReplayTokens(summary); replay > room.HistoryTokens-room.KeptTokens {
		err = fmt.Errorf("%w: replay=%d room=%d", errNoSummaryRoom, replay, room.HistoryTokens-room.KeptTokens)
		_ = s.completeLog(persistCtx, logID, "error", "", err.Error(), 0, nil, pgtype.UUID{}, nil)
		return Result{}, err
	}

	usageJSON, _ := json.Marshal(result.Usage)

	modelUUID := db.ParseUUIDOrEmpty(cfg.ModelRecordID)
	switch err = s.persistBackfills(persistCtx, backfills); {
	case err != nil:
	case len(parents) > 0:
		err = s.completeRollupLog(persistCtx, logID, summary, len(compactedMessageIDs), usageJSON, modelUUID, artifact, parents)
	default:
		err = s.completeLog(persistCtx, logID, "ok", summary, "", len(compactedMessageIDs), usageJSON, modelUUID, &artifact)
	}
	if err != nil {
		// The rows are already marked, but the log never reached status=ok, so
		// the reclaim SQL keeps them eligible for a later pass. Reporting ok
		// here would claim a summary that was never persisted.
		_ = s.completeLog(persistCtx, logID, "error", "", err.Error(), 0, nil, pgtype.UUID{}, nil)
		return Result{}, err
	}
	status := StatusOK
	if truncatedRead || len(remaining) > 0 {
		status = StatusProgress
	}
	return Result{Status: status, Summary: summary, MessageCount: len(compactedMessageIDs)}, nil
}

func (s *Service) loadAbsorbedRows(ctx context.Context, artifact Artifact) ([]sqlc.ListUncompactedMessagesBySessionRow, error) {
	compactID, err := db.ParseUUID(artifact.ID)
	if err != nil {
		return nil, err
	}
	rows, err := s.queries.ListMessagesByCompactID(ctx, compactID)
	if err != nil {
		return nil, err
	}
	converted := make([]sqlc.ListUncompactedMessagesBySessionRow, len(rows))
	for i, row := range rows {
		converted[i] = sqlc.ListUncompactedMessagesBySessionRow(row)
	}
	return converted, nil
}

// backfillLegacyCoverage reads the coverage of legacy summaries back from
// their rows. The caller persists it only once a summary that supersedes them
// is ready to commit.
func (s *Service) backfillLegacyCoverage(ctx context.Context, parents []Artifact) ([]Artifact, []sqlc.BackfillCompactionArtifactCoverageParams, error) {
	filled := append([]Artifact(nil), parents...)
	var backfills []sqlc.BackfillCompactionArtifactCoverageParams
	for i := range filled {
		if len(filled[i].Coverage) > 0 {
			continue
		}
		rows, err := s.loadAbsorbedRows(ctx, filled[i])
		if err != nil {
			return nil, nil, err
		}
		items, _ := candidatesFromRows(rows)
		if len(items) == 0 {
			return nil, nil, fmt.Errorf("%w: artifact_id=%s", errIncompleteAbsorbedCoverage, filled[i].ID)
		}
		ids := make([]pgtype.UUID, 0, len(items))
		for _, item := range items {
			ids = append(ids, item.ID)
		}
		metadata, err := artifactMetadataFor(items, ids)
		if err != nil {
			return nil, nil, err
		}
		if filled[i].Coverage, err = DecodeArtifactCoverage(metadata.Coverage); err != nil {
			return nil, nil, fmt.Errorf("backfill coverage of artifact %s: %w", filled[i].ID, err)
		}
		id, err := db.ParseUUID(filled[i].ID)
		if err != nil {
			return nil, nil, err
		}
		backfills = append(backfills, sqlc.BackfillCompactionArtifactCoverageParams{
			ID:            id,
			Coverage:      metadata.Coverage,
			AnchorStartMs: metadata.AnchorStartMs,
			AnchorEndMs:   metadata.AnchorEndMs,
		})
		filled[i].AnchorStartMs, filled[i].AnchorEndMs = metadata.AnchorStartMs, metadata.AnchorEndMs
	}
	return filled, backfills, nil
}

func (s *Service) persistBackfills(ctx context.Context, backfills []sqlc.BackfillCompactionArtifactCoverageParams) error {
	for _, backfill := range backfills {
		if err := s.queries.BackfillCompactionArtifactCoverage(ctx, backfill); err != nil {
			return err
		}
	}
	return nil
}

func expectedCompactionClaims(rows []sqlc.ListUncompactedMessagesBySessionRow, messageIDs []pgtype.UUID) ([]pgtype.UUID, error) {
	byID := make(map[pgtype.UUID]pgtype.UUID, len(rows))
	for _, row := range rows {
		byID[row.ID] = row.CompactID
	}
	expected := make([]pgtype.UUID, 0, len(messageIDs))
	for _, id := range messageIDs {
		claim, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("compaction source %s missing from selected rows", formatUUID(id))
		}
		expected = append(expected, claim)
	}
	return expected, nil
}

func (s *Service) completeLog(ctx context.Context, logID pgtype.UUID, status, summary, errMsg string, messageCount int, usage []byte, modelID pgtype.UUID, artifact *artifactMetadata) error {
	coverage := []byte("[]")
	var anchorStartMs, anchorEndMs int64
	if artifact != nil {
		coverage = artifact.Coverage
		anchorStartMs = artifact.AnchorStartMs
		anchorEndMs = artifact.AnchorEndMs
	}
	_, err := s.queries.CompleteCompactionLog(ctx, sqlc.CompleteCompactionLogParams{
		ID:            logID,
		Status:        status,
		Summary:       summary,
		MessageCount:  int32(messageCount), //nolint:gosec // count always small
		ErrorMessage:  errMsg,
		Usage:         usage,
		ModelID:       modelID,
		Coverage:      coverage,
		AnchorStartMs: anchorStartMs,
		AnchorEndMs:   anchorEndMs,
	})
	if err != nil {
		s.logger.ErrorContext(ctx, "failed to complete compaction log", slog.String("error", err.Error()))
		return err
	}
	return nil
}

func (s *Service) completeRollupLog(
	ctx context.Context,
	logID pgtype.UUID,
	summary string,
	messageCount int,
	usage []byte,
	modelID pgtype.UUID,
	artifact artifactMetadata,
	parents []Artifact,
) error {
	parentIDs := make([]pgtype.UUID, 0, len(parents))
	for _, parent := range parents {
		parentID, err := db.ParseUUID(parent.ID)
		if err != nil {
			return err
		}
		parentIDs = append(parentIDs, parentID)
	}
	_, err := s.queries.CompleteCompactionRollup(ctx, sqlc.CompleteCompactionRollupParams{
		ID:            logID,
		Status:        "ok",
		Summary:       summary,
		MessageCount:  int32(messageCount), //nolint:gosec // count always small
		Usage:         usage,
		ModelID:       modelID,
		Coverage:      artifact.Coverage,
		AnchorStartMs: artifact.AnchorStartMs,
		AnchorEndMs:   artifact.AnchorEndMs,
		Level:         int32(rollupArtifactLevel(parents)), //nolint:gosec // lineage levels are bounded by pass count
		Parents:       parentIDs,
	})
	if err != nil {
		s.logger.ErrorContext(ctx, "failed to complete compaction rollup", slog.String("error", err.Error()))
		return err
	}
	return nil
}

// estimateSummaryReplayTokens meters the summary with the same byte-based
// estimator the selection path uses, so the ineffective-summary check
// compares like units.
func estimateSummaryReplayTokens(summary string) int {
	return estimateBytesAsTokens("<summary>\n" + strings.TrimSpace(summary) + "\n</summary>")
}
