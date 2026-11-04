package contextfrag

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/toolexec"
)

// EstimateBytesPerToken is the byte-per-token heuristic shared by every
// context ledger (selection budgets, compaction triggers, manifest
// accounting), and the single swap point for a real tokenizer.
const EstimateBytesPerToken = 4

const (
	// ProviderBudgetEstimator identifies the conservative estimator used only
	// for provider-envelope decisions.
	ProviderBudgetEstimator = "bytes_ceil_div4_margin_v1"
	// ProviderBudgetSafetyFactorPercent adds 25 percent headroom after byte
	// ceiling. The midpoint of the accepted 15–30 percent range covers common
	// multilingual and JSON density without changing legacy ledger estimates.
	ProviderBudgetSafetyFactorPercent = 125
)

// EstimateImageTokens is the flat per-image estimate. Real image cost is
// resolution-dependent and provider-capped at roughly 1.1–1.6K tokens, so a
// ceiling-magnitude flat figure keeps image-heavy history visible to budget
// pressure without counting base64 payload bytes as text.
const EstimateImageTokens = 1500

// TokensFromBytes converts a byte count to the shared token estimate.
func TokensFromBytes(n int) int {
	if n <= 0 {
		return 0
	}
	return n / EstimateBytesPerToken
}

func BudgetBytesForTokens(tokens int) int64 {
	return int64(max(0, tokens)) * EstimateBytesPerToken
}

// ProviderBudgetBytesForTokens is the most bytes ProviderBudgetTokensFromBytes
// prices within tokens: the byte budget of a provider-envelope decision.
func ProviderBudgetBytesForTokens(tokens int) int64 {
	return int64(max(0, tokens)) * 100 / ProviderBudgetSafetyFactorPercent * EstimateBytesPerToken
}

// ProviderBudgetTokensFromBytes converts bytes for provider-envelope decisions,
// including selection under a provider budget and the summarizer request.
// Compaction pressure, cache metrics, and other ledger consumers keep the
// legacy floor-based TokensFromBytes contract.
func ProviderBudgetTokensFromBytes(n int) int {
	if n <= 0 {
		return 0
	}
	ceiling := n / EstimateBytesPerToken
	if n%EstimateBytesPerToken != 0 {
		ceiling++
	}
	return ceiling * ProviderBudgetSafetyFactorPercent / 100
}

// EstimateSDKMessageTokens estimates tokens for one SDK message additively
// across all parts: text and reasoning count their raw bytes, tool calls,
// tool results and files count their serialized size, and each image counts
// the flat EstimateImageTokens instead of its base64 payload.
func EstimateSDKMessageTokens(msg sdk.Message) int {
	bytes, images := sdkMessageEstimate(msg)
	return TokensFromBytes(bytes) + images*EstimateImageTokens
}

func sdkMessageEstimate(msg sdk.Message) (bytes, images int) {
	for _, part := range msg.Content {
		switch p := part.(type) {
		case sdk.TextPart:
			bytes += len(p.Text)
		case sdk.ReasoningPart:
			bytes += len(p.Text)
		case sdk.ImagePart:
			images++
		case sdk.ToolCallPart:
			bytes += len(`{"toolCallId":,"toolName":,"input":{}}`) + jsonStringBytes(p.ToolCallID) +
				jsonStringBytes(p.ToolName) + jsonPayloadBytes(p.Input.Text, p.Input.JSON) +
				jsonCacheControlBytes(p.CacheControl) + jsonMetadataBytes(p.ProviderMetadata)
		case sdk.ToolResultPart:
			bytes += len(`{"toolCallId":,"toolName":,"result":{}}`) + jsonStringBytes(p.ToolCallID) +
				jsonStringBytes(p.ToolName) + jsonPayloadBytes(p.Result.Text, p.Result.JSON) +
				jsonCacheControlBytes(p.CacheControl)
			if p.IsError {
				bytes += len(`,"isError":true`)
			}
		case sdk.FilePart:
			bytes += len(`{"data":}`) + jsonStringBytes(p.Data) + jsonCacheControlBytes(p.CacheControl)
			if p.MediaType != "" {
				bytes += len(`,"mediaType":`) + jsonStringBytes(p.MediaType)
			}
			if p.Filename != "" {
				bytes += len(`,"filename":`) + jsonStringBytes(p.Filename)
			}
		default:
			if data, err := json.Marshal(part); err == nil {
				bytes += len(data)
			}
		}
	}
	return bytes, images
}

// The sizes below are what json.Marshal writes for a tool exchange or file,
// computed without encoding it: the estimate prices those parts on every
// pass, and they carry most of a context's bytes. They count every escape
// encoding/json writes, so markup and binary output keep their price. Tool
// arguments and outputs keep their JSON canonical; whitespace a non-canonical
// document carries, which encoding/json compacts away, is priced too.

// jsonPayloadBytes sizes the "text" and "json" members of tool arguments or
// output, each omitted when empty.
func jsonPayloadBytes(text string, raw []byte) int {
	n := 0
	if text != "" {
		n += len(`"text":`) + jsonStringBytes(text)
	}
	if len(raw) > 0 {
		if n > 0 {
			n++
		}
		n += len(`"json":`) + jsonRawBytes(raw)
	}
	return n
}

// jsonCacheControlBytes sizes a part's "cacheControl" member, omitted when
// unset.
func jsonCacheControlBytes(cache *sdk.CacheControl) int {
	if cache == nil {
		return 0
	}
	n := len(`,"cacheControl":{"type":}`) + jsonStringBytes(cache.Type)
	if cache.TTL != "" {
		n += len(`,"ttl":`) + jsonStringBytes(cache.TTL)
	}
	return n
}

// jsonMetadataBytes sizes a part's "providerMetadata" member, omitted when
// empty.
func jsonMetadataBytes(metadata sdk.ProviderMetadata) int {
	if len(metadata) == 0 {
		return 0
	}
	n := len(`,"providerMetadata":{}`) + len(metadata) - 1
	for namespace, values := range metadata {
		n += jsonStringBytes(namespace) + len(`:`)
		if values == nil {
			n += len(`null`)
			continue
		}
		n += len(`{}`) + max(len(values)-1, 0)
		for key, value := range values {
			n += jsonStringBytes(key) + len(`:`) + jsonStringBytes(value)
		}
	}
	return n
}

// jsonStringBytes is the length of s encoded as a JSON string.
func jsonStringBytes(s string) int {
	n := len(s) + len(`""`)
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			n += int(jsonEscapeGrowth[c])
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			// An invalid byte is replaced by a six-byte escape.
			n += len(`\ufffd`) - 1
		case r == '\u2028' || r == '\u2029':
			// Line and paragraph separators grow from three bytes to a
			// six-byte escape.
			n += len(`\u2028`) - size
		}
		i += size
	}
	return n
}

// jsonEscapeGrowth is how many bytes encoding/json adds when it escapes an
// ASCII byte inside a string.
var jsonEscapeGrowth = func() (growth [utf8.RuneSelf]uint8) {
	for c := range 0x20 {
		growth[c] = uint8(len(`\u0000`) - 1)
	}
	for _, c := range []byte{'"', '\\', '\n', '\r', '\t', '\b', '\f'} {
		growth[c] = 1
	}
	for _, c := range []byte{'<', '>', '&'} {
		growth[c] = uint8(len(`\u0000`) - 1)
	}
	return growth
}()

// jsonRawBytes is the length of an embedded canonical JSON document as
// encoding/json writes it: the HTML characters and the line and paragraph
// separators it escapes grow it.
func jsonRawBytes(raw []byte) int {
	html := bytes.Count(raw, []byte{'<'}) + bytes.Count(raw, []byte{'>'}) + bytes.Count(raw, []byte{'&'})
	separators := bytes.Count(raw, []byte("\u2028")) + bytes.Count(raw, []byte("\u2029"))
	return len(raw) + html*(len(`\u0000`)-1) + separators*(len(`\u2028`)-len("\u2028"))
}

// EstimateFragTokens computes the token estimate from the fragment's parts,
// ignoring any preset TokenEstimate.
func EstimateFragTokens(frag ContextFrag) int {
	bytes, images := fragEstimate(frag)
	return TokensFromBytes(bytes) + images*EstimateImageTokens
}

func fragEstimate(frag ContextFrag) (bytes, images int) {
	for _, part := range frag.Parts {
		switch part.Type {
		case PartText:
			bytes += len(part.Text)
		case PartSDKMessage:
			if msg := partMessage(part); msg != nil {
				msgBytes, msgImages := sdkMessageEstimate(*msg)
				bytes += msgBytes
				images += msgImages
			}
		case PartImage:
			images++
		}
	}
	return bytes, images
}

// ResolveFragTokens returns the fragment's authoritative token estimate:
// the collector-provided TokenEstimate when set (which may carry real
// provider usage), otherwise the computed part estimate.
func ResolveFragTokens(frag ContextFrag) int {
	if frag.TokenEstimate > 0 {
		return frag.TokenEstimate
	}
	return EstimateFragTokens(frag)
}

// ResolveProviderBudgetFragTokens keeps an authoritative fragment estimate when
// it is larger, otherwise it uses the conservative provider byte estimate.
func ResolveProviderBudgetFragTokens(frag ContextFrag) int {
	bytes, images := fragEstimate(frag)
	estimated := ProviderBudgetTokensFromBytes(bytes) + images*EstimateImageTokens
	if frag.TokenEstimate > estimated {
		return frag.TokenEstimate
	}
	return estimated
}

// ProviderEnvelopeTokens prices one provider payload for envelope decisions
// with the estimator that selection applies per fragment, so a single-message
// fragment costs the same whether it is being selected or has already been
// frozen into a prefix. Tools are the provider-neutral definitions the request
// actually carries, not the local executable tool set.
func ProviderEnvelopeTokens(system string, messages []sdk.Message, tools []sdk.ToolDefinition) int {
	return ProviderFixedEnvelopeTokens(system, tools) + ProviderMessagesTokens(messages)
}

// ProviderFixedEnvelopeTokens prices the part of a provider payload that a
// change to its messages leaves alone: the system prompt and the tools.
func ProviderFixedEnvelopeTokens(system string, tools []sdk.ToolDefinition) int {
	total := ProviderBudgetTokensFromBytes(len(system))
	for _, tool := range tools {
		total += ProviderToolDefTokens(ToolDefinitionAccountingFor("", tool))
	}
	return total
}

// ProviderMessagesTokens prices provider messages for envelope decisions.
func ProviderMessagesTokens(messages []sdk.Message) int {
	total := 0
	for _, message := range messages {
		total += ProviderMessageTokens(message)
	}
	return total
}

// ProviderMessageTokens prices one provider message for envelope decisions; a
// payload prices at the sum of its messages.
func ProviderMessageTokens(message sdk.Message) int {
	bytes, images := sdkMessageEstimate(message)
	return ProviderBudgetTokensFromBytes(bytes) + images*EstimateImageTokens
}

// ProviderToolDefTokens prices one tool definition for envelope decisions.
func ProviderToolDefTokens(def ToolDefAccounting) int {
	return max(def.TokenEstimate, ProviderBudgetTokensFromBytes(def.Bytes))
}

// ToolDefAccountingFor measures one local executable tool as the provider will
// receive it (name, description, parameter schema). A definition that fails to
// serialize falls back to its visible prose size.
func ToolDefAccountingFor(provider string, tool toolexec.Tool) ToolDefAccounting {
	size := len(tool.Name) + len(tool.Description)
	if data, err := json.Marshal(tool); err == nil {
		size = len(data)
	}
	return ToolDefAccounting{
		Provider:      provider,
		Name:          tool.Name,
		Bytes:         size,
		TokenEstimate: TokensFromBytes(size),
	}
}

// ToolDefinitionAccountingFor measures one provider-bound tool definition as it
// travels on the wire. It is the ToolDefAccountingFor counterpart for the
// sdk.Request tools shape.
func ToolDefinitionAccountingFor(provider string, tool sdk.ToolDefinition) ToolDefAccounting {
	size := len(tool.Name) + len(tool.Description)
	if data, err := json.Marshal(tool); err == nil {
		size = len(data)
	}
	return ToolDefAccounting{
		Provider:      provider,
		Name:          tool.Name,
		Bytes:         size,
		TokenEstimate: TokensFromBytes(size),
	}
}
