package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/schedule"
)

// A scheduled fire over the real runtime and message store commits its steps
// as they land, and still reports the final assistant text and the run's usage
// to the schedule's log.
func TestPostgresScheduleFireReportsTheFinalTextAndUsage(t *testing.T) {
	h := newWSStepHistoryHarness(t, wsStepHistorySuccess)

	result, err := h.service.TriggerSchedule(context.Background(), h.botID, schedule.TriggerPayload{
		FireID:      "pg-fire",
		SessionID:   h.sessionID,
		Command:     directLifecyclePrompt,
		OwnerUserID: "user-1",
	}, "")
	if err != nil || result.Status != "ok" {
		t.Fatalf("TriggerSchedule() = %q, %v; want the run to complete", result.Status, err)
	}
	if result.Text != wsStepHistoryPartialText {
		t.Fatalf("result.Text = %q, want the final assistant text %q", result.Text, wsStepHistoryPartialText)
	}
	if len(result.UsageBytes) == 0 {
		t.Fatal("result.UsageBytes is empty, want the run's usage payload")
	}
	rows, err := h.messages.ListBySession(context.Background(), h.sessionID)
	if err != nil {
		t.Fatalf("list history: %v", err)
	}
	var assistant strings.Builder
	for _, row := range rows {
		if row.Role == "assistant" {
			assistant.WriteString(historyRowText(row))
		}
	}
	if !strings.Contains(assistant.String(), wsStepHistoryPartialText) {
		t.Fatalf("history assistant text = %q, want the committed answer", assistant.String())
	}
}
