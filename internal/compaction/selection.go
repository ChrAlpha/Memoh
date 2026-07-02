package compaction

import (
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

// selectCompactionRecords routes the windowing decision through the
// contextview selection engine and maps the selected fragments back to their
// records.
func selectCompactionRecords(candidates []RecordCompactionCandidate, window *contextview.CompactionWindow) []RecordCompactionCandidate {
	if len(candidates) == 0 {
		return nil
	}
	frags := make([]contextfrag.ContextFrag, 0, len(candidates))
	for i, candidate := range candidates {
		frag := historyfrag.ToFrag(candidate.Record)
		frag.Provenance.Index = i
		frags = append(frags, frag)
	}
	selector := &contextview.FragmentSelector{}
	profile := selector.ProfileFor(contextfrag.IntentCompactionCandidates)
	result := selector.Select(frags, profile, contextview.BudgetEnvelope{Compaction: window})
	if len(result.Selected) == 0 {
		return nil
	}
	selectedIndexes := make(map[int]bool, len(result.Selected))
	for _, frag := range result.Selected {
		if frag.Provenance.Index >= 0 && frag.Provenance.Index < len(candidates) {
			selectedIndexes[frag.Provenance.Index] = true
		}
	}
	out := make([]RecordCompactionCandidate, 0, len(result.Selected))
	for i, candidate := range candidates {
		if selectedIndexes[i] {
			out = append(out, candidate)
		}
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
