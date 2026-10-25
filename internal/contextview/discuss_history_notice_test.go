package contextview

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	agentpkg "github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/chat/checkpoint"
	"github.com/felinics/memoh/internal/chat/timeline"
)

// The history notice opens the Discuss history: a system message ahead of
// the summaries, kept under any budget and stable across the session's
// requests. The responses composed are the ones after the checkpoint, as
// Discuss loads them.
func TestCollectDiscussSourceFragsOpensWithTheHistoryNotice(t *testing.T) {
	t.Parallel()

	held := timeline.HistoryCheckpoint{
		Omitted: checkpoint.Omitted{Count: 2, First: time.UnixMilli(100), Last: time.UnixMilli(200)},
		Items:   []timeline.CheckpointItem{{SummaryID: "legacy", Summary: "legacy summary"}},
	}
	rc := timeline.RenderedContext{renderedTextSegment(100, "before rc"), renderedTextSegment(300, "after rc")}
	trs := []timeline.TurnResponseEntry{{RequestedAtMs: 400, Role: "assistant", Content: "after turn"}}
	composed := timeline.ComposeContextWithCheckpoint(rc, trs, held)
	scope := contextfrag.Scope{BotID: "bot-1", SessionID: "s1"}
	frags, err := (&DiscussSDKContextBuilder{}).CollectDiscussSourceFrags(context.Background(), scope, "Discuss system prompt.", DiscussContextInput{ComposedMessages: composed.Messages})
	if err != nil {
		t.Fatal(err)
	}
	notice := timeline.HistoryNotice(held.Omitted)
	if composed.Messages[0].Content != notice {
		t.Fatalf("composition opens with %+v, want the notice", composed.Messages[0])
	}
	var found bool
	for _, frag := range frags {
		if frag.ID != "discuss.message.000" {
			continue
		}
		found = true
		if frag.Kind != contextfrag.KindSystemPolicy || frag.Slot != contextfrag.SlotBeforeHistory || frag.CacheClass != contextfrag.CacheStable ||
			frag.Trust != contextfrag.TrustSystem || frag.Budget.Overflow != contextfrag.OverflowKeep {
			t.Fatalf("notice frag = %+v, want a kept, stable system policy before the history", frag)
		}
	}
	if !found {
		t.Fatal("the Discuss frags carry no history notice")
	}

	rendered, err := applyProviderRunConfig(context.Background(), nil, agentpkg.RunConfig{
		System:                   "Discuss system prompt.",
		Messages:                 legacyContextMessagesToSDK(composed.Messages),
		ContextSourceFrags:       frags,
		ContextQueryMaterialized: true,
		ContextScope:             scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range rendered.ContextManifest.Mutations.Records() {
		if mutation.Kind == contextfrag.MutationContextViewFallback {
			t.Fatalf("the typed provider path fell back to the flat oracle: %+v", mutation)
		}
	}
	if len(rendered.Messages) < 2 || rendered.Messages[0].Role != sdk.MessageRoleSystem || messageText(t, rendered.Messages[0]) != notice ||
		!strings.Contains(messageText(t, rendered.Messages[1]), "legacy summary") {
		t.Fatalf("rendered history opens with %+v, want the notice and then the summary", rendered.Messages)
	}
	if raw, err := json.Marshal(rendered.Messages); err != nil || strings.Contains(string(raw), "before ") {
		t.Fatalf("rendered history reads what the checkpoint omits: %s", raw)
	}
}
