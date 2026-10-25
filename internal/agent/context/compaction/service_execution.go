package compaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/chat/checkpoint"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/models/modelretry"
	"github.com/felinics/memoh/internal/runtimefence"
)

// minSummaryTokens is the least room a summary must have to be worth
// writing: below it a pass fails before calling the summarizer.
const minSummaryTokens = 32

// doCompaction runs one pass: it folds the summaries of the session's active
// checkpoint and the oldest candidates into one summary and commits it as the
// session's next checkpoint. previous is the summary the event's last pass
// wrote, empty for its first. It reports false when there was nothing to
// fold.
func (s *Service) doCompaction(ctx context.Context, botUUID pgtype.UUID, sessionUUID pgtype.UUID, cfg TriggerConfig, previous string) (Pass, bool, error) {
	if cfg.HistoryBudgetTokens <= 0 || cfg.SummaryWindowTokens <= 0 {
		return Pass{}, false, failed(ReasonProtectedOverflow, fmt.Errorf("%w: history_budget=%d window=%d", errCompactionUnsized, cfg.HistoryBudgetTokens, cfg.SummaryWindowTokens))
	}
	// The summarizer may write as much as the model declares it can answer;
	// a model that declares nothing gets the output its runs get.
	capability := cfg.MaxOutputTokens
	if capability <= 0 {
		capability = models.ResolveGenerationLimits(models.ClientType(cfg.ClientType), nil, cfg.SummaryWindowTokens, 0).MaxOutputTokens
	}
	promptWindow := cfg.SummaryWindowTokens - estimateBytesAsTokens(systemPrompt) - compactionPromptFramingTokens
	if promptWindow <= 0 {
		return Pass{}, false, failed(ReasonInputOverflow, fmt.Errorf("%w: window=%d", ErrSummaryWindowTooSmall, cfg.SummaryWindowTokens))
	}

	readMaxBytes := passReadMaxBytes(cfg.SummaryWindowTokens)
	parent, err := s.checkpoints.Active(ctx, cfg.SessionID)
	if err != nil {
		return Pass{}, false, err
	}
	// A further pass absorbs the summary before it. A session moved back
	// since would have the event fold the same history again.
	if previous != "" && !slices.Contains(parent.SummaryIDs(), previous) {
		return Pass{}, false, failed(ReasonStaleSource, fmt.Errorf("%w: the active checkpoint %q no longer holds the summary %s the last pass wrote", errStaleSource, parent.ID, previous))
	}
	if err := matchView(cfg, parent); err != nil {
		return Pass{}, false, err
	}
	var earlier []string
	for _, item := range parent.Items {
		if item.SummaryID != "" {
			earlier = append(earlier, item.Summary)
		}
	}
	keptIDs := parent.RowUUIDs()
	var beforeTurn, beforeSeq pgtype.Int8
	if before := cfg.CandidatesBefore; before != nil {
		beforeTurn = pgtype.Int8{Int64: before.Turn, Valid: true}
		beforeSeq = pgtype.Int8{Int64: before.Seq, Valid: true}
	}
	viewIDs, viewExternalIDs := viewFilter(cfg)
	measure, err := s.queries.MeasureHistoryCheckpointCandidates(ctx, sqlc.MeasureHistoryCheckpointCandidatesParams{
		SessionID:            sessionUUID,
		AfterTurnPosition:    parent.Position.Turn,
		AfterTurnMessageSeq:  parent.Position.Seq,
		KeptIds:              keptIDs,
		BeforeTurnPosition:   beforeTurn,
		BeforeTurnMessageSeq: beforeSeq,
		MessageIds:           viewIDs,
		ExternalMessageIds:   viewExternalIDs,
	})
	if err != nil {
		return Pass{}, false, err
	}
	if measure.CandidateCount == 0 && len(earlier) == 0 {
		return Pass{}, false, nil
	}
	rows, err := s.queries.ListHistoryCheckpointCandidatesWithinBytes(ctx, sqlc.ListHistoryCheckpointCandidatesWithinBytesParams{
		SessionID:            sessionUUID,
		AfterTurnPosition:    parent.Position.Turn,
		AfterTurnMessageSeq:  parent.Position.Seq,
		KeptIds:              keptIDs,
		BeforeTurnPosition:   beforeTurn,
		BeforeTurnMessageSeq: beforeSeq,
		MessageIds:           viewIDs,
		ExternalMessageIds:   viewExternalIDs,
		MaxBytes:             readMaxBytes,
	})
	if err != nil {
		return Pass{}, false, err
	}
	// Use the count captured by the same SQL statement as the payload rows:
	// the history may change between the two statements.
	truncatedRead := false
	if len(rows) > 0 {
		truncatedRead = rows[0].CandidateCount > int64(len(rows))
		s.logger.InfoContext(ctx, "compaction: bounded candidate read",
			slog.String("session_id", cfg.SessionID),
			slog.Int("loaded_messages", len(rows)),
			slog.Int64("candidate_count", rows[0].CandidateCount),
			slog.Int64("candidate_bytes", rows[0].CandidateBytes),
			slog.Int64("loaded_bytes", rows[len(rows)-1].CumulativeBytes),
			slog.Int64("read_max_bytes", readMaxBytes),
			slog.Bool("truncated", truncatedRead))
	} else if measure.CandidateCount > 0 {
		return Pass{}, false, failed(ReasonInputOverflow, fmt.Errorf("%w: largest_candidate_bytes=%d read_max_bytes=%d", errCompactionInputOverflow, measure.LargestCandidateBytes, readMaxBytes))
	}

	messages, barrierCount := candidatesFromRows(rows)
	protectCurrentSources(messages, cfg.ProtectedSources)
	markAnchors(messages, cfg.Anchors)
	if barrierCount > 0 {
		s.logger.WarnContext(ctx, "compaction: kept unparseable history rows as span barriers",
			slog.Int("barrier_count", barrierCount),
			slog.String("session_id", cfg.SessionID),
		)
	}
	earlierTokens := 0
	for _, summary := range earlier {
		earlierTokens += summaryReplayTokens(summary)
	}

	// The summary replaces the earlier summaries and the rows it folds; what
	// the request keeps beside it bounds how long it may be.
	room := summaryRoom{HistoryTokens: cfg.HistoryBudgetTokens, KeptTokens: keptReplayTokens(messages, cfg.ProtectedSources)}
	summaryLimit := summaryOutputLimit(room.HistoryTokens-room.KeptTokens, capability)
	if summaryLimit < minSummaryTokens {
		return Pass{}, false, failed(ReasonProtectedOverflow, fmt.Errorf("%w: history_budget=%d kept=%d summary_limit=%d", errNoSummaryRoom, room.HistoryTokens, room.KeptTokens, summaryLimit))
	}
	current := currentRequestEntries(messages, cfg.ProtectedSources, cfg.CurrentRequest)
	readWindow := promptWindow - entriesPromptCost(current)
	if readWindow <= 0 {
		return Pass{}, false, failed(ReasonInputOverflow, fmt.Errorf("%w: window=%d current_request_tokens=%d", ErrSummaryWindowTooSmall, cfg.SummaryWindowTokens, entriesPromptCost(current)))
	}
	foldable := foldableItems(messages)
	earlierCost := earlierSummariesCost(earlier)
	// The window holds what a pass reads and the summary written from it. A
	// summary is shorter than what it replaces, so reading r tokens may need
	// min(summaryLimit, r) beside it. A later group is read while at least
	// half of that is left, r + min(summaryLimit, r)/2 <= readWindow: a pass
	// gives up part of its summary room rather than leave a group to another
	// pass. No pass progresses without the earlier summaries and the first
	// group, so they need only some room left beside them.
	required := earlierCost + leadingGroupCost(foldable)
	inputTokens := readWindow - max(min(summaryLimit/2, readWindow/3, readWindow-required), 1)

	toCompact := trimCompactMessages(foldable, inputTokens-earlierCost)
	entries, compactedMessageIDs := buildEntriesAndIDs(toCompact)
	if len(compactedMessageIDs) == 0 {
		if _, pending := buildEntriesAndIDs(foldable); len(pending) > 0 {
			return Pass{}, false, failed(ReasonInputOverflow, fmt.Errorf("%w: earlier_summary_tokens=%d input_tokens=%d", errCompactionInputOverflow, earlierTokens, inputTokens))
		}
		// Nothing new to fold: the summaries are refolded only when their
		// replay is itself what no longer fits the room the request leaves.
		if len(earlier) == 0 || earlierTokens <= room.HistoryTokens-room.KeptTokens {
			return Pass{}, false, nil
		}
	}
	entries, keptAfter := keptAfterSummary(entries)
	kept := append(keptInPlace(entries), keptAfter...)
	current = append(keptAfter, current...)
	readTokens := entriesPromptCost(entries) + earlierCost
	if readTokens > inputTokens {
		return Pass{}, false, failed(ReasonInputOverflow, fmt.Errorf("%w: entries=%d entry_tokens=%d input_tokens=%d", errCompactionInputOverflow, len(entries), readTokens, inputTokens))
	}
	_, remaining := buildEntriesAndIDs(withoutCompacted(foldable, compactedMessageIDs))
	// A summary must be shorter than what it replaces, so the bound the
	// prompt states keeps one that obeys it, as the request renders it,
	// under that too.
	replacedTokens := max(entriesPromptCost(coveredEntries(entries)), compactedReplayTokens(messages, compactedMessageIDs)) + earlierTokens
	maxTokens := min(capability, promptWindow-entriesPromptCost(current)-readTokens)
	room.SummaryTokens = min(summaryLimit, summaryOutputLimit(replacedTokens-1, capability), maxTokens)
	if room.SummaryTokens <= 0 {
		return Pass{}, false, failed(ReasonIneffective, fmt.Errorf("%w: replaced_tokens=%d", errIneffectiveSummary, replacedTokens))
	}
	userPrompt := buildUserPrompt(earlier, entries, current, room)
	summarizerTokens := contextfrag.ProviderEnvelopeTokens(systemPrompt, []sdk.Message{sdk.UserMessage(userPrompt)}, nil)
	if summarizerTokens+maxTokens > cfg.SummaryWindowTokens {
		return Pass{}, false, failed(ReasonInputOverflow, fmt.Errorf("%w: request_tokens=%d max_tokens=%d window=%d", errCompactionInputOverflow, summarizerTokens, maxTokens, cfg.SummaryWindowTokens))
	}
	// A pass that folds everything the request renders may ask for the
	// summary at the end of that request instead.
	var extended *sdk.Request
	if len(remaining) == 0 {
		extended = continuationRequest(cfg, buildContinuationPrompt(kept, toolResultRefs(toCompact), room), room.SummaryTokens, capability)
	}

	if err := ctx.Err(); err != nil {
		return Pass{}, false, err
	}
	persistCtx := context.WithoutCancel(ctx)
	logID, err := s.openLog(persistCtx, botUUID, sessionUUID, cfg)
	if err != nil {
		return Pass{}, false, err
	}
	fail := func(err error) (Pass, bool, error) {
		_ = s.completeLog(persistCtx, s.queries, logID, "error", "", err.Error(), 0, nil, pgtype.UUID{})
		return Pass{}, false, err
	}
	folding := toCompact
	if len(compactedMessageIDs) > 0 {
		assetRows, err := s.queries.ListMessageAssetsBatch(persistCtx, compactedMessageIDs)
		if err != nil {
			return fail(fmt.Errorf("load compaction message assets: %w", err))
		}
		if folding, err = candidatesWithAssets(toCompact, rows, assetRows); err != nil {
			return fail(err)
		}
	}
	if err := s.captureSourceVersions(ctx, cfg, folding); err != nil {
		return fail(err)
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
	generate := func(request sdk.Request) (sdk.ModelResult, error) {
		// A transient failure gets one immediate retry, or the wait the
		// provider asks for; a rejected request is not made again.
		return modelretry.Do(models.WithModelSession(ctx, cfg.SessionID), s.logger, "agent.compaction",
			modelretry.Config{MaxAttempts: 1, FastAttempts: 1}, modelretry.Retryable,
			func(ctx context.Context) (sdk.ModelResult, error) { return model.Generate(ctx, request) })
	}
	var result sdk.ModelResult
	if extended != nil {
		s.logger.InfoContext(ctx, "compaction: summary asked at the end of the run's request",
			slog.String("session_id", cfg.SessionID),
			slog.Int("request_tokens", contextfrag.ProviderEnvelopeTokens(extended.System, extended.Messages, extended.Tools)),
			slog.Int("summarizer_request_tokens", summarizerTokens))
		result, err = generate(*extended)
		if err == nil && (len(result.ToolCalls) > 0 || result.FinishReason != sdk.FinishReasonStop || strings.TrimSpace(result.Text) == "") {
			err = fmt.Errorf("finish_reason=%s tool_calls=%d", result.FinishReason, len(result.ToolCalls))
		}
		if err != nil && ctx.Err() == nil {
			s.logger.WarnContext(ctx, "compaction: extended request gave no summary, asking the summarizer",
				slog.String("session_id", cfg.SessionID), slog.Any("error", err))
			extended = nil
		}
	}
	if extended == nil {
		result, err = generate(request)
	}
	if err != nil {
		return fail(failed(ReasonSummarizerFailed, err))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}

	summary := strings.TrimSpace(result.Text)
	if summary == "" {
		return fail(failed(ReasonEmpty, errEmptySummary))
	}
	if result.FinishReason != sdk.FinishReasonStop {
		return fail(failed(ReasonTruncated, fmt.Errorf("%w: finish_reason=%s", errIncompleteSummary, result.FinishReason)))
	}
	replay := summaryReplayTokens(summary)
	if replay >= replacedTokens {
		return fail(failed(ReasonIneffective, fmt.Errorf("%w: summary_tokens=%d replaced_tokens=%d", errIneffectiveSummary, replay, replacedTokens)))
	}
	if replay > room.HistoryTokens-room.KeptTokens {
		return fail(failed(ReasonStillOver, fmt.Errorf("%w: replay=%d room=%d", errNoSummaryRoom, replay, room.HistoryTokens-room.KeptTokens)))
	}

	usageJSON, _ := json.Marshal(result.Usage)
	modelUUID := db.ParseUUIDOrEmpty(cfg.ModelRecordID)
	fold := checkpoint.Fold{
		BotID:     cfg.BotID,
		SessionID: cfg.SessionID,
		Parent:    parent,
		SummaryID: formatUUID(logID),
		Summary:   summary,
		Read:      make([]string, 0, len(rows)),
	}
	for _, row := range rows {
		fold.Read = append(fold.Read, formatUUID(row.ID))
	}
	for _, item := range folding {
		if !item.HasPolicy(CompactPolicyAnchor) {
			fold.Folded = append(fold.Folded, checkpoint.Row{ID: formatUUID(item.ID), Position: item.Position})
		}
	}
	if err := s.inSession(ctx, cfg, func(q dbstore.Queries) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := s.checkpoints.Commit(ctx, q, fold); err != nil {
			return err
		}
		return s.completeLog(ctx, q, logID, "ok", summary, "", len(compactedMessageIDs), usageJSON, modelUUID)
	}); err != nil {
		// Nothing of the pass committed: its log settles as an error and the
		// rows it read stay candidates of the checkpoint the session has.
		return fail(staleSource(err))
	}
	pass := Pass{ArtifactID: fold.SummaryID, Summary: summary, Absorbed: parent.SummaryIDs(), More: truncatedRead || len(remaining) > 0}
	for _, item := range toCompact {
		if item.HasPolicy(CompactPolicyAnchor) {
			continue
		}
		pass.Covered = append(pass.Covered, turn.ContextMessageSource{Kind: "history", ID: formatUUID(item.ID)})
		if id := strings.TrimSpace(item.Record.ExternalMessageID); id != "" {
			pass.Covered = append(pass.Covered, turn.ContextMessageSource{Kind: "external", ID: id})
		}
	}
	return pass, true, nil
}

// viewFilter restricts the candidate read to the rows the caller's request
// renders, or leaves it open when the caller names no view.
func viewFilter(cfg TriggerConfig) ([]pgtype.UUID, []string) {
	if cfg.View == nil {
		return nil, nil
	}
	ids, externalIDs := []pgtype.UUID{}, []string{}
	for _, source := range append(slices.Clone(cfg.View), cfg.Anchors...) {
		switch source.Kind {
		case "history":
			if id, err := db.ParseUUID(source.ID); err == nil {
				ids = append(ids, id)
			}
		case "external":
			externalIDs = append(externalIDs, source.ID)
		}
	}
	return ids, externalIDs
}

// failed names the reason a pass ends the event with.
func failed(reason Reason, err error) error {
	return &EventError{Reason: reason, Err: err}
}

// rewindReplacedTurn moves the session to the checkpoint an edit or retry's
// replacement keeps, in a transaction fenced by the caller's runtime
// ownership. The request renders the history before the replaced turn, which
// only that checkpoint stands for; one that depends on the turn would fold
// into the summary rows the request does not show.
func (s *Service) rewindReplacedTurn(ctx context.Context, botUUID, sessionUUID pgtype.UUID, cfg TriggerConfig) error {
	turnUUID, err := db.ParseUUID(cfg.ReplacedTurnID)
	if err != nil {
		return err
	}
	var rewound bool
	if err := s.inSession(ctx, cfg, func(q dbstore.Queries) error {
		rewound, err = q.RewindHistoryCheckpointBeforeTurn(ctx, sqlc.RewindHistoryCheckpointBeforeTurnParams{
			SessionID: sessionUUID, BotID: botUUID, TurnID: turnUUID,
		})
		return err
	}); err != nil {
		return staleSource(err)
	}
	if rewound {
		s.logger.InfoContext(ctx, "compaction: rewound the checkpoint that depends on the replaced turn",
			slog.String("session_id", cfg.SessionID),
			slog.String("turn_id", cfg.ReplacedTurnID),
		)
	}
	return nil
}

// openLog records a pass before its summarizer call, fenced by the caller's
// runtime ownership.
func (s *Service) openLog(ctx context.Context, botUUID, sessionUUID pgtype.UUID, cfg TriggerConfig) (pgtype.UUID, error) {
	var logID pgtype.UUID
	if err := s.inSession(ctx, cfg, func(q dbstore.Queries) error {
		row, err := q.CreateCompactionLog(ctx, sqlc.CreateCompactionLogParams{BotID: botUUID, SessionID: sessionUUID})
		logID = row.ID
		return err
	}); err != nil {
		return pgtype.UUID{}, staleSource(err)
	}
	return logID, nil
}

// transactionRunner is a store that can run work in one transaction.
type transactionRunner interface {
	InTx(context.Context, func(dbstore.Queries) error) error
	SupportsTransactions() bool
}

// inSession runs fn in one transaction, fenced by the caller's runtime
// ownership when it holds one, so a run that lost it cannot write. Without a
// fence the transaction locks the bot before the session, as fenced writes
// do. A store without transactions runs fn directly.
func (s *Service) inSession(ctx context.Context, cfg TriggerConfig, fn func(dbstore.Queries) error) error {
	if _, ok := runtimefence.FromContext(ctx); ok {
		return runtimefence.InTransaction(ctx, s.queries, cfg.BotID, cfg.SessionID, fn)
	}
	if txer, ok := s.queries.(transactionRunner); ok && txer.SupportsTransactions() {
		return txer.InTx(ctx, func(q dbstore.Queries) error {
			if err := runtimefence.LockBotForSessionWrite(ctx, q, cfg.BotID); err != nil {
				return err
			}
			return fn(q)
		})
	}
	return fn(s.queries)
}

// staleSource classifies a write the database refused because the session
// or its history moved on: lost runtime ownership, a checkpoint that moved,
// or a log that is no longer pending.
func staleSource(err error) error {
	if errors.Is(err, errStaleSource) || errors.Is(err, checkpoint.ErrMoved) || errors.Is(err, runtimefence.ErrStale) || errors.Is(err, pgx.ErrNoRows) {
		return failed(ReasonStaleSource, err)
	}
	return err
}

func (s *Service) completeLog(ctx context.Context, q dbstore.Queries, logID pgtype.UUID, status, summary, errMsg string, messageCount int, usage []byte, modelID pgtype.UUID) error {
	_, err := q.CompleteCompactionLog(ctx, sqlc.CompleteCompactionLogParams{
		ID:           logID,
		Status:       status,
		Summary:      summary,
		MessageCount: int32(messageCount), //nolint:gosec // count always small
		ErrorMessage: errMsg,
		Usage:        usage,
		ModelID:      modelID,
	})
	if err != nil {
		s.logger.ErrorContext(ctx, "failed to complete compaction log", slog.String("error", err.Error()))
		return err
	}
	return nil
}

// continuationRequest appends the summary instruction to the run's last
// request. It preserves the prefix when the model is
// unchanged and the request leaves enough room for the summary.
func continuationRequest(cfg TriggerConfig, prompt string, summaryTokens, capability int) *sdk.Request {
	c := cfg.Continuation
	if c == nil || c.Model != cfg.ModelID {
		return nil
	}
	base := contextfrag.ProviderEnvelopeTokens(c.System, c.Messages, c.Tools)
	ask := contextfrag.ProviderEnvelopeTokens("", []sdk.Message{sdk.UserMessage(prompt)}, nil)
	maxTokens := min(capability, cfg.SummaryWindowTokens-base-ask)
	if maxTokens < summaryTokens {
		return nil
	}
	request := *c
	request.Messages = append(slices.Clip(c.Messages), sdk.UserMessage(prompt))
	request.MaxTokens = &maxTokens
	return &request
}
