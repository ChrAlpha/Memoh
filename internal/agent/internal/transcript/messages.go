package transcript

import (
	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/internal/runreq"
)

func NormalizeContextMessages(messages []runreq.ModelMessage) []runreq.ModelMessage {
	return runreq.RepairToolCallClosures(runreq.SanitizeMessages(messages), runreq.SyntheticToolClosureError)
}

func ValidCurrentUserMessageIndex(messages []sdk.Message, currentIndex, memoryIndex *int) *int {
	if currentIndex != nil && *currentIndex >= 0 && *currentIndex < len(messages) &&
		messages[*currentIndex].Role == sdk.MessageRoleUser && (memoryIndex == nil || *currentIndex != *memoryIndex) {
		return runreq.IntPointer(*currentIndex)
	}
	return latestUserMessageIndex(messages, optionalIndex(memoryIndex)...)
}

func latestUserMessageIndex(messages []sdk.Message, excluded ...int) *int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == sdk.MessageRoleUser && !containsIndex(excluded, i) {
			return runreq.IntPointer(i)
		}
	}
	return nil
}

func optionalIndex(index *int) []int {
	if index == nil {
		return nil
	}
	return []int{*index}
}

func containsIndex(indexes []int, target int) bool {
	for _, index := range indexes {
		if index == target {
			return true
		}
	}
	return false
}
