package contextview

import (
	"context"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/pipeline"
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
		sdk.UserMessage("[Conversation summary]\nearlier context"),
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

func TestDiscussCollector_ConsecutiveRCMerged(t *testing.T) {
	t.Parallel()

	frags := collectDiscussContext(t, DiscussContextConfig{
		RC: pipeline.RenderedContext{
			renderedTextSegment(100, "one"),
			renderedTextSegment(200, "two"),
			renderedTextSegment(300, "three"),
		},
	})

	assertDiscussMessages(t, frags, []sdk.Message{
		sdk.UserMessage("one\ntwo\nthree"),
	})
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
		Scope:  contextfrag.Scope{BotID: "bot-1", SessionID: "s1", TurnID: "t1"},
		Intent: contextfrag.IntentDiscussReply,
		Config: cfg,
	})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	return frags
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
		got = append(got, discussFragMessage(t, frag))
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

func discussFragMessage(t *testing.T, frag contextfrag.ContextFrag) sdk.Message {
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
