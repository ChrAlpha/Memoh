package contextfrag

import (
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/toolexec"
)

// assertPartJSONBytes pins partJSONBytes to json.Marshal, the encoding the
// estimate has always measured.
func assertPartJSONBytes(t *testing.T, part sdk.MessagePart) {
	t.Helper()
	data, err := json.Marshal(part)
	got, ok := partJSONBytes(part)
	if ok != (err == nil) {
		t.Fatalf("partJSONBytes(%#v) ok = %v, json.Marshal error = %v", part, ok, err)
	}
	if ok && got != len(data) {
		t.Fatalf("partJSONBytes(%#v) = %d, json.Marshal wrote %d bytes: %s", part, got, len(data), data)
	}
}

// awkwardText covers every byte class encoding/json treats specially in a
// string: quotes, backslashes, the short escapes, other control bytes, HTML
// characters, U+2028/U+2029, invalid UTF-8, DEL and multi-byte runes.
var awkwardText = []string{
	"",
	"plain",
	`quote " and backslash \`,
	"short escapes \b\f\n\r\t",
	"control \x00\x01\x1f",
	"html <a href='x'>&amp;</a>",
	"separators \u2028 and \u2029",
	"invalid \xff\xfe utf8 \xe2\x80",
	"del \x7f",
	"multibyte 中文 ✓ 🙂",
}

var awkwardJSON = []string{
	`{}`,
	`{"command":"ls"}`,
	"{ \"spaced\" : [ 1 , 2 ,\n\t3 ] ,\r\n \"nested\" : { \"k\" : null } }",
	`{"html":"<b>&</b>","esc":"\"\\\/\b\f\n\r\tA"}`,
	"{\"sep\":\"\u2028\u2029\"}",
	"{\"bad\":\"\xff\"}",
	`"just a string"`,
	`[1,2.5e3,-0,true,false,null]`,
	` 42 `,
	`{"unterminated":`,
	`{"a":1} {"b":2}`,
	`not json`,
}

func TestPartJSONBytesMatchesJSONMarshal(t *testing.T) {
	t.Parallel()

	cache := &sdk.CacheControl{Type: "ephemeral"}
	cacheTTL := &sdk.CacheControl{Type: "ephemeral", TTL: "1h"}
	var parts []sdk.MessagePart
	for _, text := range awkwardText {
		parts = append(parts,
			sdk.ToolCallPart{ToolCallID: text, ToolName: text, Input: sdk.ToolArguments{Text: text}},
			sdk.ToolResultPart{ToolCallID: text, ToolName: text, Result: sdk.ToolOutput{Text: text}, IsError: text != ""},
			sdk.FilePart{Data: text, MediaType: text, Filename: text},
			sdk.ToolCallPart{ProviderMetadata: sdk.ProviderMetadata{text: {text: text}}},
		)
	}
	for _, raw := range awkwardJSON {
		parts = append(parts,
			sdk.ToolCallPart{ToolCallID: "c", ToolName: "exec", Input: sdk.ToolArguments{JSON: json.RawMessage(raw)}},
			sdk.ToolCallPart{ToolCallID: "c", ToolName: "exec", Input: sdk.ToolArguments{JSON: json.RawMessage(raw), Text: "both"}},
			sdk.ToolResultPart{ToolCallID: "c", ToolName: "exec", Result: sdk.ToolOutput{JSON: json.RawMessage(raw)}},
			sdk.ToolResultPart{ToolCallID: "c", ToolName: "exec", Result: sdk.ToolOutput{Text: "both", JSON: json.RawMessage(raw)}},
		)
	}
	parts = append(parts,
		sdk.ToolCallPart{},
		sdk.ToolResultPart{},
		sdk.FilePart{},
		sdk.ToolCallPart{Input: sdk.ToolArguments{JSON: json.RawMessage{}}},
		sdk.ToolResultPart{Result: sdk.ToolOutput{JSON: json.RawMessage{}}},
		sdk.ToolCallPart{ToolCallID: "c", Input: sdk.ParseToolArguments(`{"b":2,"a":"<x>"}`), CacheControl: cacheTTL},
		sdk.ToolResultPart{ToolCallID: "c", Result: toolexec.OutputFromValue(map[string]any{"lines": []string{"a", "b"}}), CacheControl: cache},
		sdk.FilePart{Data: "aGVsbG8=", CacheControl: cacheTTL},
		sdk.ToolCallPart{ProviderMetadata: sdk.ProviderMetadata{}},
		sdk.ToolCallPart{ProviderMetadata: sdk.ProviderMetadata{"memoh": nil}},
		sdk.ToolCallPart{ProviderMetadata: sdk.ProviderMetadata{"memoh": {}}},
		sdk.ToolCallPart{ProviderMetadata: sdk.ProviderMetadata{
			"memoh":     {"pending_approval": "true", "execution": "workspace"},
			"anthropic": {"signature": "sig<&>"},
		}},
		sdk.TextPart{Text: "text is measured by length, but the fallback still encodes it"},
		sdk.ImagePart{Image: "data:image/png;base64,AAAA", MediaType: "image/png"},
	)
	for _, part := range parts {
		assertPartJSONBytes(t, part)
	}
}

func TestPartJSONBytesMatchesJSONMarshalOnRandomParts(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(247, 1)) //nolint:gosec // a fixed seed keeps the generated corpus reproducible
	alphabet := []string{"a", "Z", "0", " ", "\"", "\\", "/", "\n", "\t", "\x00", "\x1f", "<", ">", "&", "\x7f", "中", "🙂", "\u2028", "\u2029", "\xff", "\xe2\x80", "{", "}", "[", "]", ":", ","}
	text := func() string {
		var b strings.Builder
		for range rng.IntN(24) {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		return b.String()
	}
	document := func() json.RawMessage {
		var value any
		switch rng.IntN(4) {
		case 0:
			value = text()
		case 1:
			value = []any{text(), rng.Float64(), rng.IntN(2) == 0, nil}
		default:
			value = map[string]any{text(): text(), "n": rng.IntN(1000), "nested": map[string]any{text(): []any{text()}}}
		}
		data, err := json.MarshalIndent(value, strings.Repeat(" ", rng.IntN(2)), strings.Repeat("\t", rng.IntN(2)))
		if err != nil {
			t.Fatal(err)
		}
		if rng.IntN(8) == 0 {
			data = data[:rng.IntN(len(data))]
		}
		return data
	}
	for range 2000 {
		var metadata sdk.ProviderMetadata
		if rng.IntN(2) == 0 {
			metadata = sdk.ProviderMetadata{text(): {text(): text(), text(): text()}}
		}
		var cache *sdk.CacheControl
		if rng.IntN(2) == 0 {
			cache = &sdk.CacheControl{Type: text(), TTL: text()}
		}
		assertPartJSONBytes(t, sdk.ToolCallPart{ToolCallID: text(), ToolName: text(), Input: sdk.ToolArguments{JSON: document(), Text: text()}, CacheControl: cache, ProviderMetadata: metadata})
		assertPartJSONBytes(t, sdk.ToolResultPart{ToolCallID: text(), ToolName: text(), Result: sdk.ToolOutput{Text: text(), JSON: document()}, IsError: rng.IntN(2) == 0, CacheControl: cache})
		assertPartJSONBytes(t, sdk.FilePart{Data: text(), MediaType: text(), Filename: text(), CacheControl: cache})
	}
}

func FuzzPartJSONBytes(f *testing.F) {
	for _, text := range awkwardText {
		f.Add(text, []byte(`{"k":"v"}`))
	}
	for _, raw := range awkwardJSON {
		f.Add("x", []byte(raw))
	}
	f.Fuzz(func(t *testing.T, text string, raw []byte) {
		assertPartJSONBytes(t, sdk.ToolCallPart{ToolCallID: text, ToolName: text, Input: sdk.ToolArguments{JSON: raw, Text: text}, ProviderMetadata: sdk.ProviderMetadata{text: {text: text}}})
		assertPartJSONBytes(t, sdk.ToolResultPart{ToolCallID: text, ToolName: text, Result: sdk.ToolOutput{Text: text, JSON: raw}, IsError: true})
		assertPartJSONBytes(t, sdk.FilePart{Data: text, MediaType: text, Filename: text, CacheControl: &sdk.CacheControl{Type: text, TTL: text}})
	})
}

// assertRawJSONBytes pins rawJSONBytes to json.Valid and to the bytes
// json.Marshal writes for an embedded document.
func assertRawJSONBytes(t *testing.T, raw []byte) {
	t.Helper()
	got, ok := rawJSONBytes(raw)
	if want := json.Valid(raw); ok != want {
		t.Fatalf("rawJSONBytes(%q) ok = %v, json.Valid = %v", raw, ok, want)
	}
	if !ok {
		return
	}
	data, err := json.Marshal(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("json.Marshal(%q): %v", raw, err)
	}
	if got != len(data) {
		t.Fatalf("rawJSONBytes(%q) = %d, json.Marshal wrote %d bytes: %s", raw, got, len(data), data)
	}
}

func TestRawJSONBytesFollowsJSONValid(t *testing.T) {
	t.Parallel()

	documents := append([]string(nil), awkwardJSON...)
	documents = append(documents,
		"", " ", "-", "-0", "01", "1.", "1.5", ".5", "1e", "1e+", "1E-7", "-12.5e+3", "2e", "0x10",
		"tru", "true", "truex", "nul", "null ", "false", "[", "]", "[1,]", "[,1]", "[1 2]", "{,}", `{"a"}`, `{"a":}`, `{"a":1,}`, `{1:2}`,
		`"\u12"`, `"\u12g4"`, `"\uABCD"`, `"\x"`, `"\`, "\"tab\tinside\"", "\"\x7f\"", "\"\xe2\x80\xa8\"", "\"\xe2\x80\"",
		strings.Repeat("[", 10000)+strings.Repeat("]", 10000),
		strings.Repeat("[", 10001)+strings.Repeat("]", 10001),
		strings.Repeat(`{"a":`, 10000)+"1"+strings.Repeat("}", 10000),
		strings.Repeat(`{"a":`, 10001)+"1"+strings.Repeat("}", 10001),
	)
	for _, document := range documents {
		assertRawJSONBytes(t, []byte(document))
	}
}

func FuzzRawJSONBytes(f *testing.F) {
	for _, raw := range awkwardJSON {
		f.Add([]byte(raw))
	}
	f.Add([]byte(`{"a":[1,-2.5e+3,true,false,null,"<\u00e9>"]}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		assertRawJSONBytes(t, raw)
	})
}

// TestPartJSONBytesKnowsEveryMeasuredField fails when a Twilight upgrade
// changes a part shape partJSONBytes measures field by field; the
// measurement must then follow the new wire shape.
func TestPartJSONBytesKnowsEveryMeasuredField(t *testing.T) {
	t.Parallel()

	want := map[reflect.Type][]string{
		reflect.TypeFor[sdk.ToolCallPart]():   {`ToolCallID string json:"toolCallId"`, `ToolName string json:"toolName"`, `Input sdk.ToolArguments json:"input"`, `CacheControl *sdk.CacheControl json:"cacheControl,omitempty"`, `ProviderMetadata sdk.ProviderMetadata json:"providerMetadata,omitempty"`},
		reflect.TypeFor[sdk.ToolResultPart](): {`ToolCallID string json:"toolCallId"`, `ToolName string json:"toolName"`, `Result sdk.ToolOutput json:"result"`, `IsError bool json:"isError,omitempty"`, `CacheControl *sdk.CacheControl json:"cacheControl,omitempty"`},
		reflect.TypeFor[sdk.FilePart]():       {`Data string json:"data"`, `MediaType string json:"mediaType,omitempty"`, `Filename string json:"filename,omitempty"`, `CacheControl *sdk.CacheControl json:"cacheControl,omitempty"`},
		reflect.TypeFor[sdk.ToolArguments]():  {`JSON json.RawMessage json:"json,omitempty"`, `Text string json:"text,omitempty"`},
		reflect.TypeFor[sdk.ToolOutput]():     {`Text string json:"text,omitempty"`, `JSON json.RawMessage json:"json,omitempty"`},
		reflect.TypeFor[sdk.CacheControl]():   {`Type string json:"type"`, `TTL string json:"ttl,omitempty"`},
	}
	for typ, fields := range want {
		var got []string
		for field := range typ.Fields() {
			got = append(got, field.Name+" "+field.Type.String()+` json:"`+field.Tag.Get("json")+`"`)
		}
		if !reflect.DeepEqual(got, fields) {
			t.Errorf("%s fields = %q, partJSONBytes measures %q", typ, got, fields)
		}
		if typ.Implements(reflect.TypeFor[json.Marshaler]()) || reflect.PointerTo(typ).Implements(reflect.TypeFor[json.Marshaler]()) {
			t.Errorf("%s now encodes itself; partJSONBytes measures its fields", typ)
		}
	}
	if typ := reflect.TypeFor[sdk.ProviderMetadata](); typ.String() != "sdk.ProviderMetadata" || typ.Kind() != reflect.Map || typ.Elem().Kind() != reflect.Map || typ.Elem().Elem().Kind() != reflect.String ||
		typ.Implements(reflect.TypeFor[json.Marshaler]()) {
		t.Errorf("sdk.ProviderMetadata changed shape: %s", typ)
	}
}

func BenchmarkSDKMessageEstimateToolResult(b *testing.B) {
	output := toolexec.OutputFromValue(map[string]any{"stdout": strings.Repeat("output <line> & more\n", 4000), "exit_code": 0})
	message := sdk.ToolMessage(sdk.ToolResultPart{ToolCallID: "call", ToolName: "exec", Result: output})
	b.ReportAllocs()
	for b.Loop() {
		EstimateSDKMessageTokens(message)
	}
}
