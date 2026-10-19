package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// What a provider's prefix cache can reuse between two requests is the longest
// run of the prompt that both render identically, counted from its start. The
// prompt is the tools, then the system prompt, then the messages, in that
// order for every client type the agent speaks.
//
// The comparison works on the request bodies the real clients put on the wire,
// split into those units and each normalized to a stable serialization. The
// provider tokenizes the rendered prompt, not the byte layout of our JSON, so
// object key order and whitespace cannot cost a hit and are removed.
// cache_control is removed too: Anthropic reads the marker as a breakpoint
// position, and the cache key is the content before it. Other top-level fields
// (model, limits, routing keys, stream flags) are not part of the prompt.

// promptUnit is one element of the prompt: a tool definition, the system
// prompt, or a message.
type promptUnit struct {
	// Name is where the unit sits in the request, such as "messages[3]".
	Name string
	// Role is the message role, empty for tools and a bare system string.
	Role  string
	value any
	canon []byte
}

// promptFields lists the request fields that carry the prompt, in the order a
// provider renders them.
var promptFields = []string{"tools", "system", "instructions", "messages", "input"}

// promptUnits splits a request body into the units of its prompt.
func promptUnits(body []byte) ([]promptUnit, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("decode request body: %w", err)
	}
	var units []promptUnit
	for _, field := range promptFields {
		raw, ok := request[field]
		if !ok {
			continue
		}
		value, err := decodeNumbers(raw)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", field, err)
		}
		if items, isList := value.([]any); isList {
			for i, item := range items {
				units = append(units, newPromptUnit(fmt.Sprintf("%s[%d]", field, i), item))
			}
			continue
		}
		units = append(units, newPromptUnit(field, value))
	}
	if len(units) == 0 {
		return nil, fmt.Errorf("request body has none of %v", promptFields)
	}
	return units, nil
}

func decodeNumbers(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func newPromptUnit(name string, value any) promptUnit {
	value = withoutCacheControl(value)
	canon, _ := json.Marshal(value)
	unit := promptUnit{Name: name, value: value, canon: canon}
	if object, ok := value.(map[string]any); ok {
		unit.Role, _ = object["role"].(string)
	}
	return unit
}

func withoutCacheControl(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if key != "cache_control" {
				out[key] = withoutCacheControl(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = withoutCacheControl(item)
		}
		return out
	default:
		return value
	}
}

// prefixReport is how much of one request the next request can reuse.
type prefixReport struct {
	PrevBytes     int
	ReusableBytes int
	// Break is where the next request first differs from the previous one;
	// nil when the previous prompt is entirely a prefix of the next.
	Break *prefixBreak
}

// Ratio is the reusable share of the previous prompt.
func (r prefixReport) Ratio() float64 {
	if r.PrevBytes == 0 {
		return 1
	}
	return float64(r.ReusableBytes) / float64(r.PrevBytes)
}

// prefixBreak locates the first difference between two prompts.
type prefixBreak struct {
	// Part says what the differing unit is: tool definitions, the system
	// prompt (with the workspace file or section inside it), history, or the
	// turn in progress.
	Part string
	// Unit and Path name the unit and the JSON path of the differing value.
	Unit string
	Path string
	// Offset is the byte offset of the first differing byte within the value.
	Offset     int
	Prev, Next string
}

func (b prefixBreak) String() string {
	return fmt.Sprintf("%s at %s (%s, byte %d): previous %q, next %q", b.Part, b.Unit, b.Path, b.Offset, b.Prev, b.Next)
}

// comparePrompts measures how much of prev's prompt next reuses.
func comparePrompts(prev, next []byte) (prefixReport, error) {
	prevUnits, err := promptUnits(prev)
	if err != nil {
		return prefixReport{}, fmt.Errorf("previous request: %w", err)
	}
	nextUnits, err := promptUnits(next)
	if err != nil {
		return prefixReport{}, fmt.Errorf("next request: %w", err)
	}
	var report prefixReport
	for _, unit := range prevUnits {
		report.PrevBytes += len(unit.canon)
	}
	lastUser := lastUserUnit(nextUnits)
	for i, unit := range prevUnits {
		if i >= len(nextUnits) {
			report.Break = &prefixBreak{Part: "prompt shortened", Unit: unit.Name, Path: unit.Name, Prev: excerpt(string(unit.canon), 0)}
			return report, nil
		}
		other := nextUnits[i]
		if bytes.Equal(unit.canon, other.canon) {
			report.ReusableBytes += len(unit.canon)
			continue
		}
		report.ReusableBytes += commonPrefixLen(unit.canon, other.canon)
		path, prevText, nextText, offset := firstDifference(unit.Name, unit.value, other.value)
		report.Break = &prefixBreak{
			Part:   classifyBreak(other, i, lastUser, prevText, offset),
			Unit:   other.Name,
			Path:   path,
			Offset: offset,
			Prev:   excerpt(prevText, offset),
			Next:   excerpt(nextText, offset),
		}
		return report, nil
	}
	return report, nil
}

func commonPrefixLen(a, b []byte) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// lastUserUnit is the index of the newest user message of a prompt, the start
// of the turn in progress; -1 when there is none.
func lastUserUnit(units []promptUnit) int {
	for i := len(units) - 1; i >= 0; i-- {
		if units[i].Role == "user" {
			return i
		}
	}
	return -1
}

// classifyBreak names what the differing unit is. Text of the system prompt
// is attributed to the workspace file heading it sits under.
func classifyBreak(unit promptUnit, index, lastUser int, prevText string, offset int) string {
	switch {
	case strings.HasPrefix(unit.Name, "tools["):
		return "tool definitions"
	case strings.HasPrefix(unit.Name, "system") || strings.HasPrefix(unit.Name, "instructions") || unit.Role == "system" || unit.Role == "developer":
		return "system prompt: " + systemSection(prevText, offset)
	case lastUser >= 0 && index >= lastUser:
		return "current turn"
	default:
		return "history"
	}
}

// systemSection finds the heading the byte at offset sits under: a workspace
// file the prompt includes as "## <name>", or the prompt's own text.
func systemSection(text string, offset int) string {
	if offset > len(text) {
		offset = len(text)
	}
	head := text[:offset]
	at := strings.LastIndex(head, "\n## ")
	if at < 0 {
		if strings.HasPrefix(head, "## ") {
			at = -1
		} else {
			return "prompt text"
		}
	}
	line := head[at+1:]
	if end := strings.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	switch strings.TrimSpace(strings.TrimPrefix(line, "## ")) {
	case "MEMORY.md":
		return "memory overview (MEMORY.md)"
	case "AGENTS.md", "PROFILES.md":
		return "workspace file (" + strings.TrimSpace(strings.TrimPrefix(line, "## ")) + ")"
	default:
		return "prompt section (" + strings.TrimSpace(line) + ")"
	}
}

// firstDifference walks two decoded values in step and reports the JSON path of
// the first leaf that differs, the two texts there, and the byte offset where
// they diverge.
func firstDifference(path string, a, b any) (string, string, string, int) {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return path, describe(a), describe(b), 0
		}
		for _, key := range sortedKeys(av, bv) {
			ai, aok := av[key]
			bi, bok := bv[key]
			if !aok || !bok {
				return path + "." + key, describeMissing(ai, aok), describeMissing(bi, bok), 0
			}
			if !bytes.Equal(canonical(ai), canonical(bi)) {
				return firstDifference(path+"."+key, ai, bi)
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			return path, describe(a), describe(b), 0
		}
		for i := range max(len(av), len(bv)) {
			if i >= len(av) || i >= len(bv) {
				return fmt.Sprintf("%s[%d]", path, i), "", "", 0
			}
			if !bytes.Equal(canonical(av[i]), canonical(bv[i])) {
				return firstDifference(fmt.Sprintf("%s[%d]", path, i), av[i], bv[i])
			}
		}
	case string:
		if bs, ok := b.(string); ok {
			offset := commonPrefixLen([]byte(av), []byte(bs))
			for offset > 0 && !utf8.RuneStart(av[min(offset, len(av)-1)]) {
				offset--
			}
			return path, av, bs, offset
		}
	}
	return path, describe(a), describe(b), 0
}

func canonical(value any) []byte {
	out, _ := json.Marshal(value)
	return out
}

func describe(value any) string { return string(canonical(value)) }

func describeMissing(value any, present bool) string {
	if !present {
		return "(absent)"
	}
	return describe(value)
}

func sortedKeys(a, b map[string]any) []string {
	seen := map[string]struct{}{}
	var keys []string
	for _, m := range []map[string]any{a, b} {
		for key := range m {
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				keys = append(keys, key)
			}
		}
	}
	slices.Sort(keys)
	return keys
}

// excerpt shows the text around a byte offset, enough to see what changed.
func excerpt(text string, offset int) string {
	const around = 40
	start := max(offset-around, 0)
	end := min(offset+around, len(text))
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	out := text[start:end]
	if start > 0 {
		out = "..." + out
	}
	if end < len(text) {
		out += "..."
	}
	return out
}

func TestComparePromptsAppendOnlyReusesTheWholePreviousPrompt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ prev, next string }{
		"completions": {
			`{"model":"m","stream":true,"messages":[{"role":"system","content":"S"},{"role":"user","content":"u1"}],"tools":[{"type":"function","function":{"name":"t"}}]}`,
			`{"stream":true,"model":"m","tools":[ {"type":"function","function":{"name":"t"}} ],"messages":[{"content":"S","role":"system"},{"role":"user","content":"u1"},{"role":"assistant","content":"a1"}]}`,
		},
		"responses": {
			`{"model":"m","instructions":"S","input":[{"role":"user","content":"u1"}],"prompt_cache_key":"k1"}`,
			`{"model":"m","instructions":"S","input":[{"role":"user","content":"u1"},{"role":"assistant","content":"a1"}],"prompt_cache_key":"k1"}`,
		},
		"anthropic cache_control moves": {
			`{"model":"m","system":[{"type":"text","text":"S","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral"}}]}]}`,
			`{"model":"m","system":[{"type":"text","text":"S","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"u1"}]},{"role":"assistant","content":[{"type":"text","text":"a1","cache_control":{"type":"ephemeral"}}]}]}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			report, err := comparePrompts([]byte(tc.prev), []byte(tc.next))
			if err != nil {
				t.Fatal(err)
			}
			if report.Break != nil || report.Ratio() != 1 {
				t.Fatalf("report = %+v (%v), want the whole previous prompt reused", report, report.Break)
			}
		})
	}
}

func TestComparePromptsLocatesTheFirstDifference(t *testing.T) {
	t.Parallel()
	system := func(memory string) string {
		return fmt.Sprintf(`{"messages":[{"role":"system","content":%q},{"role":"user","content":"u1"}]}`,
			"intro\n\n## AGENTS.md\n\nrules\n\n## MEMORY.md\n\n"+memory)
	}
	for name, tc := range map[string]struct {
		prev, next string
		part       string
		unit       string
	}{
		"memory overview": {system("remembers A"), system("remembers B"), "system prompt: memory overview (MEMORY.md)", "messages[0]"},
		"workspace file": {
			`{"messages":[{"role":"system","content":"intro\n\n## AGENTS.md\n\nrules v1"},{"role":"user","content":"u1"}]}`,
			`{"messages":[{"role":"system","content":"intro\n\n## AGENTS.md\n\nrules v2"},{"role":"user","content":"u1"}]}`,
			"system prompt: workspace file (AGENTS.md)", "messages[0]",
		},
		"system text before any heading": {
			`{"messages":[{"role":"system","content":"time 10:00"},{"role":"user","content":"u1"}]}`,
			`{"messages":[{"role":"system","content":"time 10:01"},{"role":"user","content":"u1"}]}`,
			"system prompt: prompt text", "messages[0]",
		},
		"anthropic system blocks": {
			`{"system":[{"type":"text","text":"intro\n\n## MEMORY.md\n\nA"}],"messages":[{"role":"user","content":"u1"}]}`,
			`{"system":[{"type":"text","text":"intro\n\n## MEMORY.md\n\nB"}],"messages":[{"role":"user","content":"u1"}]}`,
			"system prompt: memory overview (MEMORY.md)", "system[0]",
		},
		"tool definitions": {
			`{"tools":[{"name":"a"}],"messages":[{"role":"user","content":"u1"}]}`,
			`{"tools":[{"name":"b"}],"messages":[{"role":"user","content":"u1"}]}`,
			"tool definitions", "tools[0]",
		},
		"history": {
			`{"messages":[{"role":"user","content":"u1"},{"role":"assistant","content":"a1"}]}`,
			`{"messages":[{"role":"user","content":"u1 edited"},{"role":"assistant","content":"a1"},{"role":"user","content":"u2"}]}`,
			"history", "messages[0]",
		},
		"current turn": {
			`{"messages":[{"role":"user","content":"u1"},{"role":"assistant","content":"a1"}]}`,
			`{"messages":[{"role":"user","content":"u1"},{"role":"assistant","content":"a1 rewritten"}]}`,
			"current turn", "messages[1]",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			report, err := comparePrompts([]byte(tc.prev), []byte(tc.next))
			if err != nil {
				t.Fatal(err)
			}
			if report.Break == nil {
				t.Fatalf("no break found, ratio %v", report.Ratio())
			}
			if report.Break.Part != tc.part || report.Break.Unit != tc.unit {
				t.Fatalf("break = %v, want %s in %s", report.Break, tc.part, tc.unit)
			}
			if report.Ratio() >= 1 {
				t.Fatalf("ratio = %v, want less than 1 past a break", report.Ratio())
			}
		})
	}
}
