package timeline

import (
	"fmt"
	"time"

	"github.com/felinics/memoh/internal/chat/checkpoint"
)

// HistoryNotice is the system message that opens the history of a session
// whose checkpoint leaves earlier messages out: how many, when they were
// sent, and that the context holds of them only what its summaries say. It
// names no tool; the history tools' usage says how to search the range it
// gives. The text depends on what is omitted alone, so it stays the same on
// every request of the session. It is model-visible; change it only
// deliberately. Empty when nothing is omitted.
func HistoryNotice(omitted checkpoint.Omitted) string {
	if !omitted.Omits() {
		return ""
	}
	if omitted.Count == 1 {
		return fmt.Sprintf("[History notice] 1 earlier message of this conversation, %s, is not in this context. "+
			"Only its conversation summaries, if any, carry what it said; to recall details, search the message history in that time range.",
			historyNoticeRange(omitted.First, omitted.Last))
	}
	return fmt.Sprintf("[History notice] %d earlier messages of this conversation, %s, are not in this context. "+
		"Only its conversation summaries, if any, carry what they said; to recall details, search the message history in that time range.",
		omitted.Count, historyNoticeRange(omitted.First, omitted.Last))
}

// historyNoticeRange spans the omitted messages in whole UTC seconds, the
// start rounded down and the end up, so a search over it finds every one.
func historyNoticeRange(first, last time.Time) string {
	from := first.UTC().Truncate(time.Second)
	to := last.UTC().Truncate(time.Second)
	if to.Before(last.UTC()) {
		to = to.Add(time.Second)
	}
	if !to.After(from) {
		return "sent at " + from.Format(time.RFC3339)
	}
	return "sent from " + from.Format(time.RFC3339) + " to " + to.Format(time.RFC3339)
}
