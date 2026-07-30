package contextview

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	pipeline "github.com/memohai/memoh/internal/chat/timeline"
)

func TestDiscussCollector_MergesRCAndTR(t *testing.T) {
	t.Parallel()

	frags := collectDiscussContext(t, DiscussContextConfig{
		RC: pipeline.RenderedContext{
			renderedTextSegment(100, "rc-1"),
			renderedTextSegment(300, "rc-2"),
		},
		TRs: []pipeline.TurnResponseEntry{{
			RequestedAtMs: 200,
			Role:          "assistant",
			Content:       "tr-1",
		}},
	})

	assertDiscussIDs(t, frags, []string{"discuss.rc.000", "discuss.tr.000", "discuss.rc.001"})
	assertDiscussMessages(t, frags, []sdk.Message{
		sdk.UserMessage("rc-1"),
		sdk.AssistantMessage("tr-1"),
		sdk.UserMessage("rc-2"),
	})
}

func TestDiscussCollector_SummaryBeforeHistory(t *testing.T) {
	t.Parallel()

	frags := collectDiscussContext(t, DiscussContextConfig{
		RC:             pipeline.RenderedContext{renderedTextSegment(100, "history")},
		CompactSummary: "earlier context",
	})

	if len(frags) != 2 {
		t.Fatalf("frags = %d, want 2", len(frags))
	}
	summary := frags[0]
	if summary.Kind != contextfrag.KindConversationSummary {
		t.Fatalf("summary Kind = %q, want %q", summary.Kind, contextfrag.KindConversationSummary)
	}
	if summary.Slot != contextfrag.SlotBeforeHistory {
		t.Fatalf("summary Slot = %q, want %q", summary.Slot, contextfrag.SlotBeforeHistory)
	}
	if summary.Role != sdk.MessageRoleUser {
		t.Fatalf("summary Role = %q, want %q", summary.Role, sdk.MessageRoleUser)
	}
	if summary.CacheClass != contextfrag.CacheDynamic {
		t.Fatalf("summary CacheClass = %q, want %q", summary.CacheClass, contextfrag.CacheDynamic)
	}
	if summary.Trust != contextfrag.TrustSystem {
		t.Fatalf("summary Trust = %q, want %q", summary.Trust, contextfrag.TrustSystem)
	}
	if summary.Budget.Overflow != contextfrag.OverflowKeep {
		t.Fatalf("summary Overflow = %q, want %q", summary.Budget.Overflow, contextfrag.OverflowKeep)
	}
	assertDiscussMessages(t, frags, []sdk.Message{
		sdk.UserMessage("<summary>\nearlier context\n</summary>"),
		sdk.UserMessage("history"),
	})
}

func TestDiscussCollector_EmptyInput(t *testing.T) {
	t.Parallel()

	frags := collectDiscussContext(t, DiscussContextConfig{})
	if frags != nil {
		t.Fatalf("frags = %#v, want nil", frags)
	}
}

func TestDiscussCollector_ConsecutiveRCAtomized(t *testing.T) {
	t.Parallel()

	frags := collectDiscussContext(t, DiscussContextConfig{
		RC: pipeline.RenderedContext{
			renderedTextSegment(100, "one"),
			renderedTextSegment(200, "two"),
			renderedTextSegment(300, "three"),
		},
	})

	assertDiscussIDs(t, frags, []string{"discuss.rc.000", "discuss.rc.001", "discuss.rc.002"})
	assertDiscussMessages(t, frags, []sdk.Message{
		sdk.UserMessage("one"),
		sdk.UserMessage("two"),
		sdk.UserMessage("three"),
	})
}

func TestDiscussCollector_ComposedMessagesAreAuthoritative(t *testing.T) {
	t.Parallel()

	composed := []pipeline.ContextMessage{
		{Role: "user", Content: "first user"},
		{Role: "user", Content: "second user"},
		{
			Role:                 "user",
			Content:              "<summary>\ncovered history\n</summary>",
			CompactionArtifactID: "artifact-1",
		},
		{
			Role:       "tool",
			Content:    "debug fallback",
			RawContent: json.RawMessage(`[{"type":"tool-result","toolCallId":"call-1","toolName":"lookup","result":{"answer":42}}]`),
		},
		{Role: "user", Content: "latest user"},
	}
	image := sdk.ImagePart{Image: "data:image/png;base64,abc", MediaType: "image/png"}
	frags := collectDiscussContext(t, DiscussContextConfig{
		ComposedMessages: composed,
		RC:               pipeline.RenderedContext{renderedTextSegment(1, "must not be recollected")},
		CompactSummary:   "must not be wrapped again",
		InlineImages:     []sdk.ImagePart{image},
	})

	assertDiscussIDs(t, frags, []string{
		"discuss.message.000",
		"discuss.message.001",
		"discuss.message.002",
		"discuss.message.003",
		"discuss.message.004",
	})
	summary := frags[2]
	if summary.Kind != contextfrag.KindConversationSummary ||
		summary.Slot != contextfrag.SlotBeforeHistory ||
		summary.CacheClass != contextfrag.CacheDynamic ||
		summary.Trust != contextfrag.TrustSystem ||
		summary.Budget.Overflow != contextfrag.OverflowKeep {
		t.Fatalf("summary policy = kind %q slot %q cache %q trust %q overflow %q",
			summary.Kind, summary.Slot, summary.CacheClass, summary.Trust, summary.Budget.Overflow)
	}

	want := legacyContextMessagesToSDK(composed)
	want[len(want)-1].Content = append(want[len(want)-1].Content, image)
	payload, _ := renderSDKPayload(t, frags, placementFor(frags))
	assertMessagesEqual(t, payload.Messages, want)
}

func TestDiscussCollector_TRRoleMapping(t *testing.T) {
	t.Parallel()

	frags := collectDiscussContext(t, DiscussContextConfig{
		TRs: []pipeline.TurnResponseEntry{
			{RequestedAtMs: 100, Role: "assistant", Content: "assistant text"},
			{RequestedAtMs: 200, Role: "tool", Content: "tool text"},
			{RequestedAtMs: 300, Role: "user", Content: "user text"},
		},
	})

	assertDiscussMessages(t, frags, []sdk.Message{
		sdk.AssistantMessage("assistant text"),
		sdk.UserMessage("tool text"),
		sdk.UserMessage("user text"),
	})
	assertDiscussIDs(t, frags, []string{"discuss.tr.000", "discuss.tr.001", "discuss.tr.002"})
	assertDiscussTrusts(t, frags, []contextfrag.TrustLevel{
		contextfrag.TrustWorkspace,
		contextfrag.TrustWorkspace,
		contextfrag.TrustExternal,
	})
}

func collectDiscussContext(t *testing.T, cfg DiscussContextConfig) []contextfrag.ContextFrag {
	t.Helper()
	collector := &DiscussContextCollector{}
	frags, err := collector.Collect(context.Background(), CollectRequest{
		Scope:  contextfrag.Scope{BotID: "bot-1", SessionID: "s1"},
		Intent: contextfrag.IntentDiscussReply,
		Config: cfg,
	})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	return frags
}

func assertDiscussIDs(t *testing.T, frags []contextfrag.ContextFrag, want []string) {
	t.Helper()
	if len(frags) != len(want) {
		t.Fatalf("frags = %d, want %d", len(frags), len(want))
	}
	for i, frag := range frags {
		if frag.ID != want[i] {
			t.Fatalf("frags[%d].ID = %q, want %q", i, frag.ID, want[i])
		}
	}
}

func renderedTextSegment(atMs int64, text string) pipeline.RenderedSegment {
	return pipeline.RenderedSegment{
		ReceivedAtMs: atMs,
		Content:      []pipeline.RenderedContentPiece{{Type: "text", Text: text}},
	}
}

func assertDiscussMessages(t *testing.T, frags []contextfrag.ContextFrag, want []sdk.Message) {
	t.Helper()
	got := make([]sdk.Message, 0, len(frags))
	for _, frag := range frags {
		got = append(got, discussFragMessageT(t, frag))
	}
	assertMessagesEqual(t, got, want)
}

func assertDiscussTrusts(t *testing.T, frags []contextfrag.ContextFrag, want []contextfrag.TrustLevel) {
	t.Helper()
	if len(frags) != len(want) {
		t.Fatalf("frags = %d, want %d", len(frags), len(want))
	}
	for i, frag := range frags {
		if frag.Trust != want[i] {
			t.Fatalf("frags[%d].Trust = %q, want %q", i, frag.Trust, want[i])
		}
	}
}

func discussFragMessageT(t *testing.T, frag contextfrag.ContextFrag) sdk.Message {
	t.Helper()
	if frag.Kind != contextfrag.KindConversationEvent && frag.Kind != contextfrag.KindConversationSummary {
		t.Fatalf("frag %q Kind = %q, want conversation event or summary", frag.ID, frag.Kind)
	}
	if frag.Slot != contextfrag.SlotHistory && frag.Slot != contextfrag.SlotBeforeHistory {
		t.Fatalf("frag %q Slot = %q, want history or before_history", frag.ID, frag.Slot)
	}
	if frag.CacheClass == "" {
		t.Fatalf("frag %q CacheClass should be set", frag.ID)
	}
	if len(frag.Parts) != 1 || frag.Parts[0].Type != contextfrag.PartSDKMessage {
		t.Fatalf("frag %q parts = %#v, want one sdk message part", frag.ID, frag.Parts)
	}
	msg := sdkMessagePart(frag.Parts[0])
	if msg == nil {
		t.Fatalf("frag %q has nil SDK message", frag.ID)
	}
	return *msg
}

func TestDiscussCollectorPerFragScopeFromSegmentMeta(t *testing.T) {
	t.Parallel()

	base := contextfrag.Scope{
		BotID:            "bot-1",
		SessionID:        "s1",
		Platform:         "telegram",
		ConversationType: "group",
	}
	seg := renderedTextSegment(100, "structured")
	seg.MentionsMe = true
	seg.RepliesToMe = true
	seg.Meta = &pipeline.SegmentMeta{
		MessageID:                 "msg-7",
		SenderID:                  "u-1",
		ReplyToMessageID:          "msg-3",
		ReplyToSender:             "Bob",
		ForwardMessageID:          "fwd-1",
		ForwardFromUserID:         "u-9",
		ForwardFromConversationID: "conv-9",
	}

	frags := collectDiscussContextScoped(t, base, DiscussContextConfig{RC: pipeline.RenderedContext{seg}})

	if len(frags) != 1 {
		t.Fatalf("frags = %d, want 1", len(frags))
	}
	scope := frags[0].Scope
	if scope.BotID != "bot-1" || scope.SessionID != "s1" || scope.Platform != "telegram" || scope.ConversationType != "group" {
		t.Fatalf("session-level scope fields must be preserved, got %+v", scope)
	}
	if scope.CurrentMessageID != "msg-7" {
		t.Fatalf("CurrentMessageID = %q, want msg-7", scope.CurrentMessageID)
	}
	if scope.ReplyToMessageID != "msg-3" || scope.ReplySender != "Bob" {
		t.Fatalf("reply fields = %q/%q, want msg-3/Bob", scope.ReplyToMessageID, scope.ReplySender)
	}
	if scope.ForwardMessageID != "fwd-1" || scope.ForwardFromUserID != "u-9" || scope.ForwardFromConversationID != "conv-9" {
		t.Fatalf("forward fields = %q/%q/%q", scope.ForwardMessageID, scope.ForwardFromUserID, scope.ForwardFromConversationID)
	}
	if !scope.MentionsBot || !scope.RepliesToBot {
		t.Fatalf("MentionsBot/RepliesToBot = %v/%v, want true/true", scope.MentionsBot, scope.RepliesToBot)
	}
	wantAttention := []contextfrag.AttentionReason{contextfrag.AttentionMention, contextfrag.AttentionReply}
	if len(scope.Attention) != len(wantAttention) {
		t.Fatalf("Attention = %v, want %v", scope.Attention, wantAttention)
	}
	for i := range wantAttention {
		if scope.Attention[i] != wantAttention[i] {
			t.Fatalf("Attention = %v, want %v", scope.Attention, wantAttention)
		}
	}
}

func TestDiscussCollectorPerFragScopeAttentionVariants(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name             string
		conversationType string
		mentions         bool
		replies          bool
		want             []contextfrag.AttentionReason
	}{
		{"group passive", "group", false, false, []contextfrag.AttentionReason{contextfrag.AttentionPassive}},
		{"group mention", "group", true, false, []contextfrag.AttentionReason{contextfrag.AttentionMention}},
		{"group reply", "group", false, true, []contextfrag.AttentionReason{contextfrag.AttentionReply}},
		{"direct", "direct", false, false, []contextfrag.AttentionReason{contextfrag.AttentionDirect}},
		{"private", "private", false, false, []contextfrag.AttentionReason{contextfrag.AttentionDirect}},
		{"empty type in history is passive", "", false, false, []contextfrag.AttentionReason{contextfrag.AttentionPassive}},
		{"empty type with mention keeps mention only", "", true, false, []contextfrag.AttentionReason{contextfrag.AttentionMention}},
		{"thread passive", "thread", false, false, []contextfrag.AttentionReason{contextfrag.AttentionPassive}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			seg := renderedTextSegment(100, "hi")
			seg.MentionsMe = tc.mentions
			seg.RepliesToMe = tc.replies
			seg.Meta = &pipeline.SegmentMeta{MessageID: "m-1"}
			base := contextfrag.Scope{BotID: "bot-1", ConversationType: tc.conversationType}

			frags := collectDiscussContextScoped(t, base, DiscussContextConfig{RC: pipeline.RenderedContext{seg}})

			if len(frags) != 1 {
				t.Fatalf("frags = %d, want 1", len(frags))
			}
			got := frags[0].Scope.Attention
			if len(got) != len(tc.want) {
				t.Fatalf("Attention = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("Attention = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestDiscussCollectorSegmentConversationTypeOverridesTurnType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		turnType    string
		segmentType string
		want        []contextfrag.AttentionReason
	}{
		{"group segment in direct turn", "direct", "group", []contextfrag.AttentionReason{contextfrag.AttentionPassive}},
		{"direct segment in group turn", "group", "direct", []contextfrag.AttentionReason{contextfrag.AttentionDirect}},
		{"empty segment type falls back to turn type", "direct", "", []contextfrag.AttentionReason{contextfrag.AttentionDirect}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			seg := renderedTextSegment(100, "hi")
			seg.Meta = &pipeline.SegmentMeta{MessageID: "m-1", ConversationType: tc.segmentType}
			base := contextfrag.Scope{BotID: "bot-1", ConversationType: tc.turnType}

			frags := collectDiscussContextScoped(t, base, DiscussContextConfig{RC: pipeline.RenderedContext{seg}})

			if len(frags) != 1 {
				t.Fatalf("frags = %d, want 1", len(frags))
			}
			if got := frags[0].Scope.Attention; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Attention = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDiscussCollectorPerFragSenderIdentity(t *testing.T) {
	t.Parallel()

	text := renderedTextSegment(100, "text msg")
	text.Meta = &pipeline.SegmentMeta{MessageID: "m-1", SenderID: "u-1", SenderDisplayName: "Alice"}
	image := renderedTextSegment(200, "photo msg")
	image.Meta = &pipeline.SegmentMeta{MessageID: "m-2", SenderID: "u-2", SenderUsername: "bob"}
	image.ImageRefs = []pipeline.ImageAttachmentRef{{ContentHash: "hash-1", Mime: "image/jpeg"}}
	base := contextfrag.Scope{
		BotID:             "bot-1",
		ChannelIdentityID: "ci-turn",
		DisplayName:       "Turn Sender",
		ConversationType:  "group",
	}

	frags := collectDiscussContextScoped(t, base, DiscussContextConfig{RC: pipeline.RenderedContext{text, image}})

	if len(frags) != 2 {
		t.Fatalf("frags = %d, want 2", len(frags))
	}
	if frags[0].Scope.DisplayName != "Alice" {
		t.Fatalf("text frag DisplayName = %q, want segment sender Alice", frags[0].Scope.DisplayName)
	}
	if frags[0].Scope.Metadata["sender_id"] != "u-1" {
		t.Fatalf("text frag sender_id = %q, want u-1", frags[0].Scope.Metadata["sender_id"])
	}
	if frags[1].Scope.DisplayName != "bob" {
		t.Fatalf("image frag DisplayName = %q, want username fallback bob", frags[1].Scope.DisplayName)
	}
	if frags[1].Scope.Metadata["sender_id"] != "u-2" {
		t.Fatalf("image frag sender_id = %q, want u-2", frags[1].Scope.Metadata["sender_id"])
	}
	if frags[1].Scope.Metadata["image_refs"] != "hash-1:image/jpeg" {
		t.Fatalf("image frag image_refs = %q, want hash-1:image/jpeg", frags[1].Scope.Metadata["image_refs"])
	}
	for i, frag := range frags {
		if frag.Scope.ChannelIdentityID != "ci-turn" {
			t.Fatalf("frags[%d] ChannelIdentityID = %q, must stay turn-level (Memoh identity, not platform sender)", i, frag.Scope.ChannelIdentityID)
		}
	}
}

func TestDiscussCollectorSenderFallbackChain(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		meta *pipeline.SegmentMeta
		want string
	}{
		{"display name wins", &pipeline.SegmentMeta{MessageID: "m-1", SenderID: "u-1", SenderDisplayName: "Alice", SenderUsername: "alice"}, "Alice"},
		{"username next", &pipeline.SegmentMeta{MessageID: "m-1", SenderID: "u-1", SenderUsername: "alice"}, "alice"},
		{"sender id next", &pipeline.SegmentMeta{MessageID: "m-1", SenderID: "u-1"}, "u-1"},
		{"empty sender keeps turn-level", &pipeline.SegmentMeta{MessageID: "m-1"}, "Turn Sender"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			seg := renderedTextSegment(100, "hi")
			seg.Meta = tc.meta
			base := contextfrag.Scope{BotID: "bot-1", DisplayName: "Turn Sender", ConversationType: "group"}

			frags := collectDiscussContextScoped(t, base, DiscussContextConfig{RC: pipeline.RenderedContext{seg}})

			if len(frags) != 1 {
				t.Fatalf("frags = %d, want 1", len(frags))
			}
			if frags[0].Scope.DisplayName != tc.want {
				t.Fatalf("DisplayName = %q, want %q", frags[0].Scope.DisplayName, tc.want)
			}
		})
	}
}

func TestDiscussCollectorNilMetaKeepsWholeTurnScope(t *testing.T) {
	t.Parallel()

	base := contextfrag.Scope{BotID: "bot-1", SessionID: "s1", ConversationType: "group"}
	frags := collectDiscussContextScoped(t, base, DiscussContextConfig{
		RC: pipeline.RenderedContext{renderedTextSegment(100, "plain")},
	})

	if len(frags) != 1 {
		t.Fatalf("frags = %d, want 1", len(frags))
	}
	scope := frags[0].Scope
	if scope.CurrentMessageID != "" || scope.MentionsBot || len(scope.Attention) != 0 {
		t.Fatalf("nil-Meta segment must keep the whole-turn scope untouched, got %+v", scope)
	}
}

func TestDiscussCollectorPerFragScopeDoesNotChangePayloadHash(t *testing.T) {
	t.Parallel()

	plain := renderedTextSegment(100, "same text")
	structured := renderedTextSegment(100, "same text")
	structured.MentionsMe = true
	structured.Meta = &pipeline.SegmentMeta{MessageID: "m-1", SenderID: "u-1"}
	base := contextfrag.Scope{BotID: "bot-1", ConversationType: "group"}

	plainFrags := collectDiscussContextScoped(t, base, DiscussContextConfig{RC: pipeline.RenderedContext{plain}})
	structuredFrags := collectDiscussContextScoped(t, base, DiscussContextConfig{RC: pipeline.RenderedContext{structured}})

	_, plainHash := renderSDKPayload(t, plainFrags, placementFor(plainFrags))
	_, structuredHash := renderSDKPayload(t, structuredFrags, placementFor(structuredFrags))
	if plainHash != structuredHash {
		t.Fatalf("per-frag scope must not enter the payload hash: %q != %q", plainHash, structuredHash)
	}
}

func TestDiscussRCFragConsumesRenderedSegmentContent(t *testing.T) {
	t.Parallel()

	msg := &pipeline.ICMessage{
		MessageID:    "m1",
		Sender:       &pipeline.CanonicalUser{ID: "u-1", DisplayName: "Alice", Username: "alice"},
		TimestampSec: 1,
		Content:      []pipeline.ContentNode{{Type: "text", Text: "hello"}},
		Conversation: pipeline.ConversationMeta{Channel: "telegram", ConversationType: "group"},
	}
	params := pipeline.RenderParams{ContactNames: map[string]string{"u-1": "Contact Alice"}}
	seg := pipeline.RenderMessageSegment(msg, params)
	seg.Content = []pipeline.RenderedContentPiece{{Type: "text", Text: "EDITED BYTES"}}

	frags := collectDiscussContext(t, DiscussContextConfig{RC: pipeline.RenderedContext{seg}})

	if len(frags) != 1 {
		t.Fatalf("frags = %d, want 1", len(frags))
	}
	got := discussFragMessageT(t, frags[0])
	text, ok := got.Content[0].(sdk.TextPart)
	if !ok {
		t.Fatalf("content = %#v, want text part", got.Content)
	}
	if text.Text != "EDITED BYTES" {
		t.Fatalf("frag must consume the segment's rendered bytes as-is (re-rendering from seg.IC is a proven capability, not the live path):\n--- got ---\n%s", text.Text)
	}
}

func TestRenderMessageSegmentBytesMatchPipelineRender(t *testing.T) {
	t.Parallel()

	ic := pipeline.NewEmptyIC("s1")
	ic.Nodes = []pipeline.ICNode{
		{Message: &pipeline.ICMessage{
			MessageID:    "m1",
			Sender:       &pipeline.CanonicalUser{ID: "u-1", DisplayName: "Alice", Username: "alice"},
			ReceivedAtMs: 100,
			TimestampSec: 1,
			UTCOffsetMin: 330,
			Content: []pipeline.ContentNode{
				{Type: "mention", UserID: "u-2", Children: []pipeline.ContentNode{{Type: "text", Text: "Bob"}}},
				{Type: "text", Text: ` see "this" & <that>`},
			},
			Conversation: pipeline.ConversationMeta{Channel: "telegram", ConversationName: "Dev Room", ConversationType: "group"},
		}},
		{Message: &pipeline.ICMessage{
			MessageID:        "m2",
			Sender:           &pipeline.CanonicalUser{ID: "u-2", DisplayName: "Bob"},
			ReceivedAtMs:     200,
			TimestampSec:     2,
			Content:          []pipeline.ContentNode{{Type: "text", Text: "reply body"}},
			ReplyToMessageID: "m1",
			ReplyToSender:    &pipeline.CanonicalUser{ID: "u-1", DisplayName: "Alice"},
			ReplyToPreview:   "earlier",
			ForwardInfo:      &pipeline.ForwardInfo{MessageID: "f1", SenderName: "Carol"},
			Attachments: []pipeline.Attachment{
				{Type: "image", MimeType: "image/png", ContentHash: "hash-1", FilePath: "/tmp/a.png", AltText: "pic"},
			},
			Conversation: pipeline.ConversationMeta{Channel: "telegram", ConversationType: "group"},
		}},
		{SystemEvent: &pipeline.ICSystemEvent{
			Type:         "system_event",
			Kind:         "chat_renamed",
			ReceivedAtMs: 300,
			TimestampSec: 3,
			NewTitle:     "New Room",
		}},
	}
	params := pipeline.RenderParams{ContactNames: map[string]string{"u-1": "Contact Alice"}}
	rc := pipeline.Render(ic, params)

	if len(rc) != len(ic.Nodes) {
		t.Fatalf("segments = %d, want %d", len(rc), len(ic.Nodes))
	}
	for i, node := range ic.Nodes {
		if node.Message == nil {
			continue
		}
		direct := pipeline.RenderMessageSegment(node.Message, params)
		if direct.Content[0].Text != rc[i].Content[0].Text {
			t.Fatalf("RenderMessageSegment bytes differ from pipeline render at %d:\n--- direct ---\n%s\n--- seg ---\n%s", i, direct.Content[0].Text, rc[i].Content[0].Text)
		}
	}
}

func TestDiscussRCFragCarriesImageRefsMetadata(t *testing.T) {
	t.Parallel()

	seg := renderedTextSegment(100, "photo msg")
	seg.ImageRefs = []pipeline.ImageAttachmentRef{
		{ContentHash: "hash-1", Mime: "image/jpeg"},
		{ContentHash: "hash-2", Mime: "image/png"},
	}

	frags := collectDiscussContext(t, DiscussContextConfig{RC: pipeline.RenderedContext{seg}})

	if len(frags) != 1 {
		t.Fatalf("frags = %d, want 1", len(frags))
	}
	got := frags[0].Scope.Metadata["image_refs"]
	if got != "hash-1:image/jpeg,hash-2:image/png" {
		t.Fatalf("image_refs metadata = %q, want hash-1:image/jpeg,hash-2:image/png", got)
	}
	if len(frags[0].Parts) != 1 || frags[0].Parts[0].Type != contextfrag.PartSDKMessage {
		t.Fatalf("image refs must stay out of Parts, got %#v", frags[0].Parts)
	}
}

func TestDiscussRCFragImageRefsDoNotChangePayload(t *testing.T) {
	t.Parallel()

	plain := renderedTextSegment(100, "same text")
	withRefs := renderedTextSegment(100, "same text")
	withRefs.ImageRefs = []pipeline.ImageAttachmentRef{{ContentHash: "hash-1", Mime: "image/jpeg"}}

	plainFrags := collectDiscussContext(t, DiscussContextConfig{RC: pipeline.RenderedContext{plain}})
	refFrags := collectDiscussContext(t, DiscussContextConfig{RC: pipeline.RenderedContext{withRefs}})

	plainPayload, plainHash := renderSDKPayload(t, plainFrags, placementFor(plainFrags))
	refPayload, refHash := renderSDKPayload(t, refFrags, placementFor(refFrags))
	if plainHash != refHash {
		t.Fatalf("image refs metadata must not enter the payload hash: %q != %q", plainHash, refHash)
	}
	assertMessagesEqual(t, refPayload.Messages, plainPayload.Messages)
}

func TestDiscussRCFragImageRefsDoNotMutateBaseScopeMetadata(t *testing.T) {
	t.Parallel()

	seg := renderedTextSegment(100, "photo msg")
	seg.ImageRefs = []pipeline.ImageAttachmentRef{{ContentHash: "hash-1", Mime: "image/jpeg"}}
	base := contextfrag.Scope{BotID: "bot-1", Metadata: map[string]string{"existing": "kept"}}

	frags := collectDiscussContextScoped(t, base, DiscussContextConfig{RC: pipeline.RenderedContext{seg}})

	if _, leaked := base.Metadata["image_refs"]; leaked {
		t.Fatal("collector must not mutate the caller's scope metadata map")
	}
	if len(frags) != 1 {
		t.Fatalf("frags = %d, want 1", len(frags))
	}
	if frags[0].Scope.Metadata["existing"] != "kept" {
		t.Fatalf("existing metadata must be preserved, got %#v", frags[0].Scope.Metadata)
	}
	if frags[0].Scope.Metadata["image_refs"] != "hash-1:image/jpeg" {
		t.Fatalf("image_refs = %q, want hash-1:image/jpeg", frags[0].Scope.Metadata["image_refs"])
	}
}

func collectDiscussContextScoped(t *testing.T, scope contextfrag.Scope, cfg DiscussContextConfig) []contextfrag.ContextFrag {
	t.Helper()
	collector := &DiscussContextCollector{}
	frags, err := collector.Collect(context.Background(), CollectRequest{
		Scope:  scope,
		Intent: contextfrag.IntentDiscussReply,
		Config: cfg,
	})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	return frags
}

func TestDiscussCollectorAppendsLateBindingLast(t *testing.T) {
	t.Parallel()

	frags := collectDiscussContext(t, DiscussContextConfig{
		RC:          pipeline.RenderedContext{renderedTextSegment(100, "hello")},
		LateBinding: "Only reply when mentioned.",
	})

	last := frags[len(frags)-1]
	if last.ID != "discuss.late_binding" || last.Slot != contextfrag.SlotAfterCurrent {
		t.Fatalf("last frag = %s/%s, want late binding after current", last.ID, last.Slot)
	}
	msg := discussFragMessage(last)
	if msg == nil {
		t.Fatal("late binding frag missing sdk message")
	}
	text, ok := msg.Content[0].(sdk.TextPart)
	if !ok || text.Text != "Only reply when mentioned." {
		t.Fatalf("late binding content = %#v", msg.Content)
	}
}

func TestDiscussCollectorInjectsImagesIntoLastUserFrag(t *testing.T) {
	t.Parallel()

	frags := collectDiscussContext(t, DiscussContextConfig{
		RC: pipeline.RenderedContext{
			renderedTextSegment(100, "first"),
			renderedTextSegment(300, "latest"),
		},
		TRs: []pipeline.TurnResponseEntry{{
			RequestedAtMs: 200,
			Role:          "assistant",
			Content:       "reply",
		}},
		InlineImages: []sdk.ImagePart{{Image: "data:image/png;base64,abc", MediaType: "image/png"}},
	})

	// The freshest user message (latest RC) must carry the image part.
	var lastUser *sdk.Message
	for i := len(frags) - 1; i >= 0; i-- {
		msg := discussFragMessageT(t, frags[i])
		if msg.Role == sdk.MessageRoleUser {
			lastUser = &msg
			break
		}
	}
	if lastUser == nil {
		t.Fatal("no user fragment found")
	}
	foundImage := false
	for _, part := range lastUser.Content {
		if _, ok := part.(sdk.ImagePart); ok {
			foundImage = true
		}
	}
	if !foundImage {
		t.Fatalf("image not injected into last user message: %#v", lastUser.Content)
	}
}

func TestDiscussImageInjectionPreservesPerSegmentScope(t *testing.T) {
	t.Parallel()

	seg := renderedTextSegment(100, "photo msg")
	seg.MentionsMe = true
	seg.Meta = &pipeline.SegmentMeta{MessageID: "msg-7"}
	seg.ImageRefs = []pipeline.ImageAttachmentRef{{ContentHash: "hash-1", Mime: "image/jpeg"}}
	base := contextfrag.Scope{BotID: "bot-1", ConversationType: "group"}

	frags := collectDiscussContextScoped(t, base, DiscussContextConfig{
		RC:           pipeline.RenderedContext{seg},
		InlineImages: []sdk.ImagePart{{Image: "data:image/png;base64,abc", MediaType: "image/png"}},
	})

	if len(frags) != 1 {
		t.Fatalf("frags = %d, want 1", len(frags))
	}
	msg := discussFragMessageT(t, frags[0])
	foundImage := false
	for _, part := range msg.Content {
		if _, ok := part.(sdk.ImagePart); ok {
			foundImage = true
		}
	}
	if !foundImage {
		t.Fatalf("image not injected: %#v", msg.Content)
	}
	scope := frags[0].Scope
	if scope.Metadata["image_refs"] != "hash-1:image/jpeg" {
		t.Fatalf("image injection dropped image_refs metadata, got %#v", scope.Metadata)
	}
	if scope.CurrentMessageID != "msg-7" {
		t.Fatalf("image injection dropped per-segment CurrentMessageID, got %q", scope.CurrentMessageID)
	}
	if len(scope.Attention) != 1 || scope.Attention[0] != contextfrag.AttentionMention {
		t.Fatalf("image injection dropped per-segment Attention, got %v", scope.Attention)
	}
}

func TestDiscussCollectorDropsImagesWithoutUserMessage(t *testing.T) {
	t.Parallel()

	frags := collectDiscussContext(t, DiscussContextConfig{
		TRs: []pipeline.TurnResponseEntry{{
			RequestedAtMs: 100,
			Role:          "assistant",
			Content:       "solo assistant",
		}},
		InlineImages: []sdk.ImagePart{{Image: "data:image/png;base64,abc", MediaType: "image/png"}},
	})

	for _, frag := range frags {
		msg := discussFragMessageT(t, frag)
		for _, part := range msg.Content {
			if _, ok := part.(sdk.ImagePart); ok {
				t.Fatal("images must be dropped when no user message exists (legacy parity)")
			}
		}
	}
}
