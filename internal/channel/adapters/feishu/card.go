package feishu

import (
	"encoding/json"
	"strings"
)

// Cards use the JSON 2.0 schema because only its markdown component renders
// headings, tables and fenced code blocks; the 1.0 lark_md text flattened
// them into plain paragraphs. JSON 2.0 needs Feishu client 7.20+, older
// clients show an upgrade prompt instead of the body.
const (
	feishuCardSchema = "2.0"
	// feishuCardBodyElementID names the markdown element a card's body carries.
	feishuCardBodyElementID = "body"
)

func buildFeishuCardContent(text string) (string, error) {
	return marshalFeishuCard(feishuMarkdownElement(text))
}

func buildFeishuPlainCardContent(text string) (string, error) {
	return marshalFeishuCard(map[string]any{
		"tag": "div",
		"text": map[string]any{
			"tag":     "plain_text",
			"content": strings.TrimSpace(text),
		},
	})
}

func buildFeishuStreamCardContent(text string) (string, error) {
	return buildFeishuCardContent(feishuStreamCardText(text))
}

func feishuStreamCardText(text string) string {
	return normalizeFeishuStreamText(extractReadableFromJSON(text))
}

func feishuMarkdownElement(text string) map[string]any {
	return map[string]any{
		"tag":        "markdown",
		"element_id": feishuCardBodyElementID,
		"content":    strings.TrimSpace(text),
	}
}

func marshalFeishuCard(element map[string]any) (string, error) {
	card := map[string]any{
		"schema": feishuCardSchema,
		"config": map[string]any{
			// Patching a message requires update_multi before and after.
			"update_multi":   true,
			"enable_forward": true,
		},
		"body": map[string]any{
			"elements": []map[string]any{element},
		},
	}
	data, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
