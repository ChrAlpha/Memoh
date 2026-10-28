package models

import (
	"slices"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/toolexec"
)

// Prompt cache TTL options accepted in Provider config.
//
// The values are vendor-neutral; ApplyPromptCache dispatches to the
// vendor-specific decoration based on the resolved client type. Today only
// Anthropic Messages implements caching, but the public API stays stable
// when other vendors gain similar support.
const (
	// PromptCacheTTL5m enables prompt caching with a short (~5 minute)
	// TTL. Recommended default.
	PromptCacheTTL5m = "5m"
	// PromptCacheTTL1h enables prompt caching with a 1-hour TTL. Some
	// vendors (e.g. Anthropic) bill 1h cache writes at a higher rate
	// than the default short TTL.
	PromptCacheTTL1h = "1h"
	// PromptCacheTTLOff disables prompt caching for the provider. Not
	// recommended: every request rebuilds the prefix without cache.
	PromptCacheTTLOff = "off"
)

// DefaultPromptCacheTTL is the value used when a provider does not
// explicitly configure a cache policy.
const DefaultPromptCacheTTL = PromptCacheTTL5m

// MinCacheablePrefixTokens is the floor below which a message-level cache
// breakpoint is withheld: Anthropic ignores breakpoints on prefixes under
// its per-model minimum (1024–4096 real tokens), and our byte-heuristic
// estimate undercounts CJK text, so the floor sits far below the provider
// minimum to never withhold a viable breakpoint — it only stops the
// bookkeeping from claiming a cached prefix the provider provably ignored.
const MinCacheablePrefixTokens = 256

// NormalizePromptCacheTTL coerces an arbitrary user-provided value to one
// of the accepted TTL constants. Empty or unrecognized values fall back to
// the recommended short-TTL default.
func NormalizePromptCacheTTL(s string) string {
	switch s {
	case PromptCacheTTL1h:
		return PromptCacheTTL1h
	case PromptCacheTTLOff:
		return PromptCacheTTLOff
	default:
		return DefaultPromptCacheTTL
	}
}

// ApplyPromptCache returns a request payload decorated with provider-specific
// prompt cache breakpoints. The dispatch is keyed off the resolved client
// type, so the call site does not need to know which vendor (if any) supports
// caching for the active model.
//
// For models whose vendor does not implement caching, or when the requested
// TTL is "off", the inputs are returned unchanged.
func ApplyPromptCache(
	model *sdk.Model,
	ttl string,
	system string,
	messages []sdk.Message,
	tools []toolexec.Tool,
) (string, []sdk.Message, []toolexec.Tool) {
	layout := ApplyPromptCacheWithPlan(model, ttl, contextfrag.CachePlan{}, system, messages, tools)
	return layout.System, layout.Messages, layout.Tools
}

// PromptCacheLayout is a request decorated with prompt cache breakpoints, and
// what a run needs to keep them right while it appends steps.
type PromptCacheLayout struct {
	System   string
	Messages []sdk.Message
	Tools    []toolexec.Tool
	// SystemPrepended reports that the decoration moved the system prompt
	// into Messages[0] (Anthropic's system->message cache promotion), so
	// callers can tell it apart from an unrelated leading system-role
	// message already present in the input.
	SystemPrepended bool
	// StableMessageCount is the honest count of leading input messages
	// covered by an applied message-level breakpoint: it matches the plan
	// unless placement had to fall back to an earlier message (or found
	// none), so callers keep their bookkeeping of the cached prefix truthful
	// instead of trusting a claim that was never actually cached.
	StableMessageCount int

	// cc is the breakpoint a run's calls place; nil when the vendor caches
	// without breakpoints or caching is off.
	cc *sdk.CacheControl
	// head is 1 when Messages[0] is the promoted system message.
	head int
	// prefix is the number of leading Messages the plan's breakpoints
	// decorate: the head through the stable breakpoint.
	prefix int
}

// ApplyPromptCacheWithPlan decorates the provider request using the
// placement-derived cache plan: when the plan marks a stable leading message
// span, the final message of that span receives a cache breakpoint so the
// stable prefix is cached across turns. A zero plan decorates the system
// prompt and tools only.
func ApplyPromptCacheWithPlan(
	model *sdk.Model,
	ttl string,
	plan contextfrag.CachePlan,
	system string,
	messages []sdk.Message,
	tools []toolexec.Tool,
) PromptCacheLayout {
	layout := PromptCacheLayout{System: system, Messages: messages, Tools: tools, StableMessageCount: plan.StableMessageCount}
	if model == nil {
		return layout
	}
	normalized := NormalizePromptCacheTTL(ttl)
	if normalized == PromptCacheTTLOff {
		return layout
	}
	// OpenAI-family vendors identify cache-warm backends via a
	// prompt_cache_key on the request rather than explicit breakpoints; that
	// key comes from PromptCacheKey and rides the request options, so the
	// payload itself stays untouched in the default branch below.
	switch ResolveClientType(model) {
	case string(ClientTypeAnthropicMessages):
		return applyAnthropicPromptCache(anthropicCacheControl(normalized), plan, layout)
	default:
		return layout
	}
}

// PromptCacheKey returns the per-session cache-routing key for OpenAI-family
// clients. prompt_cache_key groups requests that share a prefix lineage onto
// cache-warm backends, and the session is exactly that lineage; providers that
// cache via explicit breakpoints get no key.
func PromptCacheKey(model *sdk.Model, ttl, sessionID string) string {
	if model == nil || sessionID == "" || NormalizePromptCacheTTL(ttl) == PromptCacheTTLOff {
		return ""
	}
	switch ResolveClientType(model) {
	case string(ClientTypeOpenAIResponses), string(ClientTypeOpenAICompletions):
		return "memoh-session-" + sessionID
	default:
		return ""
	}
}

// applyAnthropicPromptCache decorates the request with Anthropic's
// cache_control breakpoints. Anthropic allows four per request and orders the
// cached prefix tools, system, messages, so one breakpoint at the end of the
// system prompt caches the tool list with it:
//
//   - The system prompt is moved out of the dedicated `system` parameter
//     into the leading message slot as a SystemMessage with cache_control
//     on its TextPart, because Twilight's WithSystem accepts only a plain
//     string and does not propagate cache_control metadata.
//   - Without a system prompt, the final tool definition receives the
//     breakpoint instead, which caches the entire tool list.
//   - The plan's stable history span ends in a breakpoint, and a span long
//     enough gets a second one at its midpoint.
//
// That leaves one breakpoint for a run's own calls; see AdvanceRun. The
// layout owns every breakpoint of the request: any the input messages carry
// (a forked run inherits its parent's) is cleared first, so the request
// never exceeds the four Anthropic accepts.
func applyAnthropicPromptCache(cc *sdk.CacheControl, plan contextfrag.CachePlan, layout PromptCacheLayout) PromptCacheLayout {
	messages := withoutCacheControls(layout.Messages)
	marked := 0
	belowMinimum := plan.StablePrefixTokenEstimate > 0 && plan.StablePrefixTokenEstimate < MinCacheablePrefixTokens
	if plan.StableMessageCount > 0 && plan.StableMessageCount <= len(messages) {
		layout.StableMessageCount = 0
		if !belowMinimum {
			if plan.MidStableMessageCount > 0 && plan.MidStableMessageCount < plan.StableMessageCount {
				messages, _ = withMessageCacheBreakpoint(messages, 0, plan.MidStableMessageCount-1, cc)
			}
			var breakpointIndex int
			messages, breakpointIndex = withMessageCacheBreakpoint(messages, 0, plan.StableMessageCount-1, cc)
			marked = breakpointIndex + 1
			layout.StableMessageCount = marked
		}
	}

	layout.cc = cc
	layout.Messages = messages
	layout.prefix = marked
	if layout.System != "" {
		systemMsg := sdk.Message{
			Role: sdk.MessageRoleSystem,
			Content: []sdk.MessagePart{
				sdk.TextPart{Text: layout.System, CacheControl: cc},
			},
		}
		layout.Messages = make([]sdk.Message, 0, len(messages)+1)
		layout.Messages = append(layout.Messages, systemMsg)
		layout.Messages = append(layout.Messages, messages...)
		layout.System = ""
		layout.SystemPrepended = true
		layout.head = 1
		layout.prefix++
		return layout
	}

	if len(layout.Tools) > 0 {
		tools := make([]toolexec.Tool, len(layout.Tools))
		copy(tools, layout.Tools)
		tools[len(tools)-1].CacheControl = cc
		layout.Tools = tools
	}
	return layout
}

// AdvanceRun returns messages, the request of one call of the run the layout
// was built for, with the run's rolling breakpoint on the last message before
// end that can carry one. Anthropic writes cache entries only at breakpoints,
// so without it no step of a run is ever cached and every call re-reads the
// run's earlier steps at full price. messages[end:] are the loop's per-call
// carriers, rebuilt every call, which the breakpoint must not cover.
//
// The breakpoint the previous call placed stays where it is: it names the
// entry that call wrote, so the read does not depend on Anthropic's 20-block
// lookback however many blocks a step appended. It takes over the slot of the
// mid-span breakpoint, which only matters to the first call of a dispatch,
// so a request carries four breakpoints at most. Any older rolling
// breakpoint is cleared.
func (l PromptCacheLayout) AdvanceRun(messages []sdk.Message, end int) []sdk.Message {
	if l.cc == nil {
		return messages
	}
	end = min(end, len(messages))
	out, tail := withMessageCacheBreakpoint(messages, l.prefix, end-1, l.cc)
	if tail < 0 {
		return messages
	}
	previous := -1
	for i := len(out) - 1; i >= l.prefix; i-- {
		if i == tail || !hasCacheControl(out[i].Content) {
			continue
		}
		if i < tail && previous < 0 {
			previous = i
			continue
		}
		out[i].Content = clearedCacheControls(out[i].Content)
	}
	if previous >= 0 {
		for i := l.head; i < l.prefix-1; i++ {
			out[i].Content = clearedCacheControls(out[i].Content)
		}
	}
	return out
}

// withMessageCacheBreakpoint sets a cache_control breakpoint on the last
// content part of messages[index], or — if that message cannot carry one on
// the wire — scans back to the nearest earlier message in [floor, index] that
// can. A message cannot when it ends in reasoning, which Anthropic does not
// let a breakpoint sit on, or when it is a system message inside the
// conversation, which Twilight rewrites into tagged user text without the
// breakpoint. Placement must not silently drop the breakpoint, since that
// would leave the caller believing the declared prefix is cached when
// Anthropic never cached anything. Returns the updated messages and the index
// that received the breakpoint, or -1 if no message in [floor, index]
// qualifies.
func withMessageCacheBreakpoint(messages []sdk.Message, floor, index int, cc *sdk.CacheControl) ([]sdk.Message, int) {
	out := make([]sdk.Message, len(messages))
	copy(out, messages)
	conversation := slices.IndexFunc(messages, func(message sdk.Message) bool { return message.Role != sdk.MessageRoleSystem })
	for i := index; i >= floor; i-- {
		if conversation >= 0 && i > conversation && out[i].Role == sdk.MessageRoleSystem {
			continue
		}
		if content, ok := withCacheControl(out[i].Content, cc); ok {
			out[i].Content = content
			return out, i
		}
	}
	return out, -1
}

// withCacheControl returns a copy of content with cc on its last part,
// reporting false when that part cannot carry a breakpoint.
func withCacheControl(content []sdk.MessagePart, cc *sdk.CacheControl) ([]sdk.MessagePart, bool) {
	if len(content) == 0 {
		return content, false
	}
	last, ok := partWithCacheControl(content[len(content)-1], cc)
	if !ok {
		return content, false
	}
	out := make([]sdk.MessagePart, len(content))
	copy(out, content)
	out[len(out)-1] = last
	return out, true
}

func hasCacheControl(content []sdk.MessagePart) bool {
	for _, part := range content {
		if partCacheControl(part) != nil {
			return true
		}
	}
	return false
}

// clearedCacheControls returns content without breakpoints, copied only when
// it carried one.
func clearedCacheControls(content []sdk.MessagePart) []sdk.MessagePart {
	if !hasCacheControl(content) {
		return content
	}
	out := make([]sdk.MessagePart, len(content))
	for i, part := range content {
		out[i], _ = partWithCacheControl(part, nil)
	}
	return out
}

// withoutCacheControls returns messages without breakpoints, copied only when
// one carried any.
func withoutCacheControls(messages []sdk.Message) []sdk.Message {
	if !slices.ContainsFunc(messages, func(message sdk.Message) bool { return hasCacheControl(message.Content) }) {
		return messages
	}
	out := make([]sdk.Message, len(messages))
	copy(out, messages)
	ClearCacheControls(out)
	return out
}

// ClearCacheControls removes every prompt cache breakpoint from messages in
// place. Callers that compare, store or re-dispatch a request use it on their
// own copy, since a breakpoint says where the request was cached, not what it
// says.
func ClearCacheControls(messages []sdk.Message) {
	for i := range messages {
		messages[i].Content = clearedCacheControls(messages[i].Content)
	}
}

func partWithCacheControl(part sdk.MessagePart, cc *sdk.CacheControl) (sdk.MessagePart, bool) {
	switch p := part.(type) {
	case sdk.TextPart:
		p.CacheControl = cc
		return p, true
	case sdk.ImagePart:
		p.CacheControl = cc
		return p, true
	case sdk.FilePart:
		p.CacheControl = cc
		return p, true
	case sdk.ToolCallPart:
		p.CacheControl = cc
		return p, true
	case sdk.ToolResultPart:
		p.CacheControl = cc
		return p, true
	default:
		return part, false
	}
}

func partCacheControl(part sdk.MessagePart) *sdk.CacheControl {
	switch p := part.(type) {
	case sdk.TextPart:
		return p.CacheControl
	case sdk.ImagePart:
		return p.CacheControl
	case sdk.FilePart:
		return p.CacheControl
	case sdk.ToolCallPart:
		return p.CacheControl
	case sdk.ToolResultPart:
		return p.CacheControl
	default:
		return nil
	}
}

// anthropicCacheControl returns the SDK cache_control payload for Anthropic
// Messages requests for the given normalized TTL.
func anthropicCacheControl(ttl string) *sdk.CacheControl {
	switch ttl {
	case PromptCacheTTL1h:
		return &sdk.CacheControl{Type: "ephemeral", TTL: "1h"}
	case PromptCacheTTLOff:
		return nil
	default:
		return &sdk.CacheControl{Type: "ephemeral"}
	}
}
