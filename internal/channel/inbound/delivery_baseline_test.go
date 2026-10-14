package inbound

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/channel"
)

// The delivery layer changes what a reply looks like in a few ways and in no
// other. TestDeliveryMatchesBaseline runs every scenario on the current code
// and compares what the adapter received with the recording made on the code
// before the layer (4b97f4d95). A scenario listed in allowedDifferences is
// compared with its baseline recording rewritten by the change it names; every
// other scenario must match exactly, and so must what observers saw in all of
// them.
//
// The changes:
//
//   - later assistant outputs are new messages: the first output is the
//     preview's final, and each later one used to be a further final on the same
//     preview, overwriting it.
//   - a run that a messaging tool answered flushes the text the preview shows.
//   - an error after the preview shows text is a message of its own: it used to
//     be pushed into the stream, where it overwrote the text. The text is
//     flushed before an error that ends the run.
//   - a request for approval or an answer that the channel cannot render is a
//     plain message between two previews: the preview with text now ends with
//     the completed status that every ended preview gets, and the message is
//     sent before the next preview opens rather than after.
var allowedDifferences = map[string]func(t *testing.T, calls []recordedCall, sc deliveryScenario) []recordedCall{
	"multi_assistant":            laterOutputsAreMessages,
	"multi_assistant_with_tool":  laterOutputsAreMessages,
	"suppress_tail":              suppressedRunFlushes,
	"error_after_text":           errorEndingRunIsMessage,
	"long_stream_then_error":     errorEndingRunIsMessage,
	"terminal_failed_after_text": errorEndingRunIsMessage,
	"error_event_after_text":     errorEventIsMessage,
	"approval_text_after_text":   promptBetweenPreviews,
	"approval_text_only":         promptBetweenPreviews,
	"approval_text_twice":        promptBetweenPreviews,
	"askuser_text_after_text":    promptBetweenPreviews,
}

func isPush(call recordedCall, eventType string) bool {
	return call.Op == "push" && call.Event != nil && call.Event.Type == eventType
}

func isCompletedPush(call recordedCall) bool {
	return isPush(call, "status") && call.Event.Status == "completed"
}

func isStartedPush(call recordedCall) bool {
	return isPush(call, "status") && call.Event.Status == "started"
}

func isEmptyFinal(call recordedCall) bool {
	return isPush(call, "final") && call.Event.Final != nil && call.Event.Final.Text == ""
}

func emptyFinalOn(stream int) recordedCall {
	return recordedCall{Op: "push", Stream: stream, Event: &recordedEvent{Type: "final", Final: &recordedMessage{}}}
}

func targetOf(calls []recordedCall) string {
	for _, call := range calls {
		if call.Op == "open" {
			return call.Target
		}
	}
	return ""
}

func replyOf(calls []recordedCall) string {
	for _, call := range calls {
		if call.Op == "open" {
			return call.ReplyTo
		}
	}
	return ""
}

func sendOf(target, replyTo, text string) recordedCall {
	return recordedCall{Op: "send", Target: target, Message: &recordedMessage{Text: text, Format: "plain", ReplyTo: replyTo}}
}

// insertAfterLast inserts items after the last call with op.
func insertAfterLast(calls []recordedCall, op string, items ...recordedCall) []recordedCall {
	at := len(calls)
	for i, call := range calls {
		if call.Op == op {
			at = i + 1
		}
	}
	out := append([]recordedCall{}, calls[:at]...)
	out = append(out, items...)
	return append(out, calls[at:]...)
}

func laterOutputsAreMessages(t *testing.T, calls []recordedCall, _ deliveryScenario) []recordedCall {
	t.Helper()
	var out, sends []recordedCall
	seen := false
	for _, call := range calls {
		if isPush(call, "final") && call.Event.Final != nil && call.Event.Final.Text != "" {
			if seen {
				final := call.Event.Final
				sends = append(sends, recordedCall{Op: "send", Target: targetOf(calls), Message: final})
				continue
			}
			seen = true
		}
		out = append(out, call)
	}
	return insertAfterLast(out, "close", sends...)
}

func suppressedRunFlushes(t *testing.T, calls []recordedCall, _ deliveryScenario) []recordedCall {
	t.Helper()
	var out []recordedCall
	for _, call := range calls {
		if isCompletedPush(call) {
			out = append(out, emptyFinalOn(call.Stream))
		}
		out = append(out, call)
	}
	return out
}

// errorSends are the messages an error text is delivered as: the text split to
// the channel's size.
func errorSends(calls []recordedCall, sc deliveryScenario, text string) []recordedCall {
	limit := sc.limit
	if limit <= 0 {
		limit = 2000
	}
	var sends []recordedCall
	for _, chunk := range channel.ChunkText(text, limit) {
		sends = append(sends, sendOf(targetOf(calls), replyOf(calls), chunk))
	}
	return sends
}

func errorEndingRunIsMessage(t *testing.T, calls []recordedCall, sc deliveryScenario) []recordedCall {
	t.Helper()
	var out []recordedCall
	var sends []recordedCall
	for i := len(calls) - 1; i >= 0; i-- {
		if isPush(calls[i], "error") {
			sends = errorSends(calls, sc, calls[i].Event.Error)
			break
		}
	}
	replaced := false
	for i := len(calls) - 1; i >= 0; i-- {
		call := calls[i]
		if !replaced && isPush(call, "error") {
			call = emptyFinalOn(call.Stream)
			replaced = true
		}
		out = append([]recordedCall{call}, out...)
	}
	return insertAfterLast(out, "close", sends...)
}

func errorEventIsMessage(t *testing.T, calls []recordedCall, sc deliveryScenario) []recordedCall {
	t.Helper()
	var out []recordedCall
	for _, call := range calls {
		if isPush(call, "error") {
			out = append(out, errorSends(calls, sc, call.Event.Error)...)
			continue
		}
		out = append(out, call)
	}
	return out
}

func promptBetweenPreviews(t *testing.T, calls []recordedCall, _ deliveryScenario) []recordedCall {
	t.Helper()
	var out []recordedCall
	for i := 0; i < len(calls); i++ {
		if i+4 < len(calls) && isEmptyFinal(calls[i]) && calls[i+1].Op == "close" && calls[i+2].Op == "open" &&
			isStartedPush(calls[i+3]) && calls[i+4].Op == "send" {
			completed := recordedCall{Op: "push", Stream: calls[i].Stream, Event: &recordedEvent{Type: "status", Status: "completed"}}
			out = append(out, calls[i], completed, calls[i+1], calls[i+4], calls[i+2], calls[i+3])
			i += 4
			continue
		}
		out = append(out, calls[i])
	}
	return out
}

func TestDeliveryMatchesBaseline(t *testing.T) {
	dir := filepath.Join("..", "delivery", "testdata", "baseline")
	scenarios := deliveryScenarios()
	known := map[string]bool{}
	for _, sc := range scenarios {
		known[sc.name] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if name := strings.TrimSuffix(entry.Name(), ".json"); !known[name] {
			t.Errorf("fixture %s has no scenario", entry.Name())
		}
	}
	for name := range allowedDifferences {
		if !known[name] {
			t.Errorf("allowed difference names unknown scenario %s", name)
		}
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, sc.name+".json")) //nolint:gosec // repo-local fixture
			if err != nil {
				t.Fatal(err)
			}
			var want deliveryRecording
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if rewrite := allowedDifferences[sc.name]; rewrite != nil {
				want.Calls = rewrite(t, want.Calls, sc)
			}
			got := runDeliveryScenario(t, sc)
			if reflect.DeepEqual(got, want) {
				return
			}
			gotJSON, _ := json.MarshalIndent(got, "", "  ")
			wantJSON, _ := json.MarshalIndent(want, "", "  ")
			t.Fatalf("recording differs from the baseline\n--- want\n%s\n--- got\n%s", wantJSON, gotJSON)
		})
	}
}
