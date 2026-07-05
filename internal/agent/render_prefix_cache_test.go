package agent

import (
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

// TestRenderedStableMessageCount guards against inferring the Anthropic
// system->message cache promotion from messages[0].Role alone. contextview's
// TrimNoticeFrag (internal/contextview/selector_budget.go) renders
// HistoryTrimNotice as a CacheClass: contextfrag.CacheNever system-role
// message that can legitimately land at messages[0] after budget trimming,
// with no cache promotion ever having happened. The caller must instead pass
// down whether ApplyPromptCacheWithPlan actually prepended a system message.
func TestRenderedStableMessageCount(t *testing.T) {
	t.Parallel()

	volatileLeadingSystemMessage := sdk.Message{
		Role:    sdk.MessageRoleSystem,
		Content: []sdk.MessagePart{sdk.TextPart{Text: "some volatile system-role notice, e.g. a history-trim notice"}},
	}

	t.Run("volatile leading system message is not counted when nothing was prepended", func(t *testing.T) {
		t.Parallel()
		plan := contextfrag.CachePlan{StableMessageCount: 0}
		messages := []sdk.Message{volatileLeadingSystemMessage, sdk.UserMessage("hello")}

		if got := renderedStableMessageCount(plan, messages, false); got != 0 {
			t.Fatalf("got %d, want 0", got)
		}
	})

	t.Run("promoted system message is counted when systemPrepended is true", func(t *testing.T) {
		t.Parallel()
		plan := contextfrag.CachePlan{StableMessageCount: 0}
		messages := []sdk.Message{volatileLeadingSystemMessage, sdk.UserMessage("hello")}

		if got := renderedStableMessageCount(plan, messages, true); got != 1 {
			t.Fatalf("got %d, want 1", got)
		}
	})

	t.Run("promotion stacks with a non-zero stable message count", func(t *testing.T) {
		t.Parallel()
		plan := contextfrag.CachePlan{StableMessageCount: 2}
		messages := []sdk.Message{
			volatileLeadingSystemMessage,
			sdk.UserMessage("stable-1"),
			sdk.UserMessage("stable-2"),
			sdk.UserMessage("volatile"),
		}

		if got := renderedStableMessageCount(plan, messages, true); got != 3 {
			t.Fatalf("got %d, want 3", got)
		}
	})
}
