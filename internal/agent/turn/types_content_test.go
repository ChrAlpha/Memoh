package turn

import (
	"encoding/json"
	"testing"
)

func TestModelMessageContentShapes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		content string
		calls   []ToolCall
		text    string
		parts   int
		has     bool
	}{
		{name: "empty"},
		{name: "text", content: `"hello"`, text: "hello", has: true},
		{name: "padded text", content: " \n\"hello\"", text: "hello", has: true},
		{name: "blank text", content: `"  "`, text: "  "},
		{name: "blank text with calls", content: `""`, calls: []ToolCall{{ID: "c"}}, has: true},
		{name: "null", content: `null`},
		{name: "object", content: `{"type":"text","text":"x"}`},
		{name: "invalid", content: `[{"type":`},
		{name: "text parts", content: `[{"type":"text","text":"a"},{"type":"reasoning","text":"r"},{"type":"text","text":"b"}]`, text: "a\nb", parts: 3, has: true},
		{name: "tool result part", content: `[{"type":"tool-result","toolCallId":"c","result":{"ok":true}}]`, parts: 1, has: true},
		{name: "empty parts", content: `[]`},
		{name: "non-object parts", content: `["a"]`},
	} {
		message := ModelMessage{Content: json.RawMessage(tc.content), ToolCalls: tc.calls}
		if got := message.TextContent(); got != tc.text {
			t.Errorf("%s: TextContent = %q, want %q", tc.name, got, tc.text)
		}
		if got := len(message.ContentParts()); got != tc.parts {
			t.Errorf("%s: ContentParts = %d, want %d", tc.name, got, tc.parts)
		}
		if got := message.HasContent(); got != tc.has {
			t.Errorf("%s: HasContent = %v, want %v", tc.name, got, tc.has)
		}
	}
}
