package pipeline

import (
	"encoding/json"
	"strings"

	"github.com/memohai/memoh/internal/conversation"
	messagepkg "github.com/memohai/memoh/internal/message"
)

// DecodeTurnResponseEntry converts a persisted bot message into a TR entry for
// pipeline context composition. Only visible text is preserved; structured
// tool-call / tool-result payloads are skipped to avoid re-injecting raw JSON
// into later prompts.
func DecodeTurnResponseEntry(msg messagepkg.Message) (TurnResponseEntry, bool) {
	role := strings.TrimSpace(msg.Role)
	if role != "assistant" && role != "tool" {
		return TurnResponseEntry{}, false
	}

	var modelMsg conversation.ModelMessage
	if err := json.Unmarshal(msg.Content, &modelMsg); err != nil {
		return TurnResponseEntry{}, false
	}

	content := strings.TrimSpace(modelMsg.TextContent())
	if content == "" {
		return TurnResponseEntry{}, false
	}

	return TurnResponseEntry{
		RequestedAtMs: msg.CreatedAt.UnixMilli(),
		Role:          msg.Role,
		Content:       content,
	}, true
}
