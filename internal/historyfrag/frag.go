package historyfrag

import (
	"encoding/json"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/conversation"
)

func ToFrag(record HistoryRecord) contextfrag.ContextFrag {
	msg := modelMessageToSDKMessage(record.ModelMessage)
	kind := record.Kind
	if kind == "" {
		kind = contextfrag.KindConversationEvent
	}
	provenance := record.Provenance
	if strings.TrimSpace(provenance.Source) == "" {
		provenance.Source = string(record.SourceKind)
	}
	if strings.TrimSpace(provenance.SourceID) == "" {
		provenance.SourceID = strings.TrimSpace(record.Ref.ID)
	}
	if strings.TrimSpace(provenance.Collector) == "" {
		provenance.Collector = CollectorHistoryRecords
	}

	frag := contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID:            fragmentID(record),
		Message:       msg,
		Kind:          kind,
		Slot:          contextfrag.SlotHistory,
		Priority:      contextfrag.PriorityForMessage(msg),
		CacheClass:    contextfrag.CacheNever,
		Trust:         contextfrag.TrustExternal,
		Scope:         record.Scope,
		Source:        provenance.Source,
		SourceID:      provenance.SourceID,
		Collector:     provenance.Collector,
		Index:         provenance.Index,
		Budget:        record.Budget,
		TokenEstimate: recordTokenEstimate(record),
	})
	frag = contextfrag.WithContextRef(frag, record.Ref)
	frag.Coverage = record.Coverage
	return frag
}

func ToModelMessages(records []HistoryRecord) []conversation.ModelMessage {
	out := make([]conversation.ModelMessage, 0, len(records))
	for _, record := range records {
		out = append(out, record.ModelMessage)
	}
	return out
}

func ToSDKMessages(records []HistoryRecord) []sdk.Message {
	out := make([]sdk.Message, 0, len(records))
	for _, record := range records {
		out = append(out, modelMessageToSDKMessage(record.ModelMessage))
	}
	return out
}

func recordTokenEstimate(record HistoryRecord) int {
	if record.UsageOutputTokens != nil && *record.UsageOutputTokens > 0 {
		return *record.UsageOutputTokens
	}
	if text := record.ModelMessage.TextContent(); len(text) > 0 {
		return len(text) / 4
	}
	return len(record.ModelMessage.Content) / 4
}

func fragmentID(record HistoryRecord) string {
	source := strings.TrimSpace(string(record.SourceKind))
	if source == "" {
		source = "history"
	}
	id := strings.TrimSpace(record.Ref.ID)
	if id == "" {
		id = strings.TrimSpace(record.DBMessageID)
	}
	if id == "" {
		return "history." + source
	}
	return "history." + source + "." + id
}

func modelMessageToSDKMessage(mm conversation.ModelMessage) sdk.Message {
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

func appendTopLevelToolParts(msg *sdk.Message, mm conversation.ModelMessage) {
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
