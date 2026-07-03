package contextview

import (
	"context"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
)

func SelectProviderStepMessages(ctx context.Context, input agentpkg.ContextStepSelectionInput) agentpkg.ContextStepSelectionResult {
	if input.InitialMessageCount < 0 || input.InitialMessageCount >= len(input.Messages) {
		return agentpkg.ContextStepSelectionResult{}
	}
	loopMessages := input.Messages[input.InitialMessageCount:]
	if len(loopMessages) == 0 {
		return agentpkg.ContextStepSelectionResult{}
	}

	frags, err := (&HistoryMessagesCollector{}).Collect(ctx, CollectRequest{
		Scope:  input.Scope,
		Intent: contextfrag.IntentRunConfigPreProvider,
		Config: HistoryMessagesConfig{
			Messages:        loopMessages,
			TrimmablePrefix: len(loopMessages),
		},
	})
	if err != nil || len(frags) == 0 {
		return agentpkg.ContextStepSelectionResult{}
	}

	selector := &FragmentSelector{}
	selection := selector.Select(frags, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{
		MaxTokens: input.BudgetMaxTokens,
	})
	if len(selection.Dropped) == 0 {
		return agentpkg.ContextStepSelectionResult{}
	}

	selected := selectedProviderStepFrags(selection, input.Scope)
	messages := make([]sdk.Message, 0, input.InitialMessageCount+len(selected))
	messages = append(messages, cloneSDKMessages(input.Messages[:input.InitialMessageCount])...)
	messages = append(messages, sdkMessagesFromFrags(selected)...)

	return agentpkg.ContextStepSelectionResult{
		Messages:    messages,
		Dropped:     len(selection.Dropped),
		DropReasons: dropReasonHistogram(selection.Summary.DropReasons),
	}
}

func selectedProviderStepFrags(selection SelectionResult, scope contextfrag.Scope) []contextfrag.ContextFrag {
	if !selection.TrimNotice || selection.TrimNoticeIndex < 0 || selection.TrimNoticeIndex > len(selection.Selected) {
		return selection.Selected
	}
	notice := contextfrag.NormalizeContextRefs([]contextfrag.ContextFrag{TrimNoticeFrag(scope)})[0]
	selected := make([]contextfrag.ContextFrag, 0, len(selection.Selected)+1)
	selected = append(selected, selection.Selected[:selection.TrimNoticeIndex]...)
	selected = append(selected, notice)
	selected = append(selected, selection.Selected[selection.TrimNoticeIndex:]...)
	return selected
}

func sdkMessagesFromFrags(frags []contextfrag.ContextFrag) []sdk.Message {
	var messages []sdk.Message
	for _, frag := range frags {
		for _, part := range frag.Parts {
			if part.Type != contextfrag.PartSDKMessage {
				continue
			}
			if msg := sdkMessagePart(part); msg != nil {
				messages = append(messages, cloneSDKMessage(*msg))
			}
		}
	}
	return messages
}

func cloneSDKMessages(messages []sdk.Message) []sdk.Message {
	out := make([]sdk.Message, len(messages))
	for i, msg := range messages {
		out[i] = cloneSDKMessage(msg)
	}
	return out
}

func dropReasonHistogram(records []DropRecord) map[string]int {
	if len(records) == 0 {
		return nil
	}
	out := make(map[string]int, len(records))
	for _, record := range records {
		out[record.Reason]++
	}
	return out
}
