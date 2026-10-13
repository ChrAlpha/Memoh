package contextview

import (
	"slices"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
)

const imageOmitEditPrefix = "images.budget_omit."

func shedOptionalHistoryImages(frags []contextfrag.ContextFrag, optional []string, historyBudget int) ([]contextfrag.ContextFrag, []fragBudgetEdit) {
	excess, total, trimmable, inInput := -historyBudget, -historyBudget, false, false
	for _, frag := range frags {
		if frag.Slot == contextfrag.SlotSystem || frag.Slot == contextfrag.SlotCurrentUser || frag.Kind == contextfrag.KindCurrentUserMessage {
			continue
		}
		cost := contextfrag.ResolveProviderBudgetFragTokens(frag)
		total += cost
		inInput = inInput || slices.Contains(optional, frag.ID)
		if inInput || frag.Slot != contextfrag.SlotHistory || frag.Kind != contextfrag.KindConversationEvent {
			excess += cost
		} else {
			trimmable = true
		}
	}
	if total <= 0 {
		return frags, nil
	}
	if trimmable {
		excess += contextfrag.ResolveProviderBudgetFragTokens(TrimNoticeFrag(contextfrag.Scope{}))
	}
	out := slices.Clone(frags)
	var edits []fragBudgetEdit
	for i, frag := range out {
		if excess <= 0 {
			break
		}
		msg := contextfrag.FragMessage(frag)
		if !slices.Contains(optional, frag.ID) || msg == nil || msg.Role != sdk.MessageRoleUser || frag.Kind != contextfrag.KindConversationEvent {
			continue
		}
		kept := *msg
		for excess > 0 {
			image := slices.IndexFunc(kept.Content, func(part sdk.MessagePart) bool { _, ok := part.(sdk.ImagePart); return ok })
			if image < 0 {
				break
			}
			before := contextfrag.ResolveProviderBudgetFragTokens(out[i])
			kept.Content = slices.Delete(slices.Clone(kept.Content), image, image+1)
			out[i] = contextfrag.RebuildFragMessage(frag, kept)
			excess -= before - contextfrag.ResolveProviderBudgetFragTokens(out[i])
		}
		if len(kept.Content) != len(msg.Content) {
			edits = append(edits, fragBudgetEdit{fragID: frag.ID, reason: string(contextfrag.MutationCurrentInputImagesOmitted), trace: contextfrag.ContextEditTrace{
				EditID: imageOmitEditPrefix + frag.ID, Op: contextfrag.EditReplace, Slot: frag.Slot, Refs: []contextfrag.ContextRef{out[i].Ref},
			}})
		}
	}
	return out, edits
}
