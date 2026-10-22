package runreq

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestOutboundAssetRefsToMessageRefs(t *testing.T) {
	t.Parallel()
	refs := []OutboundAssetRef{
		{ContentHash: "a1", Role: "attachment", Ordinal: 0},
		{ContentHash: "", Role: "attachment", Ordinal: 1},
		{ContentHash: "a2", Ordinal: 2},
	}
	result := OutboundAssetRefsToMessageRefs(refs)
	if len(result) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(result))
	}
	if result[0].ContentHash != "a1" || result[0].Role != "attachment" {
		t.Fatalf("unexpected ref[0]: %+v", result[0])
	}
	if result[1].ContentHash != "a2" || result[1].Role != "attachment" {
		t.Fatalf("unexpected ref[1]: %+v", result[1])
	}
}

func TestOutboundAssetRefsToMessageRefs_Empty(t *testing.T) {
	t.Parallel()
	result := OutboundAssetRefsToMessageRefs(nil)
	if result != nil {
		t.Fatalf("expected nil, got %v", result)
	}
}

func TestSanitizeMessagesNormalizesUserMultipartImageBytes(t *testing.T) {
	t.Parallel()
	content, err := json.Marshal([]map[string]any{
		{"type": "text", "text": "> quoted reply\n\nWhere is Antelope Canyon?"},
		{"type": "image", "image": map[string]any{"0": 137, "1": 80}, "mediaType": "image/png"},
	})
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}

	cleaned := SanitizeMessages([]ModelMessage{{
		Role:    "user",
		Content: content,
	}})
	if len(cleaned) != 1 {
		t.Fatalf("expected 1 message, got %d", len(cleaned))
	}
	if bytes.Equal(cleaned[0].Content, content) {
		t.Fatalf("expected user multipart content to be normalized")
	}
	var parts []map[string]any
	if err := json.Unmarshal(cleaned[0].Content, &parts); err != nil {
		t.Fatalf("unmarshal normalized content: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts after normalization, got %d", len(parts))
	}
	if got := parts[0]["text"]; got != "> quoted reply\n\nWhere is Antelope Canyon?" {
		t.Fatalf("unexpected text part: %#v", got)
	}
	image, _ := parts[1]["image"].(string)
	if !strings.HasPrefix(image, "data:image/png;base64,") {
		t.Fatalf("expected data URL image payload, got %#v", parts[1]["image"])
	}
}

func TestSanitizeMessagesKeepsAssistantMultipartMessages(t *testing.T) {
	t.Parallel()
	content, err := json.Marshal([]map[string]any{
		{"type": "text", "text": "answer"},
		{"type": "image", "image": "data:image/png;base64,aGVsbG8="},
	})
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}

	cleaned := SanitizeMessages([]ModelMessage{{
		Role:    "assistant",
		Content: content,
	}})
	if len(cleaned) != 1 {
		t.Fatalf("expected 1 message, got %d", len(cleaned))
	}
	if !bytes.Equal(cleaned[0].Content, content) {
		t.Fatalf("assistant multipart content should remain unchanged")
	}
}

func TestNormalizeImagePartsToDataURL_ConvertsIndexedObject(t *testing.T) {
	msg := ModelMessage{
		Role: "user",
		Content: json.RawMessage(`[
			{"type":"text","text":"hello"},
			{"type":"image","image":{"0":82,"1":73,"2":70,"3":70},"mediaType":"image/webp"}
		]`),
	}

	normalized, changed := NormalizeImagePartsToDataURL(msg)
	if !changed {
		t.Fatal("expected message to be normalized")
	}

	var parts []map[string]any
	if err := json.Unmarshal(normalized.Content, &parts); err != nil {
		t.Fatalf("failed to unmarshal normalized content: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	image, ok := parts[1]["image"].(string)
	if !ok {
		t.Fatalf("expected image to be string data url, got %T", parts[1]["image"])
	}
	expected := "data:image/webp;base64," + base64.StdEncoding.EncodeToString([]byte{82, 73, 70, 70})
	if image != expected {
		t.Fatalf("unexpected data url, got %q", image)
	}
}

func TestNormalizeImagePartsToDataURL_LeavesStringImageUntouched(t *testing.T) {
	original := `[
		{"type":"image","image":"data:image/png;base64,AAAA","mediaType":"image/png"}
	]`
	msg := ModelMessage{
		Role:    "user",
		Content: json.RawMessage(original),
	}

	normalized, changed := NormalizeImagePartsToDataURL(msg)
	if changed {
		t.Fatal("expected no normalization for string image")
	}
	if string(normalized.Content) != original {
		t.Fatalf("expected content unchanged, got %s", string(normalized.Content))
	}
}
