package runreq

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// NormalizeUserMessageContent rewrites a user message's content into the part list the model API takes.
func NormalizeUserMessageContent(msg ModelMessage) ModelMessage {
	if !strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
		return msg
	}
	normalized, changed := normalizeUserContentParts(msg.Content)
	if !changed {
		return msg
	}
	msg.Content = normalized
	return msg
}

func normalizeUserContentParts(content json.RawMessage) (json.RawMessage, bool) {
	if len(content) == 0 {
		return nil, false
	}
	var parts []map[string]any
	if err := json.Unmarshal(content, &parts); err != nil || len(parts) == 0 {
		return nil, false
	}

	changed := false
	rebuilt := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		partType := strings.TrimSpace(strings.ToLower(ReadAnyString(part["type"])))
		switch partType {
		case "image":
			normalized, ok, didChange := normalizeUserImagePart(part)
			if didChange {
				changed = true
			}
			if ok {
				rebuilt = append(rebuilt, normalized)
			}
		default:
			rebuilt = append(rebuilt, part)
		}
	}
	if !changed {
		return nil, false
	}
	if len(rebuilt) == 0 {
		rebuilt = append(rebuilt, map[string]any{
			"type": "text",
			"text": "[User sent an attachment]",
		})
	}
	data, err := json.Marshal(rebuilt)
	if err != nil {
		return nil, false
	}
	return data, true
}

func normalizeUserImagePart(part map[string]any) (map[string]any, bool, bool) {
	raw, ok := part["image"]
	if !ok {
		return nil, false, true
	}
	if image, ok := raw.(string); ok && strings.TrimSpace(image) != "" {
		return part, true, false
	}
	bytes, ok := anyIndexedByteObject(raw)
	if !ok {
		return nil, false, true
	}
	cloned := cloneAnyMap(part)
	mediaType := strings.TrimSpace(ReadAnyString(cloned["mediaType"]))
	encoded := base64.StdEncoding.EncodeToString(bytes)
	if mediaType != "" {
		cloned["image"] = "data:" + mediaType + ";base64," + encoded
	} else {
		cloned["image"] = encoded
	}
	return cloned, true, true
}
