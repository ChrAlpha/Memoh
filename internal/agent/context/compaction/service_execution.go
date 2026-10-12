package compaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/models/modelretry"
)

func (s *Service) doCompaction(ctx context.Context, botUUID pgtype.UUID, sessionUUID pgtype.UUID, cfg TriggerConfig) (res Result, err error) {
	protectedIDs, externalIDs := protectedSourceIDs(cfg.ProtectedSources)
	measure, err := s.queries.MeasureUncompactedMessagesBySession(ctx, sqlc.MeasureUncompactedMessagesBySessionParams{SessionID: sessionUUID, ProtectedMessageIds: protectedIDs, ProtectedExternalIds: externalIDs})
	if err != nil {
		return Result{}, err
	}
	if measure.CandidateCount == 0 && !cfg.AllowFrontierFusion {
		return Result{Status: StatusNoop}, nil
	}
	// Cap the compaction input to avoid exceeding the compaction model's
	// context window. NewTriggerConfig sets MaxCompactTokens to 85% of the
	// model's window. If not set, use a conservative default of 30K tokens. Prior
	// summaries and message entries share this one budget — an additive prior
	// allowance would let the combined prompt exceed the window headroom.
	baseMaxCompactTokens := cfg.MaxCompactTokens
	if baseMaxCompactTokens <= 0 {
		baseMaxCompactTokens = 30000
	}

	// Bound the summary output and derive the hard input budget: window minus
	// output reserve minus the fixed system prompt and wrapper framing. A
	// window that cannot hold even that fails closed before claiming rows.
	maxOutputTokens := maxCompactionSummaryTokens
	if cfg.SummaryWindowTokens > 0 {
		maxOutputTokens = min(maxCompactionSummaryTokens, max(1, cfg.SummaryWindowTokens/10))
	}
	if cfg.HistoryBudgetTokens > 0 {
		outputBudget := cfg.HistoryBudgetTokens
		if cfg.TargetTokens > 0 {
			outputBudget = min(outputBudget, cfg.TargetTokens)
		}
		maxOutputTokens = summaryOutputLimit(outputBudget, maxOutputTokens)
		if maxOutputTokens <= 0 {
			return Result{Status: StatusNoop}, nil
		}
	}
	maxCompactTokens, err := boundedCompactionInputTokens(cfg, baseMaxCompactTokens, maxOutputTokens, systemPrompt)
	if err != nil {
		return Result{}, err
	}

	frontier, err := NewArtifactProjection(s.queries).LoadActiveSession(ctx, ArtifactOwner{BotID: cfg.BotID, SessionID: cfg.SessionID, SessionIDKnown: true})
	if err != nil {
		return Result{}, err
	}
	for _, issue := range frontier.Issues {
		s.logger.WarnContext(ctx, "compaction: ignored invalid artifact lineage", slog.String("issue", issue.Error()))
	}
	canFuse := shouldFuseFrontier(cfg, frontier.Artifacts, maxCompactTokens) && frontierHasPersistedCoverage(frontier.Artifacts)
	rawTarget := cfg.TargetTokens
	if rawTarget > 0 {
		rawTarget = max(1, rawTarget-frontierSummaryTokens(frontier.Artifacts))
	}
	var priorSummaries []string
	for _, artifact := range frontier.Artifacts {
		if strings.TrimSpace(artifact.Summary) != "" {
			priorSummaries = append(priorSummaries, artifact.Summary)
		}
	}
	priorSummaries = capPriorSummaries(priorSummaries, maxCompactTokens/4)
	priorTokens := priorContextTokens(priorSummaries)
	entriesBudget := max(maxCompactTokens-priorTokens, maxCompactTokens/2)

	selectCfg := cfg
	selectCfg.TargetTokens = rawTarget
	scan, err := s.readCompactionSpan(ctx, sessionUUID, selectCfg, measure, entriesBudget)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		if ctx.Err() == nil {
			if scanErr := s.advanceScan(ctx, sessionUUID, scan); scanErr != nil {
				if err != nil {
					scanErr = fmt.Errorf("%w; %s", scanErr, err.Error())
				}
				err = scanErr
			}
			res.ScanPending = err == nil && res.Status == StatusNoop && !scan.done && scan.after.Valid &&
				(scan.state.CompactionScanEpoch != scan.state.CompactionEpoch || scan.after != scan.state.CompactionScanAfter)
		}
	}()
	rows, messages, toCompact := scan.rows, scan.items, scan.span
	truncatedRead := scan.partial
	fusing := len(toCompact) == 0 && scan.done && canFuse
	if len(toCompact) == 0 && !fusing {
		return Result{Status: StatusNoop}, nil
	}
	selectedSystemPrompt := systemPrompt
	var absorbedSegments []absorbedSegment
	absorbTokens := 0
	if fusing {
		selectedSystemPrompt = fusionSystemPrompt
		maxCompactTokens, err = boundedCompactionInputTokens(cfg, baseMaxCompactTokens, maxOutputTokens, selectedSystemPrompt)
		if err != nil {
			return Result{}, err
		}
		absorbedSegments, absorbTokens, err = buildAbsorbedContext(ctx, frontier.Artifacts, maxCompactTokens/2, s.loadAbsorbedRows)
		if err != nil {
			return Result{}, err
		}
		priorSummaries, priorTokens = nil, 0
	}

	s.logger.InfoContext(ctx, "compaction: before trim",
		slog.Int("messages", len(toCompact)),
		slog.Int("total_uncompacted", len(messages)),
		slog.Int("max_compact_tokens", maxCompactTokens),
		slog.Int("prior_context_tokens", priorTokens),
		slog.Int("absorbed_context_tokens", absorbTokens),
	)
	// The progress guarantee may keep one oversized markable group past the
	// entries budget; the prior context is reference-only, so shrink it (down
	// to nothing) before letting the combined prompt exceed MaxCompactTokens.
	if entriesCost := markableCompactCost(toCompact); !fusing && entriesCost+priorTokens > maxCompactTokens {
		priorSummaries = capPriorSummaries(priorSummaries, maxCompactTokens-entriesCost)
		priorTokens = priorContextTokens(priorSummaries)
	}
	s.logger.InfoContext(ctx, "compaction: after trim",
		slog.Int("messages", len(toCompact)),
		slog.Int("prior_summaries", len(priorSummaries)),
		slog.Int("absorbed_segments", len(absorbedSegments)),
	)

	entries, compactedMessageIDs := buildEntriesAndIDs(toCompact)
	if len(compactedMessageIDs) > 0 {
		scan.after = compactedMessageIDs[len(compactedMessageIDs)-1]
	}
	if (len(entries) == 0 || len(compactedMessageIDs) == 0) && !fusing {
		// No complete group survived: every selected group had a row that rendered
		// empty (a reasoning-only message, or a tool exchange whose result renders
		// empty). buildEntriesAndIDs withholds such a group from both entries and
		// ids, so summarizing here would either destroy rows for a junk summary or
		// mark rows we cannot faithfully summarize. Leave them in raw history.
		return Result{Status: StatusNoop}, nil
	}
	// Cap the rendered entries and verify the final prompt cost before any
	// row is claimed: a selection that cannot fit (an unsplittable tool
	// exchange larger than the budget) must fail closed with zero claims and
	// zero provider calls instead of overflowing the summarizer window.
	contextTokens := priorTokens + absorbTokens
	entries = capEntriesToBudget(entries, maxCompactTokens-contextTokens)
	if cost := entriesPromptCost(entries); cost+contextTokens > maxCompactTokens {
		err = fmt.Errorf("%w: entries=%d entry_tokens=%d max_compact_tokens=%d",
			errCompactionInputOverflow, len(entries), cost, maxCompactTokens)
		if !fusing {
			err = fmt.Errorf("%w: %w", ErrIneffectiveSummary, err)
		}
		return Result{}, err
	}

	replayBudget, retainedTokens := 0, 0
	if cfg.HistoryBudgetTokens > 0 {
		retainedTokens = scan.behindTokens + retainedReplayTokens(messages, compactedMessageIDs, frontier.Artifacts, fusing, cfg.ProtectedSources)
		replayBudget = cfg.TargetTokens
		if replayBudget <= 0 {
			replayBudget = cfg.HistoryBudgetTokens
		}
		if remaining := cfg.HistoryBudgetTokens - retainedTokens; remaining > summaryProviderReplayTokens("") {
			replayBudget = min(replayBudget, remaining)
		}
		maxOutputTokens = summaryOutputLimit(replayBudget, maxOutputTokens)
		if maxOutputTokens <= 0 {
			return Result{Status: StatusNoop}, nil
		}
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
	if fusing {
		artifact, err = rollupArtifactMetadata(frontier.Artifacts, artifact)
		if err != nil {
			_ = s.completeLog(persistCtx, logID, "error", "", err.Error(), 0, nil, pgtype.UUID{}, nil)
			return Result{}, err
		}
	}

	userPrompt := buildUserPrompt(priorSummaries, entries)
	if fusing {
		userPrompt = buildFusionUserPrompt(priorSummaries, absorbedSegments, entries)
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
		selectedSystemPrompt, []sdk.Message{sdk.UserMessage(userPrompt)}, nil,
	)

	request := sdk.Request{
		System:    systemPromptDecorated,
		Messages:  sdkMessages,
		MaxTokens: &maxOutputTokens,
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
	replacementTokens := entriesPromptCost(entries)
	if fusing {
		replacementTokens = summaryReplacementTokens(entries, frontier.Artifacts)
	}
	if summaryTokens := estimateSummaryReplayTokens(summary); summaryTokens >= replacementTokens {
		reason := ErrIneffectiveSummary
		if fusing {
			reason = errors.New("compaction: rollup does not reduce replay tokens")
		}
		err = fmt.Errorf("%w: summary_tokens=%d raw_tokens=%d", reason, summaryTokens, replacementTokens)
		_ = s.completeLog(persistCtx, logID, "error", "", err.Error(), 0, nil, pgtype.UUID{}, nil)
		return Result{}, err
	}

	if replayBudget > 0 && summaryProviderReplayTokens(summary) > replayBudget {
		err = fmt.Errorf("compaction: summary replay exceeds output budget: replay=%d budget=%d", summaryProviderReplayTokens(summary), replayBudget)
		_ = s.completeLog(persistCtx, logID, "error", "", err.Error(), 0, nil, pgtype.UUID{}, nil)
		return Result{}, err
	}

	usageJSON, _ := json.Marshal(result.Usage)

	modelUUID := db.ParseUUIDOrEmpty(cfg.ModelRecordID)
	if fusing {
		err = s.completeRollupLog(persistCtx, logID, summary, len(compactedMessageIDs), usageJSON, modelUUID, artifact, frontier.Artifacts)
	} else {
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
	if cfg.HistoryBudgetTokens > 0 && (truncatedRead || retainedTokens+summaryProviderReplayTokens(summary) > cfg.HistoryBudgetTokens) {
		status = StatusProgress
	}
	return Result{Status: status, Summary: summary, MessageCount: len(compactedMessageIDs)}, nil
}

func boundedCompactionInputTokens(cfg TriggerConfig, maxCompactTokens, maxOutputTokens int, prompt string) (int, error) {
	if cfg.SummaryWindowTokens <= 0 {
		return maxCompactTokens, nil
	}
	fixedPromptTokens := estimateBytesAsTokens(prompt) + compactionPromptFramingTokens
	inputBudget := cfg.SummaryWindowTokens - maxOutputTokens - fixedPromptTokens
	if inputBudget <= 0 {
		return 0, fmt.Errorf("%w: window=%d output_reserve=%d fixed_prompt=%d",
			ErrSummaryWindowTooSmall, cfg.SummaryWindowTokens, maxOutputTokens, fixedPromptTokens)
	}
	return min(maxCompactTokens, inputBudget), nil
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

// compactionPromptFramingTokens is a fixed safety allowance for the user
// prompt wrapper, entry headers, and provider framing that the byte estimator
// does not attribute to any single entry.
const compactionPromptFramingTokens = 512
