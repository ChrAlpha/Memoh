package compaction

const (
	minCompactionReadBytes = 512 << 10
	maxCompactionReadBytes = 8 << 20
)

// passReadMaxBytes is the most candidate payload one pass reads for a
// summarizer of the window, sized from the prompt window its fixed prompt
// leaves.
func passReadMaxBytes(summaryWindowTokens int) int64 {
	return compactionReadMaxBytes(summaryWindowTokens - estimateBytesAsTokens(systemPrompt) - compactionPromptFramingTokens)
}

func compactionReadMaxBytes(inputTokens int) int64 {
	bytes := int64(inputTokens) * 8
	if bytes < minCompactionReadBytes {
		return minCompactionReadBytes
	}
	if bytes > maxCompactionReadBytes {
		return maxCompactionReadBytes
	}
	return bytes
}
