package contextview

import (
	"reflect"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	native "github.com/felinics/memoh/internal/agent/runtime/native"
)

func providerContextSwitchSources(frags []contextfrag.ContextFrag, messages []sdk.Message) []native.ContextSwitchSource {
	sources := make([]native.ContextSwitchSource, len(messages))
	index := 0
	for _, frag := range frags {
		if frag.Slot == contextfrag.SlotSystem {
			continue
		}
		message, single := singleSDKMessage(frag)
		for _, part := range frag.Parts {
			if part.Type != contextfrag.PartSDKMessage || sdkMessagePart(part) == nil {
				continue
			}
			if index >= len(messages) {
				return make([]native.ContextSwitchSource, len(messages))
			}
			if single && reflect.DeepEqual(message, messages[index]) && frag.Ref.Durability == contextfrag.RefDurable &&
				frag.Slot != contextfrag.SlotCurrentUser && (frag.Kind == contextfrag.KindConversationSummary || frag.Kind == contextfrag.KindConversationEvent) {
				sources[index].Ref = frag.Ref
				if frag.Coverage != nil {
					sources[index].Coverage = append([]contextfrag.ContextRef(nil), frag.Coverage.CoveredRefs...)
				}
			}
			index++
		}
	}
	return sources
}
