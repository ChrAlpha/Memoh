package inbound

import (
	"fmt"
	"reflect"
	"testing"
)

func fmtStream(op string, stream int) string { return fmt.Sprintf("%s:%d", op, stream) }

// A run whose text before a tool call is followed by two outputs, with
// reasoning between them, shows the same on a channel that hides tool calls and
// on one that shows them: two previews, the first holding the text before the
// call, the second the call (when shown) and the later outputs. The final the
// observers see is the one message of the later outputs, and nothing is sent
// as a message of its own.
func TestRunToolCallEndsIMTextSegment(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools bool
		want  []string
	}{
		{name: "hidden", want: []string{
			"open:1", "delta:1:Let me check.", "finish:1", "open:2", "delta:2:hm", "delta:2:Part one.", "delta:2:Part two.", "finish:2",
		}},
		{name: "shown", tools: true, want: []string{
			"open:1", "delta:1:Let me check.", "finish:1", "open:2", "tool_call_start:2", "tool_call_end:2", "delta:2:hm", "delta:2:Part one.", "delta:2:Part two.", "finish:2",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payloads := append(append([]string{}, turnWithToolCall...), turnWithToolCallEnd())
			rec := runDeliveryScenario(t, deliveryScenario{name: tc.name, caps: scenarioCaps(nil), tools: tc.tools, gateway: scriptedRun(payloads...)})
			var got []string
			for _, call := range rec.Calls {
				switch {
				case call.Op == "open":
					got = append(got, fmtStream("open", call.Stream))
				case call.Op == "finish":
					got = append(got, fmtStream("finish", call.Stream))
				case call.Op == "send":
					t.Fatalf("a message was sent on its own: %+v", call.Message)
				case call.Op == "update" && call.Event != nil && call.Event.Type == "delta":
					got = append(got, fmtStream("delta", call.Stream)+":"+call.Event.Delta)
				case call.Op == "update" && call.Event != nil && (call.Event.Type == "tool_call_start" || call.Event.Type == "tool_call_end"):
					got = append(got, fmtStream(call.Event.Type, call.Stream))
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("calls = %v, want %v", got, tc.want)
			}
			var finals []string
			for _, event := range rec.Observed {
				if event.Type == "final" && event.Final != nil && event.Final.Text != "hello" {
					finals = append(finals, event.Final.Text)
				}
			}
			if want := []string{"Part one.\nPart two."}; !reflect.DeepEqual(finals, want) {
				t.Fatalf("observed finals = %v, want %v", finals, want)
			}
		})
	}
}
