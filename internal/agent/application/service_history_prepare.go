package application

import (
	"strings"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runreq"
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

// projectInterruptedHistoryReasoning turns still-live interrupted reasoning
// into assistant text for the next model call. Durable history keeps the typed
// reasoning part; this projection covers the non-DCP fallback and providers that
// ignore reasoning_content in prior messages. Checkpoints a completed answer
// already superseded stay hidden, same as on the DCP path.
func projectInterruptedHistoryReasoning(records []historyfrag.HistoryRecord) []historyfrag.HistoryRecord {
	checkpoint := messagepkg.LatestInterruptedCheckpoint(len(records), func(i int) (bool, bool) {
		return strings.EqualFold(strings.TrimSpace(records[i].ModelMessage.Role), "assistant"),
			records[i].Metadata[messagepkg.AgentStepInterruptedMetadataKey] == true
	})
	if checkpoint < 0 {
		return records
	}
	projected := append([]historyfrag.HistoryRecord(nil), records...)
	projected[checkpoint].ModelMessage = timeline.ProjectInterruptedReasoning(records[checkpoint].ModelMessage)
	return projected
}
