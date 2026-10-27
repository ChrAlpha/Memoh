package compaction

const (
	minCompactionReadBytes = 512 << 10
	maxCompactionReadBytes = 8 << 20
)

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
