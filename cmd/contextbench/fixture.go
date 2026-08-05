package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"reflect"
	"slices"
	"sort"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
)

const fixtureSeed int64 = 0x434f4e54455854

type benchFixture struct {
	systemFrags []contextfrag.ContextFrag
	sourceFrags []contextfrag.ContextFrag
	messages    []sdk.Message
	tools       []sdk.Tool
	toolDefs    []contextfrag.ToolDefAccounting
	requiredIDs []string
	systemIDs   []string
}

type providerPayload struct {
	System   string        `json:"system"`
	Messages []sdk.Message `json:"messages"`
	Tools    []sdk.Tool    `json:"tools,omitempty"`
}

func buildS1Fixture() benchFixture {
	rng := rand.New(rand.NewSource(fixtureSeed)) //nolint:gosec // a fixed seed is the benchmark contract
	sections := []agentpkg.SystemSection{
		section("system.prompt.intro", contextfrag.KindSystemPrompt, 10, contextfrag.RetentionRequired, 0, seededText(rng, 900, false)),
		section("system.bot_identity", contextfrag.KindBotIdentity, 20, contextfrag.RetentionPreferred, 0, seededText(rng, 420, true)),
		section("system.prompt.body", contextfrag.KindSystemPrompt, 30, contextfrag.RetentionRequired, 0, seededText(rng, 2_600, false)),
		section("system.prompt.tail", contextfrag.KindSystemPrompt, 50, contextfrag.RetentionRequired, 0, seededText(rng, 1_400, false)),
	}
	toolRender := contextfrag.RenderPolicy{Format: contextfrag.RenderMarkdown, GroupID: "system.tool_usage", GroupJoiner: "\n\n"}
	sections = append(sections, agentpkg.SystemSection{
		ID: "system.tool_usage.header", Kind: contextfrag.KindToolUsage, Priority: 45,
		RetentionTier: contextfrag.RetentionPreferred, Text: "## Tool usage", Render: toolRender,
	})
	for i := range 10 {
		sections = append(sections, agentpkg.SystemSection{
			ID: "system.tool_usage.provider-" + twoDigits(i), Kind: contextfrag.KindToolUsage, Priority: 45,
			RetentionTier: contextfrag.RetentionPreferred, DropPriority: contextfrag.DropPriority(rng.Intn(3)),
			Text: seededText(rng, 260+rng.Intn(641), i%3 == 0), Render: toolRender,
		})
	}
	identityRender := contextfrag.RenderPolicy{Format: contextfrag.RenderMarkdown, GroupID: "system.platform_identity", GroupJoiner: "\n"}
	sections = append(sections, agentpkg.SystemSection{
		ID: "system.platform_identity.header", Kind: contextfrag.KindPlatformIdentity, Priority: 60,
		RetentionTier: contextfrag.RetentionPreferred, Text: "## Connected identities", Render: identityRender,
	})
	for i := range 8 {
		sections = append(sections, agentpkg.SystemSection{
			ID: "system.platform_identity.identity-" + twoDigits(i), Kind: contextfrag.KindPlatformIdentity, Priority: 60,
			RetentionTier: contextfrag.RetentionPreferred, DropPriority: contextfrag.DropPriority(rng.Intn(3)),
			Text: seededText(rng, 180+rng.Intn(421), i%3 == 1), Render: identityRender,
		})
	}
	skillRender := contextfrag.RenderPolicy{Format: contextfrag.RenderMarkdown, GroupID: "system.skills", GroupJoiner: "\n"}
	sections = append(sections, agentpkg.SystemSection{
		ID: "system.skills.header", Kind: contextfrag.KindSkillsCatalog, Priority: 65,
		RetentionTier: contextfrag.RetentionOptional, RequiredCapability: "use_skill",
		Text: "## Available skills (40)", Render: skillRender,
	})
	for i := range 40 {
		sections = append(sections, agentpkg.SystemSection{
			ID: "system.skill.skill-" + twoDigits(i), Kind: contextfrag.KindSkillsCatalog, Priority: 65,
			RetentionTier: contextfrag.RetentionOptional, DropPriority: contextfrag.DropPriority(rng.Intn(5)),
			RequiredCapability: "use_skill", Text: seededText(rng, 50+rng.Intn(1_951), i < 12 || i%7 == 0),
			Render: skillRender,
		})
	}
	for i := range 6 {
		sections = append(sections, agentpkg.SystemSection{
			ID: "system.workspace_file.file-" + twoDigits(i), Kind: contextfrag.KindWorkspaceInstruction, Priority: 70,
			RetentionTier: contextfrag.RetentionPreferred, DropPriority: contextfrag.DropPriority(rng.Intn(4)),
			Text: seededText(rng, 1_024+rng.Intn(19*1_024+1), i%2 == 0),
		})
	}

	systemFrags := agentpkg.SystemSectionFrags(sections, contextfrag.Scope{BotID: "contextbench"})
	messages := make([]sdk.Message, 0, 31)
	messageFrags := make([]contextfrag.ContextFrag, 0, 31)
	for i := range 30 {
		text := seededText(rng, 180+rng.Intn(921), i%5 == 0)
		msg := sdk.UserMessage(text)
		trust := contextfrag.TrustExternal
		if i%2 == 1 {
			msg = sdk.AssistantMessage(text)
			trust = contextfrag.TrustWorkspace
		}
		messages = append(messages, msg)
		messageFrags = append(messageFrags, estimatedMessageFrag("message."+threeDigits(i), msg, contextfrag.KindConversationEvent, contextfrag.SlotHistory, trust, i))
	}
	current := sdk.UserMessage("Compare the retained context precisely and explain any omitted material.")
	messages = append(messages, current)
	currentFrag := estimatedMessageFrag("message.current", current, contextfrag.KindCurrentUserMessage, contextfrag.SlotCurrentUser, contextfrag.TrustUser, len(messages)-1)
	currentFrag.CacheClass = contextfrag.CacheNever
	currentFrag.Budget.Overflow = contextfrag.OverflowKeep
	messageFrags = append(messageFrags, currentFrag)

	tools := realisticTools()
	toolDefs := make([]contextfrag.ToolDefAccounting, 0, len(tools))
	for _, tool := range tools {
		toolDefs = append(toolDefs, contextfrag.ToolDefAccountingFor("contextbench", tool))
	}
	sourceFrags := append(slices.Clone(systemFrags), messageFrags...)
	requiredIDs := make([]string, 0, 4)
	systemIDs := make([]string, 0, len(systemFrags))
	for _, frag := range systemFrags {
		systemIDs = append(systemIDs, frag.ID)
		if frag.RetentionTier == contextfrag.RetentionRequired {
			requiredIDs = append(requiredIDs, frag.ID)
		}
	}
	return benchFixture{
		systemFrags: systemFrags, sourceFrags: sourceFrags, messages: messages,
		tools: tools, toolDefs: toolDefs, requiredIDs: requiredIDs, systemIDs: systemIDs,
	}
}

func section(id string, kind contextfrag.Kind, priority int, retention contextfrag.RetentionTier, drop contextfrag.DropPriority, text string) agentpkg.SystemSection {
	return agentpkg.SystemSection{ID: id, Kind: kind, Priority: priority, RetentionTier: retention, DropPriority: drop, Text: text}
}

func realisticTools() []sdk.Tool {
	names := []string{"use_skill", "read", "write", "apply_patch", "exec", "search_memory", "web_search", "browser_action", "computer_action", "ask_user", "spawn_agent", "read_media"}
	tools := make([]sdk.Tool, 0, len(names))
	for i, name := range names {
		tools = append(tools, sdk.Tool{
			Name: name, Description: "Contextbench tool " + name + " with bounded, production-shaped usage guidance.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "request text"},
					"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100 + i},
				},
				"required": []string{"query"},
			},
		})
	}
	return tools
}

func estimatedMessageFrag(id string, msg sdk.Message, kind contextfrag.Kind, slot contextfrag.Slot, trust contextfrag.TrustLevel, index int) contextfrag.ContextFrag {
	frag := contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID: id, Message: msg, Kind: kind, Slot: slot, Priority: contextfrag.PriorityForMessage(msg),
		CacheClass: contextfrag.CacheStable, Trust: trust, Scope: contextfrag.Scope{BotID: "contextbench"},
		Source: "contextbench", Collector: "contextbench", Index: index,
	})
	frag.TokenEstimate = contextfrag.ResolveProviderBudgetFragTokens(frag)
	return frag
}

func typedConfig(fixture benchFixture, source []contextfrag.ContextFrag, window int) agentpkg.RunConfig {
	return agentpkg.RunConfig{
		ContextSourceFrags: append([]contextfrag.ContextFrag(nil), source...), ContextScope: contextfrag.Scope{BotID: "contextbench"},
		ContextQueryMaterialized: true, ContextToolDefs: append([]contextfrag.ToolDefAccounting(nil), fixture.toolDefs...),
		ContextToolDefsResolved: true, ContextBudgetMaxTokens: window,
	}
}

func legacyPayload(fixture benchFixture) providerPayload {
	return providerPayload{System: flattenSystem(fixture.systemFrags), Messages: cloneMessages(fixture.messages), Tools: fixture.tools}
}

func flattenSystem(frags []contextfrag.ContextFrag) string {
	ordered := append([]contextfrag.ContextFrag(nil), frags...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Priority < ordered[j].Priority })
	var out strings.Builder
	var previous contextfrag.RenderPolicy
	written := false
	for _, frag := range ordered {
		if frag.Slot != contextfrag.SlotSystem {
			continue
		}
		for _, part := range frag.Parts {
			if part.Type != contextfrag.PartText {
				continue
			}
			if written {
				out.WriteString(contextfrag.RenderSeparator(previous, frag.Render))
			}
			out.WriteString(contextfrag.RenderText(part.Text, frag.Render))
			previous = frag.Render
			written = true
		}
	}
	return out.String()
}

func providerPayloadMetrics(payload providerPayload) (hash string, payloadBytes, tokens int) {
	hash, payloadBytes = contextfrag.ProviderPayloadHashAndBytes(payload.System, payload.Messages, payload.Tools)
	withoutImageData, images := payloadWithoutImageData(payload)
	_, estimatedBytes := contextfrag.ProviderPayloadHashAndBytes(withoutImageData.System, withoutImageData.Messages, withoutImageData.Tools)
	return hash, payloadBytes, contextfrag.ProviderBudgetTokensFromBytes(estimatedBytes) + images*contextfrag.EstimateImageTokens
}

func providerEnvelopeTokens(payload providerPayload) int {
	total := contextfrag.ProviderBudgetTokensFromBytes(len(payload.System))
	for i, message := range payload.Messages {
		frag := contextfrag.MessageFrag(contextfrag.MessageFragInput{
			ID: "estimate." + threeDigits(i), Message: message,
			Kind: contextfrag.KindConversationEvent, Slot: contextfrag.SlotHistory,
		})
		total += contextfrag.ResolveProviderBudgetFragTokens(frag)
	}
	for _, tool := range payload.Tools {
		accounting := contextfrag.ToolDefAccountingFor("contextbench", tool)
		total += max(accounting.TokenEstimate, contextfrag.ProviderBudgetTokensFromBytes(accounting.Bytes))
	}
	return total
}

func payloadWithoutImageData(payload providerPayload) (providerPayload, int) {
	out := payload
	out.Messages = cloneMessages(payload.Messages)
	images := 0
	for i := range out.Messages {
		for j, part := range out.Messages[i].Content {
			image, ok := part.(sdk.ImagePart)
			if !ok {
				continue
			}
			images++
			image.Image = ""
			out.Messages[i].Content[j] = image
		}
	}
	return out, images
}

func rawProviderPayload(payload providerPayload) []byte {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return raw
}

func cloneMessages(messages []sdk.Message) []sdk.Message {
	out := make([]sdk.Message, len(messages))
	for i, message := range messages {
		out[i] = message
		out[i].Content = append([]sdk.MessagePart(nil), message.Content...)
	}
	return out
}

func seededText(rng *rand.Rand, size int, multilingual bool) string {
	if size <= 0 {
		return ""
	}
	pattern := "context orchestration fixture data "
	if multilingual {
		pattern = "上下文编排🙂稳定性数据 "
	}
	var out strings.Builder
	out.Grow(size)
	for out.Len()+len(pattern) <= size {
		out.WriteString(pattern)
	}
	for out.Len() < size {
		out.WriteByte(byte('a' + rng.Intn(26)))
	}
	return out.String()
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func marshalStable(value any) ([]byte, error) {
	return json.Marshal(value)
}

func equalMessages(left, right sdk.Message) bool {
	return reflect.DeepEqual(left, right)
}

func twoDigits(value int) string {
	return string([]byte{'0' + byte(value/10), '0' + byte(value%10)})
}

func threeDigits(value int) string {
	return string([]byte{'0' + byte(value/100), '0' + byte(value/10%10), '0' + byte(value%10)})
}
