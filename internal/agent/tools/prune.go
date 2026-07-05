package tools

import (
	"github.com/memohai/memoh/internal/contextlimit"
)

const (
	listMaxEntries        = 200
	listCollapseThreshold = 50
)

func pruneToolOutputText(text, label string) string {
	return contextlimit.PruneTier.LimitString(text, label)
}
