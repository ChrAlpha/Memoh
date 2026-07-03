package compaction

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/contextview"
	"github.com/memohai/memoh/internal/db"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	"github.com/memohai/memoh/internal/historyfrag"
	messagepkg "github.com/memohai/memoh/internal/message"
)

// RecordCompactionCandidate is one uncompacted history record considered for
// compaction. Selection policy lives in the contextview selection engine.
type RecordCompactionCandidate struct {
	Record historyfrag.HistoryRecord
}

func recordCandidatesFromRows(rows []sqlc.ListUncompactedMessagesBySessionRow) ([]RecordCompactionCandidate, int) {
	candidates := make([]RecordCompactionCandidate, 0, len(rows))
	skipped := 0
	for _, row := range rows {
		record, err := historyfrag.FromDBMessage(rowToMessage(row), rowScopeFallback(row))
		if err != nil {
			skipped++
			continue
		}
		candidates = append(candidates, RecordCompactionCandidate{Record: record})
	}
	return candidates, skipped
}

// compactionView runs the compaction candidates through the full context
// view lifecycle: collect records into fragments, select the compactable
// window under the trigger budget, and render the summarization prompt. The
// returned candidates are the records selected for compaction, in original
// order.
func compactionView(ctx context.Context, scope contextfrag.Scope, candidates []RecordCompactionCandidate, window *contextview.CompactionWindow, priorSummaries []string) (*contextview.ContextView, []RecordCompactionCandidate, error) {
	if len(candidates) == 0 {
		return nil, nil, nil
	}
	records := make([]historyfrag.HistoryRecord, 0, len(candidates))
	for _, candidate := range candidates {
		records = append(records, candidate.Record)
	}
	builder := contextview.NewBuilder(
		contextview.NewMapCollectorRegistry(&contextview.CompactionRecordsCollector{}),
		&contextview.FragmentSelector{},
		contextview.IdentityPlacer{},
		contextview.NewMapRendererRegistry(&contextview.CompactionPromptRenderer{PriorSummaries: priorSummaries}),
	)
	view, err := builder.Build(ctx, contextview.BuildInput{
		Scope:  scope,
		Intent: contextfrag.IntentCompactionCandidates,
		Sources: []contextview.SourceSpec{{
			Name:   "compaction_records",
			Config: contextview.CompactionRecordsConfig{Records: records},
		}},
		Targets: []contextfrag.RenderTarget{contextfrag.RenderCompactionPrompt},
		Budget:  contextview.BudgetEnvelope{Compaction: window},
	})
	if err != nil {
		return nil, nil, err
	}
	selectedIndexes := make(map[int]bool, len(view.Selected))
	for _, frag := range view.Selected {
		if frag.Provenance.Index >= 0 && frag.Provenance.Index < len(candidates) {
			selectedIndexes[frag.Provenance.Index] = true
		}
	}
	toCompact := make([]RecordCompactionCandidate, 0, len(view.Selected))
	for i, candidate := range candidates {
		if selectedIndexes[i] {
			toCompact = append(toCompact, candidate)
		}
	}
	return view, toCompact, nil
}

// selectionReasonHistogram condenses the selection drop trace ("kept in
// history" for the compaction intent) into reason counts for the log line.
func selectionReasonHistogram(records []contextview.DropRecord) map[string]int {
	if len(records) == 0 {
		return nil
	}
	out := make(map[string]int, 4)
	for _, record := range records {
		out[record.Reason]++
	}
	return out
}

func rowToMessage(row sqlc.ListUncompactedMessagesBySessionRow) messagepkg.Message {
	return messagepkg.Message{
		ID:                      formatUUID(row.ID),
		BotID:                   formatUUID(row.BotID),
		SessionID:               formatUUID(row.SessionID),
		SenderChannelIdentityID: formatUUID(row.SenderChannelIdentityID),
		SenderUserID:            formatUUID(row.SenderUserID),
		SenderDisplayName:       textValue(row.SenderDisplayName),
		Platform:                textValue(row.Platform),
		ExternalMessageID:       textValue(row.ExternalMessageID),
		SourceReplyToMessageID:  textValue(row.SourceReplyToMessageID),
		Role:                    row.Role,
		Content:                 row.Content,
		Metadata:                metadataMap(row.Metadata),
		Usage:                   row.Usage,
		SessionMode:             strings.TrimSpace(row.SessionMode),
		RuntimeType:             strings.TrimSpace(row.RuntimeType),
		CompactID:               formatUUID(row.CompactID),
		EventID:                 formatUUID(row.EventID),
		DisplayContent:          textValue(row.DisplayText),
		CreatedAt:               row.CreatedAt.Time,
	}
}

func rowScopeFallback(row sqlc.ListUncompactedMessagesBySessionRow) historyfrag.ScopeFallback {
	return historyfrag.ScopeFallback{
		ConversationType: textValue(row.ConversationType),
		ConversationName: strings.TrimSpace(row.ConversationName),
		ReplyTarget:      textValue(row.ReplyTarget),
	}
}

func textValue(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return strings.TrimSpace(value.String)
}

func metadataMap(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

func messageIDsFromRecordRefs(refs []contextfrag.ContextRef) ([]pgtype.UUID, error) {
	ids := make([]pgtype.UUID, 0, len(refs))
	for _, ref := range refs {
		id, err := db.ParseUUID(ref.ID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}
