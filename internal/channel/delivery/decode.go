package delivery

import (
	"encoding/json"
	"strings"

	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/attachment"
	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/i18n"
)

// agentStreamEnvelope is the JSON shape produced by internal/agent.StreamEvent.
type agentStreamEnvelope struct {
	Type     string              `json:"type"`
	Delta    string              `json:"delta"`
	Code     string              `json:"code"`
	Error    string              `json:"error"`
	Message  string              `json:"message"`
	Data     json.RawMessage     `json:"data"`
	Messages []turn.ModelMessage `json:"messages"`

	ToolName    string          `json:"toolName"`
	ToolCallID  string          `json:"toolCallId"`
	ApprovalID  string          `json:"approvalId"`
	UserInputID string          `json:"userInputId"`
	ShortID     int             `json:"shortId"`
	Status      string          `json:"status"`
	Input       json.RawMessage `json:"input"`
	Result      json.RawMessage `json:"result"`
	Metadata    json.RawMessage `json:"metadata"`
	Attachments json.RawMessage `json:"attachments"`
	Reactions   json.RawMessage `json:"reactions"`
}

// Decode converts one agent stream chunk into channel events and the model
// messages the chunk carries. An error event whose code has copy in t is shown
// with that copy; any other error keeps its own text.
func Decode(chunk json.RawMessage, t *i18n.Localizer) ([]channel.StreamEvent, []turn.ModelMessage, error) {
	if len(chunk) == 0 {
		return nil, nil, nil
	}
	var envelope agentStreamEnvelope
	if err := json.Unmarshal(chunk, &envelope); err != nil {
		return nil, nil, err
	}
	finalMessages := make([]turn.ModelMessage, 0, len(envelope.Messages))
	finalMessages = append(finalMessages, envelope.Messages...)
	eventType := strings.ToLower(strings.TrimSpace(envelope.Type))
	switch eventType {
	case "text_delta":
		if envelope.Delta == "" {
			return nil, finalMessages, nil
		}
		return []channel.StreamEvent{
			{
				Type:  channel.StreamEventDelta,
				Delta: envelope.Delta,
				Phase: channel.StreamPhaseText,
			},
		}, finalMessages, nil
	case "reasoning_delta":
		if envelope.Delta == "" {
			return nil, finalMessages, nil
		}
		return []channel.StreamEvent{
			{
				Type:  channel.StreamEventDelta,
				Delta: envelope.Delta,
				Phase: channel.StreamPhaseReasoning,
			},
		}, finalMessages, nil
	case "tool_call_start":
		return []channel.StreamEvent{
			{
				Type: channel.StreamEventToolCallStart,
				ToolCall: &channel.StreamToolCall{
					Name:   strings.TrimSpace(envelope.ToolName),
					CallID: strings.TrimSpace(envelope.ToolCallID),
					Input:  parseRawJSON(envelope.Input),
				},
			},
		}, finalMessages, nil
	case "tool_call_end":
		return []channel.StreamEvent{
			{
				Type: channel.StreamEventToolCallEnd,
				ToolCall: &channel.StreamToolCall{
					Name:   strings.TrimSpace(envelope.ToolName),
					CallID: strings.TrimSpace(envelope.ToolCallID),
					Input:  parseRawJSON(envelope.Input),
					Result: parseRawJSON(envelope.Result),
				},
			},
		}, finalMessages, nil
	case "tool_approval_request":
		return []channel.StreamEvent{
			{
				Type: channel.StreamEventToolCallStart,
				ToolCall: &channel.StreamToolCall{
					Name:       strings.TrimSpace(envelope.ToolName),
					CallID:     strings.TrimSpace(envelope.ToolCallID),
					Input:      parseRawJSON(envelope.Input),
					ApprovalID: strings.TrimSpace(envelope.ApprovalID),
					ShortID:    envelope.ShortID,
					Actions: []channel.Action{
						{Type: "tool_approval", Label: "Approve", Value: "approve:" + strings.TrimSpace(envelope.ApprovalID)},
						{Type: "tool_approval", Label: "Reject", Value: "reject:" + strings.TrimSpace(envelope.ApprovalID)},
					},
				},
			},
		}, finalMessages, nil
	case "user_input_request":
		userInputID := strings.TrimSpace(envelope.UserInputID)
		if userInputID == "" {
			userInputID = strings.TrimSpace(envelope.ApprovalID)
		}
		payload := canonicalUserInputPayload(envelope.Metadata, envelope.Input)
		input := map[string]any{
			"user_input_id": userInputID,
			"short_id":      envelope.ShortID,
			"status":        strings.TrimSpace(envelope.Status),
			"payload":       parseRawJSON(payload),
		}
		actions := userInputActions(userInputID)
		return []channel.StreamEvent{
			{
				Type: channel.StreamEventToolCallStart,
				ToolCall: &channel.StreamToolCall{
					Name:    strings.TrimSpace(envelope.ToolName),
					CallID:  strings.TrimSpace(envelope.ToolCallID),
					Input:   input,
					ShortID: envelope.ShortID,
					Actions: actions,
				},
			},
		}, finalMessages, nil
	case "reasoning_start":
		return []channel.StreamEvent{
			{Type: channel.StreamEventPhaseStart, Phase: channel.StreamPhaseReasoning},
		}, finalMessages, nil
	case "reasoning_end":
		return []channel.StreamEvent{
			{Type: channel.StreamEventPhaseEnd, Phase: channel.StreamPhaseReasoning},
		}, finalMessages, nil
	case "text_start":
		return []channel.StreamEvent{
			{Type: channel.StreamEventPhaseStart, Phase: channel.StreamPhaseText},
		}, finalMessages, nil
	case "text_end":
		return []channel.StreamEvent{
			{Type: channel.StreamEventPhaseEnd, Phase: channel.StreamPhaseText},
		}, finalMessages, nil
	case "attachment_delta":
		attachments := parseAttachmentDelta(envelope.Attachments)
		if len(attachments) == 0 {
			return nil, finalMessages, nil
		}
		return []channel.StreamEvent{
			{Type: channel.StreamEventAttachment, Attachments: attachments},
		}, finalMessages, nil
	case "reaction_delta":
		reactions := parseReactionDelta(envelope.Reactions)
		if len(reactions) == 0 {
			return nil, finalMessages, nil
		}
		return []channel.StreamEvent{
			{Type: channel.StreamEventReaction, Reactions: reactions},
		}, finalMessages, nil
	case "agent_start":
		return []channel.StreamEvent{
			{
				Type: channel.StreamEventAgentStart,
				Metadata: map[string]any{
					"input": parseRawJSON(envelope.Input),
					"data":  parseRawJSON(envelope.Data),
				},
			},
		}, finalMessages, nil
	case "agent_end":
		return []channel.StreamEvent{
			{
				Type: channel.StreamEventAgentEnd,
				Metadata: map[string]any{
					"result": parseRawJSON(envelope.Result),
					"data":   parseRawJSON(envelope.Data),
				},
			},
		}, finalMessages, nil
	case "retry":
		// The agent discards the failed attempt and regenerates the reply;
		// the terminal messages carry only the surviving attempt.
		return []channel.StreamEvent{
			{Type: channel.StreamEventReset},
		}, finalMessages, nil
	case "processing_started":
		return []channel.StreamEvent{
			{Type: channel.StreamEventProcessingStarted},
		}, finalMessages, nil
	case "processing_completed":
		return []channel.StreamEvent{
			{Type: channel.StreamEventProcessingCompleted},
		}, finalMessages, nil
	case "processing_failed":
		streamError := strings.TrimSpace(envelope.Error)
		if streamError == "" {
			streamError = strings.TrimSpace(envelope.Message)
		}
		return []channel.StreamEvent{
			{
				Type:  channel.StreamEventProcessingFailed,
				Error: streamError,
			},
		}, finalMessages, nil
	case "error":
		// Without a catalogued code the event is a failed run; its own text is
		// never shown.
		return []channel.StreamEvent{channel.CodeEvent(t, apperror.Code(envelope.Code), nil)}, finalMessages, nil
	default:
		return nil, finalMessages, nil
	}
}

func canonicalUserInputPayload(metadata, fallback json.RawMessage) json.RawMessage {
	var deferred struct {
		UIPayload json.RawMessage `json:"ui_payload"`
	}
	if err := json.Unmarshal(metadata, &deferred); err == nil && len(deferred.UIPayload) > 0 && string(deferred.UIPayload) != "null" {
		return deferred.UIPayload
	}
	return fallback
}

// userInputActions emits the single marker action for a pending ask_user
// request. The marker's Type ("user_input") is what keeps the tool-call
// filter from dropping the prompt; its Value is only ever consumed by the
// Telegram legacy respond: parser as a silent no-op. Adapters with native
// controls (Telegram) rebuild the real interactive UI from the payload —
// the shared layer must not fabricate per-option keyboards here, because no
// other adapter renders user_input buttons and Telegram replaces them anyway.
func userInputActions(userInputID string) []channel.Action {
	userInputID = strings.TrimSpace(userInputID)
	if userInputID == "" {
		return nil
	}
	return []channel.Action{{Type: "user_input", Label: "Reply", Value: "respond:" + userInputID}}
}

// parseRawJSON converts raw JSON bytes to a typed value for StreamToolCall fields.
func parseRawJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

// parseAttachmentDelta converts raw JSON attachment data to channel Attachments.
func parseAttachmentDelta(raw json.RawMessage) []channel.Attachment {
	if len(raw) == 0 {
		return nil
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	attachments := make([]channel.Attachment, 0, len(items))
	for _, item := range items {
		bundle := attachment.BundleFromMap(item)
		attachments = append(attachments, channel.AttachmentFromBundle(bundle))
	}
	return attachments
}

// parseReactionDelta converts raw JSON reaction data to channel ReactRequests.
func parseReactionDelta(raw json.RawMessage) []channel.ReactRequest {
	if len(raw) == 0 {
		return nil
	}
	var items []struct {
		Emoji string `json:"emoji"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	reactions := make([]channel.ReactRequest, 0, len(items))
	for _, item := range items {
		emoji := strings.TrimSpace(item.Emoji)
		if emoji == "" {
			continue
		}
		reactions = append(reactions, channel.ReactRequest{
			Emoji: emoji,
		})
	}
	return reactions
}
