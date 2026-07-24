package contextview

import (
	"reflect"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
)

func TestFragmentSelector_MustKeepSystem(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		textFrag("system", contextfrag.SlotSystem, contextfrag.KindSystemPrompt, sdk.MessageRoleSystem, "system"),
		messageFrag("old", sdk.AssistantMessage("old")),
		messageFrag("current", sdk.UserMessage("current")),
	}

	result := selectCompactionFrags(frags)

	assertSelectedIDs(t, result, []string{"old"})
	assertDroppedReason(t, result, "system", string(TagMustKeep))
}

func TestFragmentSelector_MustKeepCurrentUser(t *testing.T) {
	t.Parallel()

	current := textFrag("current", contextfrag.SlotCurrentUser, contextfrag.KindCurrentUserMessage, sdk.MessageRoleUser, "now")
	frags := []contextfrag.ContextFrag{
		messageFrag("old", sdk.AssistantMessage("old")),
		current,
	}

	result := selectCompactionFrags(frags)

	assertSelectedIDs(t, result, []string{"old"})
	assertDroppedReason(t, result, "current", string(TagMustKeep))
}

func TestFragmentSelector_MustKeepOverflowKeepPolicy(t *testing.T) {
	t.Parallel()

	keep := messageFrag("summary", sdk.AssistantMessage("summary"))
	keep.Budget = contextfrag.BudgetPolicy{Overflow: contextfrag.OverflowKeep}
	frags := []contextfrag.ContextFrag{
		messageFrag("old", sdk.AssistantMessage("old")),
		keep,
		messageFrag("current", sdk.UserMessage("current")),
	}

	result := selectCompactionFrags(frags)

	assertSelectedIDs(t, result, []string{"old"})
	assertDroppedReason(t, result, "summary", string(TagMustKeep))
}

func TestFragmentSelector_PreserveRecentFromLastUser(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		messageFrag("old-user", sdk.UserMessage("old question")),
		messageFrag("old-assistant", sdk.AssistantMessage("old answer")),
		messageFrag("current-user", sdk.UserMessage("new question")),
		messageFrag("current-assistant", sdk.AssistantMessage("new answer")),
	}

	result := selectCompactionFrags(frags)

	assertSelectedIDs(t, result, []string{"old-user", "old-assistant"})
	assertDroppedReason(t, result, "current-user", string(TagPreserveRecent))
	assertDroppedReason(t, result, "current-assistant", string(TagPreserveRecent))
}

func TestFragmentSelector_PreserveRecentNoUserFallback(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		messageFrag("old", sdk.AssistantMessage("old")),
		toolCallFrag("call", "calc", "call-1"),
		toolResultFrag("result", "calc", "call-1", "42"),
	}

	result := selectCompactionFrags(frags)

	assertSelectedIDs(t, result, []string{"old"})
	assertDroppedReason(t, result, "call", string(TagPreserveRecent))
	assertDroppedReason(t, result, "result", string(TagPreserveRecent))
}

func TestFragmentSelector_ToolClosureNotOrphaned(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		messageFrag("context", sdk.AssistantMessage("context")),
		toolCallFrag("call", "calc", "call-1"),
		toolResultFrag("result", "calc", "call-1", "42"),
		messageFrag("tail", sdk.AssistantMessage("done")),
	}

	result := selectCompactionFrags(frags)

	assertSelectedIDs(t, result, []string{"context", "call", "result"})
}

func TestFragmentSelector_CompactionCutoffDoesNotOrphanToolResult(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		messageFrag("context", sdk.AssistantMessage("context")),
		toolCallFrag("call", "calc", "call-1"),
		toolResultFrag("result", "calc", "call-1", "42"),
		messageFrag("tail", sdk.AssistantMessage("done")),
	}

	result := (&FragmentSelector{}).SelectCompactionCandidates(frags, 2)

	assertSelectedIDs(t, result, []string{"context", "call", "result"})
}

func TestFragmentSelector_CompactionCutoffKeepsCurrentTurnHeadAndTail(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		messageFrag("current-user", sdk.UserMessage("current instruction")),
		messageFrag("loop-1", sdk.AssistantMessage("loop step 1")),
		messageFrag("loop-2", sdk.AssistantMessage("loop step 2")),
		messageFrag("tail", sdk.AssistantMessage("latest tail")),
	}

	result := (&FragmentSelector{}).SelectCompactionCandidates(frags, len(frags))

	assertSelectedIDs(t, result, []string{"loop-1", "loop-2"})
	assertDroppedReason(t, result, "current-user", string(TagPreserveRecent))
	assertDroppedReason(t, result, "tail", string(TagPreserveRecent))
}

func TestFragmentSelector_CompactionNormalizesRecentUserRole(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		messageFrag("old", sdk.AssistantMessage("old answer")),
		messageFrag("current-user", sdk.Message{
			Role:    sdk.MessageRole(" User "),
			Content: []sdk.MessagePart{sdk.TextPart{Text: "current instruction"}},
		}),
		messageFrag("tail", sdk.AssistantMessage("latest tail")),
	}

	result := (&FragmentSelector{}).SelectCompactionCandidates(frags, len(frags))

	assertSelectedIDs(t, result, []string{"old"})
	assertDroppedReason(t, result, "current-user", string(TagPreserveRecent))
	assertDroppedReason(t, result, "tail", string(TagPreserveRecent))
}

func TestFragmentSelector_CompactionNormalizesToolResultRole(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		messageFrag("context", sdk.AssistantMessage("context")),
		toolCallFrag("call", "calc", "call-1"),
		messageFrag("result", sdk.Message{
			Role: sdk.MessageRole(" Tool "),
			Content: []sdk.MessagePart{sdk.ToolResultPart{
				ToolCallID: "call-1",
				ToolName:   "calc",
				Result:     "42",
			}},
		}),
		messageFrag("tail", sdk.AssistantMessage("done")),
	}

	result := (&FragmentSelector{}).SelectCompactionCandidates(frags, 2)

	assertSelectedIDs(t, result, []string{"context", "call", "result"})
}

func TestFragmentSelector_CanDropOlderHistory(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		messageFrag("old-1", sdk.UserMessage("old 1")),
		messageFrag("old-2", sdk.AssistantMessage("old 2")),
		messageFrag("latest", sdk.UserMessage("latest")),
	}

	result := selectCompactionFrags(frags)

	assertSelectedIDs(t, result, []string{"old-1", "old-2"})
	assertDroppedReason(t, result, "latest", string(TagPreserveRecent))
}

func TestFragmentSelector_DeterministicSameInputSameOutput(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		messageFrag("old", sdk.UserMessage("old")),
		messageFrag("answer", sdk.AssistantMessage("answer")),
		messageFrag("latest", sdk.UserMessage("latest")),
	}

	first := selectCompactionFrags(frags)
	second := selectCompactionFrags(frags)

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("Select() not deterministic:\nfirst=%#v\nsecond=%#v", first, second)
	}
}

func TestFragmentSelector_EmptyInput(t *testing.T) {
	t.Parallel()

	result := selectCompactionFrags(nil)

	if len(result.Selected) != 0 || len(result.Dropped) != 0 {
		t.Fatalf("result = %#v, want empty", result)
	}
	if result.Summary.TotalCollected != 0 || result.Summary.TotalSelected != 0 || result.Summary.TotalDropped != 0 {
		t.Fatalf("summary = %#v, want zero counts", result.Summary)
	}
}

func TestFragmentSelector_CompactionProfileMustKeepSystemAndCurrentUser(t *testing.T) {
	t.Parallel()

	profile := (&FragmentSelector{}).ProfileFor(contextfrag.IntentCompactionCandidates)

	if profile.Intent != contextfrag.IntentCompactionCandidates {
		t.Fatalf("Intent = %q, want %q", profile.Intent, contextfrag.IntentCompactionCandidates)
	}
	if !slotInProfile(profile, contextfrag.SlotSystem) {
		t.Fatalf("MustKeepSlots = %#v, want system", profile.MustKeepSlots)
	}
	if !slotInProfile(profile, contextfrag.SlotCurrentUser) {
		t.Fatalf("MustKeepSlots = %#v, want current_user", profile.MustKeepSlots)
	}
}

func selectCompactionFrags(frags []contextfrag.ContextFrag) SelectionResult {
	selector := &FragmentSelector{}
	profile := selector.ProfileFor(contextfrag.IntentCompactionCandidates)
	return selector.Select(frags, profile, BudgetEnvelope{MaxTokens: 1})
}

func assertSelectedIDs(t *testing.T, result SelectionResult, want []string) {
	t.Helper()
	got := fragIDs(result.Selected)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected ids = %#v, want %#v; dropped=%#v", got, want, fragIDs(result.Dropped))
	}
	if result.Summary.TotalSelected != len(want) {
		t.Fatalf("TotalSelected = %d, want %d", result.Summary.TotalSelected, len(want))
	}
}

func assertDroppedReason(t *testing.T, result SelectionResult, fragID, reason string) {
	t.Helper()
	for _, record := range result.Summary.DropReasons {
		if record.FragID == fragID {
			if record.Reason != reason {
				t.Fatalf("drop reason for %s = %q, want %q", fragID, record.Reason, reason)
			}
			return
		}
	}
	t.Fatalf("missing drop record for %s; records=%#v", fragID, result.Summary.DropReasons)
}

func fragIDs(frags []contextfrag.ContextFrag) []string {
	out := make([]string, len(frags))
	for i, frag := range frags {
		out[i] = frag.ID
	}
	return out
}

func slotInProfile(profile IntentProfile, slot contextfrag.Slot) bool {
	for _, candidate := range profile.MustKeepSlots {
		if candidate == slot {
			return true
		}
	}
	return false
}

func toolCallFrag(id, name, callID string) contextfrag.ContextFrag {
	return messageFrag(id, sdk.Message{
		Role: sdk.MessageRoleAssistant,
		Content: []sdk.MessagePart{
			sdk.ToolCallPart{ToolCallID: callID, ToolName: name, Input: map[string]any{}},
		},
	})
}

func toolResultFrag(id, name, callID, result string) contextfrag.ContextFrag {
	return messageFrag(id, sdk.ToolMessage(sdk.ToolResultPart{
		ToolCallID: callID,
		ToolName:   name,
		Result:     result,
	}))
}
