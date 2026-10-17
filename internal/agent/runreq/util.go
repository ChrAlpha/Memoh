package runreq

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db"
)

// SanitizeMessages normalizes user content and image parts, and drops messages
// with no role or with neither content nor a tool call ID.
func SanitizeMessages(messages []ModelMessage) []ModelMessage {
	cleaned := make([]ModelMessage, 0, len(messages))
	for _, msg := range messages {
		msg = NormalizeUserMessageContent(msg)
		if normalized, ok := NormalizeImagePartsToDataURL(msg); ok {
			msg = normalized
		}
		if strings.TrimSpace(msg.Role) == "" {
			continue
		}
		if !msg.HasContent() && strings.TrimSpace(msg.ToolCallID) == "" {
			continue
		}
		cleaned = append(cleaned, msg)
	}
	return cleaned
}

// NormalizeImagePartsToDataURL rewrites byte-array image parts as data URLs and reports whether it changed msg.
func NormalizeImagePartsToDataURL(msg ModelMessage) (ModelMessage, bool) {
	if len(msg.Content) == 0 {
		return msg, false
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(msg.Content, &parts); err != nil || len(parts) == 0 {
		return msg, false
	}

	changed := false
	for i := range parts {
		partTypeRaw, ok := parts[i]["type"]
		if !ok {
			continue
		}
		var partType string
		if err := json.Unmarshal(partTypeRaw, &partType); err != nil || !strings.EqualFold(partType, "image") {
			continue
		}

		imageRaw, ok := parts[i]["image"]
		if !ok || len(imageRaw) == 0 {
			continue
		}
		var tmp string
		if json.Unmarshal(imageRaw, &tmp) == nil {
			continue
		}

		var payload []byte
		if b, ok := decodeIndexedByteObject(imageRaw); ok {
			payload = b
		} else if b, ok := decodeByteArray(imageRaw); ok {
			payload = b
		} else {
			continue
		}
		if len(payload) == 0 {
			continue
		}

		// action trigger to image only here.
		mediaType := "application/octet-stream"
		if mediaTypeRaw, ok := parts[i]["mediaType"]; ok {
			var mt string
			if err := json.Unmarshal(mediaTypeRaw, &mt); err == nil && strings.TrimSpace(mt) != "" {
				mediaType = strings.TrimSpace(mt)
			}
		}
		dataURL := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(payload)
		rebuilt, err := json.Marshal(dataURL)
		if err != nil {
			continue
		}
		parts[i]["image"] = rebuilt
		changed = true
	}

	if !changed {
		return msg, false
	}
	rebuiltContent, err := json.Marshal(parts)
	if err != nil {
		return msg, false
	}
	msg.Content = rebuiltContent
	return msg, true
}

func decodeByteArray(raw json.RawMessage) ([]byte, bool) {
	var arr []int
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, false
	}
	if len(arr) == 0 {
		return nil, false
	}
	out := make([]byte, len(arr))
	for i, v := range arr {
		if v < 0 || v > 255 {
			return nil, false
		}
		out[i] = byte(v)
	}
	return out, true
}

func decodeIndexedByteObject(raw json.RawMessage) ([]byte, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || len(obj) == 0 {
		return nil, false
	}
	type indexedByte struct {
		idx int
		val byte
	}
	items := make([]indexedByte, 0, len(obj))
	for k, vRaw := range obj {
		idx, err := strconv.Atoi(k)
		if err != nil || idx < 0 {
			return nil, false
		}
		var val int
		if err := json.Unmarshal(vRaw, &val); err != nil || val < 0 || val > 255 {
			return nil, false
		}
		items = append(items, indexedByte{idx: idx, val: byte(val)})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].idx < items[j].idx })
	for i := range items {
		if items[i].idx != i {
			return nil, false
		}
	}
	out := make([]byte, len(items))
	for i := range items {
		out[i] = items[i].val
	}
	return out, true
}

// NonNilModelMessages is m, or an empty slice when m is nil.
func NonNilModelMessages(m []ModelMessage) []ModelMessage {
	if m == nil {
		return []ModelMessage{}
	}
	return m
}

// ParseServiceUUID parses a UUID taken from a service request.
func ParseServiceUUID(id string) (pgtype.UUID, error) {
	if strings.TrimSpace(id) == "" {
		return pgtype.UUID{}, errors.New("empty id")
	}
	return db.ParseUUID(id)
}

// IntPointer returns a pointer to value.
func IntPointer(value int) *int { return &value }

func cloneAnyMap(input map[string]any) map[string]any {
	cloned := make(map[string]any, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}

// ReadAnyString is value as a string, or "" when it is not one.
func ReadAnyString(value any) string {
	text, _ := value.(string)
	return text
}

func anyIndexedByteObject(value any) ([]byte, bool) {
	obj, ok := value.(map[string]any)
	if !ok || len(obj) == 0 {
		return nil, false
	}
	indexes := make([]int, 0, len(obj))
	values := make(map[int]byte, len(obj))
	for key, raw := range obj {
		idx, err := strconv.Atoi(strings.TrimSpace(key))
		if err != nil || idx < 0 {
			return nil, false
		}
		byteValue, ok := anyNumberToByte(raw)
		if !ok {
			return nil, false
		}
		indexes = append(indexes, idx)
		values[idx] = byteValue
	}
	sort.Ints(indexes)
	if indexes[len(indexes)-1]+1 != len(indexes) {
		return nil, false
	}
	bytes := make([]byte, len(indexes))
	for _, idx := range indexes {
		bytes[idx] = values[idx]
	}
	return bytes, true
}

func anyNumberToByte(value any) (byte, bool) {
	floatValue, ok := value.(float64)
	if !ok || math.IsNaN(floatValue) || math.IsInf(floatValue, 0) {
		return 0, false
	}
	if floatValue < 0 || floatValue > 255 || math.Trunc(floatValue) != floatValue {
		return 0, false
	}
	parsed, err := strconv.ParseUint(strconv.FormatFloat(floatValue, 'f', 0, 64), 10, 8)
	if err != nil {
		return 0, false
	}
	return byte(parsed), true
}

// CloneMetadataMap copies metadata; a nil map yields an empty one.
func CloneMetadataMap(metadata map[string]any) map[string]any {
	if metadata == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(metadata))
	for key, value := range metadata {
		out[key] = value
	}
	return out
}

// NonNilContext is ctx, or context.Background() when ctx is nil.
func NonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// ValidateSessionBot rejects a session that belongs to another bot.
func ValidateSessionBot(botID, sessionID, sessionBotID string) error {
	bid := strings.TrimSpace(botID)
	sid := strings.TrimSpace(sessionID)
	sb := strings.TrimSpace(sessionBotID)
	if bid == "" || sb == "" || bid == sb {
		return nil
	}
	return fmt.Errorf("session %s belongs to bot %s, not %s", sid, sb, bid)
}

// MetadataString is the trimmed string stored under key, or "".
func MetadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

// RuntimeSessionMeta overlays session metadata with runtime metadata
// (runtime wins), giving drivers one merged view of their keys.
func RuntimeSessionMeta(sess session.Thread) map[string]any {
	out := make(map[string]any, len(sess.Metadata)+len(sess.RuntimeMetadata))
	for key, value := range sess.Metadata {
		out[key] = value
	}
	for key, value := range sess.RuntimeMetadata {
		out[key] = value
	}
	return out
}

// FirstNonEmpty is the first value that is not empty after trimming.
func FirstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
