package contextfrag

import (
	"encoding/json"
	"unicode/utf8"

	sdk "github.com/felinics/twilight/sdk"
)

// partJSONBytes reports the length of json.Marshal(part) without encoding it.
// Estimates price every tool exchange and file in the context several times
// per request, and those parts carry most of its bytes, so measuring them by
// encoding allocated and compacted the whole payload on each pass. The known
// part shapes are measured field by field; anything else is encoded. ok is
// false exactly when json.Marshal fails, which for the measured shapes is a
// tool argument or output whose JSON is not one document.
func partJSONBytes(part sdk.MessagePart) (n int, ok bool) {
	switch p := part.(type) {
	case sdk.ToolCallPart:
		return toolCallPartJSONBytes(p)
	case sdk.ToolResultPart:
		return toolResultPartJSONBytes(p)
	case sdk.FilePart:
		return filePartJSONBytes(p), true
	default:
		data, err := json.Marshal(part)
		return len(data), err == nil
	}
}

func toolCallPartJSONBytes(p sdk.ToolCallPart) (int, bool) { //nolint:gocritic // hugeParam: measured by value like the MessagePart it came from
	input, ok := toolArgumentsJSONBytes(p.Input)
	if !ok {
		return 0, false
	}
	var fields jsonObjectSize
	fields.add("toolCallId", jsonStringBytes(p.ToolCallID))
	fields.add("toolName", jsonStringBytes(p.ToolName))
	fields.add("input", input)
	if p.CacheControl != nil {
		fields.add("cacheControl", cacheControlJSONBytes(*p.CacheControl))
	}
	if len(p.ProviderMetadata) > 0 {
		fields.add("providerMetadata", providerMetadataJSONBytes(p.ProviderMetadata))
	}
	return fields.bytes(), true
}

func toolResultPartJSONBytes(p sdk.ToolResultPart) (int, bool) { //nolint:gocritic // hugeParam: measured by value like the MessagePart it came from
	result, ok := toolOutputJSONBytes(p.Result)
	if !ok {
		return 0, false
	}
	var fields jsonObjectSize
	fields.add("toolCallId", jsonStringBytes(p.ToolCallID))
	fields.add("toolName", jsonStringBytes(p.ToolName))
	fields.add("result", result)
	if p.IsError {
		fields.add("isError", len("true"))
	}
	if p.CacheControl != nil {
		fields.add("cacheControl", cacheControlJSONBytes(*p.CacheControl))
	}
	return fields.bytes(), true
}

func filePartJSONBytes(p sdk.FilePart) int {
	var fields jsonObjectSize
	fields.add("data", jsonStringBytes(p.Data))
	if p.MediaType != "" {
		fields.add("mediaType", jsonStringBytes(p.MediaType))
	}
	if p.Filename != "" {
		fields.add("filename", jsonStringBytes(p.Filename))
	}
	if p.CacheControl != nil {
		fields.add("cacheControl", cacheControlJSONBytes(*p.CacheControl))
	}
	return fields.bytes()
}

func toolArgumentsJSONBytes(a sdk.ToolArguments) (int, bool) {
	var fields jsonObjectSize
	if len(a.JSON) > 0 {
		raw, ok := rawJSONBytes(a.JSON)
		if !ok {
			return 0, false
		}
		fields.add("json", raw)
	}
	if a.Text != "" {
		fields.add("text", jsonStringBytes(a.Text))
	}
	return fields.bytes(), true
}

func toolOutputJSONBytes(o sdk.ToolOutput) (int, bool) {
	var fields jsonObjectSize
	if o.Text != "" {
		fields.add("text", jsonStringBytes(o.Text))
	}
	if len(o.JSON) > 0 {
		raw, ok := rawJSONBytes(o.JSON)
		if !ok {
			return 0, false
		}
		fields.add("json", raw)
	}
	return fields.bytes(), true
}

func cacheControlJSONBytes(c sdk.CacheControl) int {
	var fields jsonObjectSize
	fields.add("type", jsonStringBytes(c.Type))
	if c.TTL != "" {
		fields.add("ttl", jsonStringBytes(c.TTL))
	}
	return fields.bytes()
}

func providerMetadataJSONBytes(m sdk.ProviderMetadata) int {
	var namespaces jsonObjectSize
	for namespace, values := range m {
		if values == nil {
			namespaces.addKey(namespace, len("null"))
			continue
		}
		var entries jsonObjectSize
		for key, value := range values {
			entries.addKey(key, jsonStringBytes(value))
		}
		namespaces.addKey(namespace, entries.bytes())
	}
	return namespaces.bytes()
}

// jsonObjectSize accumulates the length of a JSON object member by member.
type jsonObjectSize struct {
	members int
	size    int
}

// add counts a member whose name needs no escaping, as struct field names do.
func (o *jsonObjectSize) add(name string, value int) {
	o.member(len(name)+2, value)
}

// addKey counts a member whose name is arbitrary text, as map keys are.
func (o *jsonObjectSize) addKey(key string, value int) {
	o.member(jsonStringBytes(key), value)
}

func (o *jsonObjectSize) member(name, value int) {
	if o.members > 0 {
		o.size++ // ,
	}
	o.members++
	o.size += name + 1 + value // name:value
}

func (o *jsonObjectSize) bytes() int {
	return o.size + 2 // {}
}

// jsonStringBytes is the length of s encoded as a JSON string the way
// encoding/json writes it: quoted, with HTML-sensitive characters, control
// characters, U+2028 and U+2029 escaped, and each invalid UTF-8 byte replaced
// by \ufffd.
func jsonStringBytes(s string) int {
	n := 2
	for i := 0; i < len(s); {
		start := i
		for i < len(s) && jsonPlainASCII[s[i]] {
			i++
		}
		n += i - start
		if i == len(s) {
			break
		}
		if b := s[i]; b < utf8.RuneSelf {
			switch b {
			case '\\', '"', '\b', '\f', '\n', '\r', '\t':
				n += 2
			default:
				n += len(`\u0000`)
			}
			i++
			continue
		}
		c, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case c == utf8.RuneError && size == 1:
			n += len(`\ufffd`)
		case c == '\u2028' || c == '\u2029':
			n += len(`\u2028`)
		default:
			n += size
		}
		i += size
	}
	return n
}

// jsonPlainASCII marks the ASCII bytes encoding/json writes into a string
// unchanged.
var jsonPlainASCII = func() (plain [256]bool) {
	for c := 0x20; c < utf8.RuneSelf; c++ {
		plain[c] = true
	}
	for _, c := range []byte{'"', '\\', '<', '>', '&'} {
		plain[c] = false
	}
	return plain
}()

// rawJSONStringPlain marks the bytes a string literal keeps through
// compaction, each counting one byte.
var rawJSONStringPlain = func() (plain [256]bool) {
	for c := 0x20; c < len(plain); c++ {
		plain[c] = true
	}
	for _, c := range []byte{'"', '\\', '<', '>', '&', 0xE2} {
		plain[c] = false
	}
	return plain
}()

// rawJSONBytes is the length encoding/json writes for an embedded JSON
// document: compacted, with HTML-sensitive characters, U+2028 and U+2029
// escaped. It reports false for bytes that are not one JSON document, which
// json.Marshal rejects; validity follows json.Valid, nesting limit included.
func rawJSONBytes(raw []byte) (int, bool) {
	m := rawJSONMeasure{data: raw}
	m.skipSpace()
	if !m.value(0) {
		return 0, false
	}
	m.skipSpace()
	return m.size, m.pos == len(raw)
}

// rawJSONMaxDepth is the nesting encoding/json accepts.
const rawJSONMaxDepth = 10000

// rawJSONMeasure validates and measures a JSON document in one pass.
type rawJSONMeasure struct {
	data []byte
	pos  int
	size int
}

func (m *rawJSONMeasure) skipSpace() {
	for m.pos < len(m.data) {
		switch m.data[m.pos] {
		case ' ', '\t', '\n', '\r':
			m.pos++
		default:
			return
		}
	}
}

// next consumes c, counting it, when it is the next byte.
func (m *rawJSONMeasure) next(c byte) bool {
	if m.pos < len(m.data) && m.data[m.pos] == c {
		m.pos++
		m.size++
		return true
	}
	return false
}

func (m *rawJSONMeasure) value(depth int) bool {
	if m.pos >= len(m.data) {
		return false
	}
	switch c := m.data[m.pos]; {
	case c == '{':
		return depth < rawJSONMaxDepth && m.object(depth+1)
	case c == '[':
		return depth < rawJSONMaxDepth && m.array(depth+1)
	case c == '"':
		return m.quoted()
	case c == '-' || '0' <= c && c <= '9':
		return m.number()
	case c == 't':
		return m.literal("true")
	case c == 'f':
		return m.literal("false")
	case c == 'n':
		return m.literal("null")
	default:
		return false
	}
}

func (m *rawJSONMeasure) object(depth int) bool {
	m.next('{')
	m.skipSpace()
	if m.next('}') {
		return true
	}
	for {
		if m.pos >= len(m.data) || m.data[m.pos] != '"' || !m.quoted() {
			return false
		}
		m.skipSpace()
		if !m.next(':') {
			return false
		}
		m.skipSpace()
		if !m.value(depth) {
			return false
		}
		m.skipSpace()
		if m.next('}') {
			return true
		}
		if !m.next(',') {
			return false
		}
		m.skipSpace()
	}
}

func (m *rawJSONMeasure) array(depth int) bool {
	m.next('[')
	m.skipSpace()
	if m.next(']') {
		return true
	}
	for {
		if !m.value(depth) {
			return false
		}
		m.skipSpace()
		if m.next(']') {
			return true
		}
		if !m.next(',') {
			return false
		}
		m.skipSpace()
	}
}

// quoted measures a string literal. Escapes are copied as written; the bytes
// encoding/json escapes on the way out grow to six.
func (m *rawJSONMeasure) quoted() bool {
	m.next('"')
	for m.pos < len(m.data) {
		start := m.pos
		for m.pos < len(m.data) && rawJSONStringPlain[m.data[m.pos]] {
			m.pos++
		}
		m.size += m.pos - start
		if m.pos == len(m.data) {
			return false
		}
		switch c := m.data[m.pos]; {
		case c == '"':
			m.pos++
			m.size++
			return true
		case c == '\\':
			if !m.escape() {
				return false
			}
		case c < 0x20:
			return false
		case c == '<' || c == '>' || c == '&':
			m.pos++
			m.size += len(`\u0000`)
		case m.pos+2 < len(m.data) && m.data[m.pos+1] == 0x80 && m.data[m.pos+2]&^1 == 0xA8:
			m.pos += 3
			m.size += len(`\u2028`)
		default:
			m.pos++
			m.size++
		}
	}
	return false
}

func (m *rawJSONMeasure) escape() bool {
	if m.pos+1 >= len(m.data) {
		return false
	}
	switch m.data[m.pos+1] {
	case 'b', 'f', 'n', 'r', 't', '\\', '/', '"':
		m.pos += 2
		m.size += 2
		return true
	case 'u':
		if m.pos+6 > len(m.data) {
			return false
		}
		for _, h := range m.data[m.pos+2 : m.pos+6] {
			if !isHexDigit(h) {
				return false
			}
		}
		m.pos += 6
		m.size += 6
		return true
	default:
		return false
	}
}

func isHexDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func (m *rawJSONMeasure) number() bool {
	m.next('-')
	switch {
	case m.next('0'):
	case m.digits() == 0:
		return false
	}
	if m.next('.') && m.digits() == 0 {
		return false
	}
	if m.next('e') || m.next('E') {
		if !m.next('+') {
			m.next('-')
		}
		if m.digits() == 0 {
			return false
		}
	}
	return true
}

// digits consumes a run of decimal digits and reports how many there were.
func (m *rawJSONMeasure) digits() int {
	start := m.pos
	for m.pos < len(m.data) && '0' <= m.data[m.pos] && m.data[m.pos] <= '9' {
		m.pos++
	}
	m.size += m.pos - start
	return m.pos - start
}

func (m *rawJSONMeasure) literal(word string) bool {
	if len(m.data)-m.pos < len(word) || string(m.data[m.pos:m.pos+len(word)]) != word {
		return false
	}
	m.pos += len(word)
	m.size += len(word)
	return true
}
