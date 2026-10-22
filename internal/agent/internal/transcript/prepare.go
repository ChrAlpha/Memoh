package transcript

import (
	"strings"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/chat/timeline"
)

func historyErrorCode(meta map[string]any) string {
	if meta == nil {
		return ""
	}
	code, _ := meta[messagepkg.HistoryErrorCodeMetadataKey].(string)
	return strings.TrimSpace(code)
}

// dropPreflightFailureTurns removes inputs rejected before the agent accepted
// them. They stay in the UI, but a later ordinary message must not smuggle a
// Hook-rejected input into the model context.
func dropPreflightFailureTurns(records []historyfrag.HistoryRecord) []historyfrag.HistoryRecord {
	if len(records) == 0 {
		return records
	}
	out := make([]historyfrag.HistoryRecord, 0, len(records))
	for _, rec := range records {
		if rec.Metadata != nil && rec.Metadata[messagepkg.HistoryFailureOriginMetadataKey] == messagepkg.HistoryFailureOriginUserMessageHook {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// dropEmptyHistoryFailures removes empty assistant rows that exist only to
// mark a timeout/interrupt. They stay in the UI, but replaying them as
// empty assistant turns is what made the next provider call 400.
func dropEmptyHistoryFailures(records []historyfrag.HistoryRecord) []historyfrag.HistoryRecord {
	if len(records) == 0 {
		return records
	}
	out := make([]historyfrag.HistoryRecord, 0, len(records))
	for _, rec := range records {
		if strings.EqualFold(strings.TrimSpace(rec.ModelMessage.Role), "assistant") &&
			historyErrorCode(rec.Metadata) != "" &&
			runreq.IsEmptyAssistantMessage(rec.ModelMessage) {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// projectInterruptedHistoryReasoning turns interrupted reasoning into
// assistant text. Durable history keeps the typed reasoning part; every
// checkpoint is projected the same way on every turn, so a superseded one
// neither rewrites the replayed prefix nor reaches the provider as unsigned
// reasoning.
func projectInterruptedHistoryReasoning(records []historyfrag.HistoryRecord) []historyfrag.HistoryRecord {
	var projected []historyfrag.HistoryRecord
	for i, record := range records {
		if !strings.EqualFold(strings.TrimSpace(record.ModelMessage.Role), "assistant") ||
			record.Metadata[messagepkg.AgentStepInterruptedMetadataKey] != true {
			continue
		}
		if projected == nil {
			projected = append([]historyfrag.HistoryRecord(nil), records...)
		}
		projected[i].ModelMessage = timeline.ProjectInterruptedReasoning(record.ModelMessage)
	}
	if projected == nil {
		return records
	}
	return projected
}
