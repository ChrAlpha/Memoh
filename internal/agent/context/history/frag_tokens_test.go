package historyfrag

import (
	"encoding/json"
	"testing"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/agent/turn"
	"github.com/memohai/memoh/internal/messageconv"
)

func TestRecordTokenEstimateFallbackCountsToolCalls(t *testing.T) {
	t.Parallel()

	text := "Let me check that file."
	record := HistoryRecord{ModelMessage: turn.ModelMessage{
		Role:    "assistant",
		Content: json.RawMessage(`"` + text + `"`),
		ToolCalls: []turn.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: turn.ToolCallFunction{
				Name:      "read_file",
				Arguments: `{"path":"/data/projects/memoh/internal/agent/runtime/native/agent.go"}`,
			},
		}},
	}}

	got := recordTokenEstimate(record)
	if want := contextfrag.EstimateSDKMessageTokens(messageconv.ModelMessageToSDKMessage(record.ModelMessage)); got != want {
		t.Fatalf("recordTokenEstimate = %d, want shared estimator value %d", got, want)
	}
	if textOnly := len(text) / 4; got <= textOnly {
		t.Fatalf("recordTokenEstimate = %d, must exceed text-only estimate %d", got, textOnly)
	}
}

func TestRecordTokenEstimatePrefersRealUsage(t *testing.T) {
	t.Parallel()

	usage := 123
	record := HistoryRecord{
		UsageOutputTokens: &usage,
		ModelMessage:      turn.ModelMessage{Role: "assistant", Content: json.RawMessage(`"short"`)},
	}
	if got := recordTokenEstimate(record); got != 123 {
		t.Fatalf("recordTokenEstimate = %d, want real usage 123", got)
	}
}
