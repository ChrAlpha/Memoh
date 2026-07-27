package application

import (
	"encoding/json"
	"testing"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/agent/turn"
	"github.com/memohai/memoh/internal/messageconv"
)

func TestEstimateMessageTokensCountsToolCalls(t *testing.T) {
	t.Parallel()

	text := "Let me check that file."
	msg := ModelMessage{
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
	}

	got := estimateMessageTokens(msg)
	if want := contextfrag.EstimateSDKMessageTokens(messageconv.ModelMessageToSDKMessage(msg)); got != want {
		t.Fatalf("estimateMessageTokens = %d, want shared estimator value %d", got, want)
	}
	if textOnly := len(text) / 4; got <= textOnly {
		t.Fatalf("estimateMessageTokens = %d, must exceed text-only estimate %d", got, textOnly)
	}
}

func TestEstimateMessageTokensMatchesSharedEstimatorForText(t *testing.T) {
	t.Parallel()

	msg := ModelMessage{Role: "user", Content: json.RawMessage(`"What changed in the last release?"`)}
	got := estimateMessageTokens(msg)
	if want := contextfrag.EstimateSDKMessageTokens(messageconv.ModelMessageToSDKMessage(msg)); got != want {
		t.Fatalf("estimateMessageTokens = %d, want shared estimator value %d", got, want)
	}
}
