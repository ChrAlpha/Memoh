package application

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/db"
)

func sanitizeMessages(messages []ModelMessage) []ModelMessage {
	cleaned := make([]ModelMessage, 0, len(messages))
	for _, msg := range messages {
		if msg, ok := sanitizeMessage(msg); ok {
			cleaned = append(cleaned, msg)
		}
	}
	return cleaned
}

// sanitizeMessage normalizes legacy image content and reports whether the
// message is worth sending at all.
func sanitizeMessage(msg ModelMessage) (ModelMessage, bool) {
	msg = normalizeUserMessageContent(msg)
	if normalized, ok := normalizeImagePartsToDataURL(msg); ok {
		msg = normalized
	}
	if strings.TrimSpace(msg.Role) == "" {
		return msg, false
	}
	if !msg.HasContent() && strings.TrimSpace(msg.ToolCallID) == "" {
		return msg, false
	}
	return msg, true
}

func normalizeImagePartsToDataURL(msg ModelMessage) (ModelMessage, bool) {
	if !hasImageByteField(msg.Content) {
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

// Legacy image normalization rewrites only content that names an image, so
// these probes let every other message skip decoding. A name decodes from
// its own letters or from \u escapes of them, so content holding neither
// holds no part the normalizers would change.

// mentionsImage reports whether content can hold a part typed "image". Only
// the tail of the name is matched: Go lowercases U+0130 to 'i', and no other
// non-ASCII letter lowercases to one in "mage".
func mentionsImage(content []byte) bool {
	const tail = "mage"
	for i := 0; i+len(tail) <= len(content); i++ {
		if content[i]|0x20 == tail[0] && bytes.EqualFold(content[i:i+len(tail)], []byte(tail)) {
			return true
		}
	}
	return escapesASCIILetter(content)
}

// hasImageByteField reports whether content can hold an "image" field whose
// value is an object or an array, the shapes legacy rows stored bytes in.
func hasImageByteField(content []byte) bool {
	if escapesASCIILetter(content) {
		return true
	}
	const key = `"image"`
	for rest := content; ; {
		i := bytes.Index(rest, []byte(key))
		if i < 0 {
			return false
		}
		rest = rest[i+len(key):]
		value := bytes.TrimLeft(rest, jsonSpace)
		if len(value) == 0 || value[0] != ':' {
			continue
		}
		value = bytes.TrimLeft(value[1:], jsonSpace)
		if len(value) > 0 && (value[0] == '{' || value[0] == '[') {
			return true
		}
	}
}

const jsonSpace = " \t\r\n"

// escapesASCIILetter reports whether content may spell an ASCII letter as a
// \u escape. JSON writers never do, so this only sends content that does
// down the decoding path.
func escapesASCIILetter(content []byte) bool {
	for rest := content; ; {
		i := bytes.Index(rest, []byte(`\u00`))
		if i < 0 {
			return false
		}
		rest = rest[i+len(`\u00`):]
		if len(rest) < 2 {
			return false
		}
		hi, hiOK := hexValue(rest[0])
		lo, loOK := hexValue(rest[1])
		if !hiOK || !loOK {
			continue
		}
		if c := hi<<4 | lo; ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z') {
			return true
		}
	}
}

func hexValue(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
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

func nonNilModelMessages(m []ModelMessage) []ModelMessage {
	if m == nil {
		return []ModelMessage{}
	}
	return m
}

func parseServiceUUID(id string) (pgtype.UUID, error) {
	if strings.TrimSpace(id) == "" {
		return pgtype.UUID{}, errors.New("empty id")
	}
	return db.ParseUUID(id)
}
