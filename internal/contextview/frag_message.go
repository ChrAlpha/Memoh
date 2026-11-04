package contextview

import (
	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
)

func isToolExchangeFrag(frag contextfrag.ContextFrag) bool {
	msg := contextfrag.FragMessage(frag)
	if msg == nil {
		return false
	}
	if msg.Role == sdk.MessageRoleTool {
		return true
	}
	for _, part := range msg.Content {
		switch part.(type) {
		case sdk.ToolCallPart, sdk.ToolResultPart:
			return true
		}
	}
	return false
}
