package historyfrag

import (
	"encoding/json"
	"strings"

	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// RuntimeContext is one message of runtime context a turn's request carried
// right after its input, such as the memory the turn recalled.
type RuntimeContext struct {
	Kind    string
	Message turn.ModelMessage
}

type storedRuntimeContext struct {
	Kind    string          `json:"kind"`
	Message json.RawMessage `json:"message"`
}

// RuntimeContextMetadata is the request-row metadata that records items, each
// message in the stored row format; nil when there is none.
func RuntimeContextMetadata(items []RuntimeContext) (map[string]any, error) {
	if len(items) == 0 {
		return nil, nil
	}
	stored := make([]storedRuntimeContext, 0, len(items))
	for _, item := range items {
		raw, err := MarshalStoredModelMessage(item.Message)
		if err != nil {
			return nil, err
		}
		stored = append(stored, storedRuntimeContext{Kind: item.Kind, Message: raw})
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return map[string]any{messagepkg.RuntimeContextMetadataKey: value}, nil
}

// RuntimeContextFromMetadata is the runtime context a request row records,
// decoded as rows are.
func RuntimeContextFromMetadata(messageID string, metadata map[string]any) []RuntimeContext {
	value, ok := metadata[messagepkg.RuntimeContextMetadataKey]
	if !ok {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var stored []storedRuntimeContext
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil
	}
	out := make([]RuntimeContext, 0, len(stored))
	for _, item := range stored {
		if strings.TrimSpace(item.Kind) == "" || len(item.Message) == 0 {
			continue
		}
		message := DecodeStoredModelMessage(nil, messageID, "user", item.Message)
		out = append(out, RuntimeContext{Kind: item.Kind, Message: message})
	}
	return out
}
