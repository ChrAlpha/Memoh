package native

import (
	"sync"

	sdk "github.com/memohai/twilight-ai/sdk"
)

type providerAttemptState struct {
	mu              sync.RWMutex
	messages        []sdk.Message
	stepIndex       int
	systemPrepended bool
	stored          bool
}

func (s *providerAttemptState) store(params *sdk.GenerateParams, stepIndex int, systemPrepended bool) {
	if s == nil || params == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = cloneProviderMessages(params.Messages)
	s.stepIndex = stepIndex
	s.systemPrepended = systemPrepended
	s.stored = true
}

func (s *providerAttemptState) retryMessages(previous *sdk.StreamResult) ([]sdk.Message, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	messages := cloneProviderMessages(s.messages)
	stepIndex := s.stepIndex
	systemPrepended := s.systemPrepended
	stored := s.stored
	s.mu.RUnlock()
	if !stored {
		return nil, false
	}

	if systemPrepended {
		if len(messages) == 0 {
			return nil, false
		}
		messages = messages[1:]
	}
	clearProviderCacheControls(messages)
	if previous != nil && stepIndex >= 0 && stepIndex < len(previous.Steps) {
		messages = append(messages, cloneProviderMessages(previous.Steps[stepIndex].Messages)...)
	}
	return messages, true
}

func cloneProviderMessages(messages []sdk.Message) []sdk.Message {
	if messages == nil {
		return nil
	}
	cloned := make([]sdk.Message, len(messages))
	for i := range messages {
		cloned[i] = messages[i]
		cloned[i].Content = append([]sdk.MessagePart(nil), messages[i].Content...)
		if messages[i].Usage != nil {
			usage := *messages[i].Usage
			cloned[i].Usage = &usage
		}
	}
	return cloned
}

func clearProviderCacheControls(messages []sdk.Message) {
	for i := range messages {
		for j, part := range messages[i].Content {
			switch value := part.(type) {
			case sdk.TextPart:
				value.CacheControl = nil
				messages[i].Content[j] = value
			case sdk.ImagePart:
				value.CacheControl = nil
				messages[i].Content[j] = value
			case sdk.FilePart:
				value.CacheControl = nil
				messages[i].Content[j] = value
			}
		}
	}
}

func retryProviderAttemptMessages(cfg RunConfig, previous *sdk.StreamResult) []sdk.Message {
	if messages, ok := cfg.providerAttemptState.retryMessages(previous); ok {
		return messages
	}
	accumulated := []sdk.Message(nil)
	if previous != nil {
		accumulated = previous.Messages
	}
	merged := make([]sdk.Message, 0, len(cfg.Messages)+len(accumulated))
	merged = append(merged, cfg.Messages...)
	merged = append(merged, accumulated...)
	return merged
}
