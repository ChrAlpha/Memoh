package messageconv

import (
	"encoding/json"
	"log/slog"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/agent/turn"
)

func SDKMessagesToModelMessages(msgs []sdk.Message) []turn.ModelMessage {
	return SDKMessagesToModelMessagesWithLogger(nil, msgs)
}

func SDKMessagesToModelMessagesWithLogger(log *slog.Logger, msgs []sdk.Message) []turn.ModelMessage {
	result := make([]turn.ModelMessage, 0, len(msgs))
	for _, msg := range msgs {
		data, err := json.Marshal(msg)
		if err != nil {
			if log != nil {
				log.Warn("messageconv: sdk message marshal failed", slog.String("role", string(msg.Role)), slog.Any("error", err))
			}
			continue
		}
		var envelope struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			if log != nil {
				log.Warn("messageconv: sdk message content extract failed", slog.String("role", string(msg.Role)), slog.Any("error", err))
			}
			continue
		}
		var usage json.RawMessage
		if msg.Usage != nil {
			usage, _ = json.Marshal(msg.Usage)
		}
		result = append(result, turn.ModelMessage{
			Role:    string(msg.Role),
			Content: envelope.Content,
			Usage:   usage,
		})
	}
	return result
}

func ModelMessageToSDKMessage(mm turn.ModelMessage) sdk.Message {
	var s string
	if err := json.Unmarshal(mm.Content, &s); err == nil {
		msg := sdk.Message{
			Role:    sdk.MessageRole(mm.Role),
			Content: []sdk.MessagePart{sdk.TextPart{Text: s}},
		}
		appendTopLevelToolParts(&msg, mm)
		return msg
	}

	envelope, _ := json.Marshal(struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}{
		Role:    mm.Role,
		Content: mm.Content,
	})
	var msg sdk.Message
	if err := json.Unmarshal(envelope, &msg); err == nil {
		restoreToolResultOutputs(&msg, mm.Content)
		appendTopLevelToolParts(&msg, mm)
		return msg
	}

	msg = sdk.Message{Role: sdk.MessageRole(mm.Role)}
	appendTopLevelToolParts(&msg, mm)
	return msg
}

// restoreToolResultOutputs recovers ToolResultPart.Result values that the SDK
// unmarshal dropped: legacy rows persist tool results under an "output" key
// that sdk.Message does not decode, leaving Result nil.
func restoreToolResultOutputs(msg *sdk.Message, content json.RawMessage) {
	var rawParts []json.RawMessage
	if err := json.Unmarshal(content, &rawParts); err != nil || len(rawParts) != len(msg.Content) {
		return
	}
	for i, part := range msg.Content {
		trp, ok := part.(sdk.ToolResultPart)
		if !ok || trp.Result != nil {
			continue
		}
		var probe struct {
			Output json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(rawParts[i], &probe); err != nil || len(probe.Output) == 0 {
			continue
		}
		var output any
		if err := json.Unmarshal(probe.Output, &output); err != nil || output == nil {
			continue
		}
		trp.Result = output
		msg.Content[i] = trp
	}
}

// appendTopLevelToolParts folds the OpenAI-style top-level ToolCalls/Name/
// ToolCallID columns into SDK message parts so tool closure survives rows that
// never encoded tool activity inside Content.
func appendTopLevelToolParts(msg *sdk.Message, mm turn.ModelMessage) {
	for _, call := range mm.ToolCalls {
		if hasToolCallPart(msg.Content, call.ID) {
			continue
		}
		var input any
		if strings.TrimSpace(call.Function.Arguments) != "" {
			if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
				input = call.Function.Arguments
			}
		}
		msg.Content = append(msg.Content, sdk.ToolCallPart{
			ToolCallID: strings.TrimSpace(call.ID),
			ToolName:   strings.TrimSpace(call.Function.Name),
			Input:      input,
		})
	}
	if name := strings.TrimSpace(mm.Name); name != "" {
		propagateToolName(msg, name, strings.TrimSpace(mm.ToolCallID))
	}
}

func hasToolCallPart(parts []sdk.MessagePart, callID string) bool {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return false
	}
	for _, part := range parts {
		if tcp, ok := part.(sdk.ToolCallPart); ok && tcp.ToolCallID == callID {
			return true
		}
	}
	return false
}

func propagateToolName(msg *sdk.Message, name, toolCallID string) {
	for i, part := range msg.Content {
		if trp, ok := part.(sdk.ToolResultPart); ok && strings.TrimSpace(trp.ToolName) == "" {
			trp.ToolName = name
			if toolCallID != "" && strings.TrimSpace(trp.ToolCallID) == "" {
				trp.ToolCallID = toolCallID
			}
			msg.Content[i] = trp
			return
		}
	}
}

func ModelMessagesToSDKMessages(msgs []turn.ModelMessage) []sdk.Message {
	result := make([]sdk.Message, 0, len(msgs))
	for _, mm := range msgs {
		result = append(result, ModelMessageToSDKMessage(mm))
	}
	return result
}

func PrependUserMessage(query string, output []turn.ModelMessage) []turn.ModelMessage {
	if strings.TrimSpace(query) == "" {
		return output
	}
	round := make([]turn.ModelMessage, 0, 1+len(output))
	round = append(round, turn.ModelMessage{
		Role:    "user",
		Content: turn.NewTextContent(query),
	})
	return append(round, output...)
}
