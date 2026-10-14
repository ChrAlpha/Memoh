package inbound

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/turn"
)

// Check, not a regression suite: does a bot's reply stay beside the message it
// answered when another member speaks while the run is going? The old
// discuss path sorted the reply by write time and the platform message by a
// one-second timestamp, and the two interleaved wrongly.

// orderPart is one message of a model request, flattened for assertions.
type orderPart struct {
	role      string
	text      string
	reasoning []string
	// reasoningDialect is "format/model" of each reasoning block: a block with
	// no format is never replayed to a provider.
	reasoningDialect []string
	callIDs          []string
	callNames        []string
	callInput        []string
	resultIDs        []string
	results          []string
}

func orderParts(t *testing.T, messages []sdk.Message) []orderPart {
	t.Helper()
	out := make([]orderPart, 0, len(messages))
	for _, m := range messages {
		p := orderPart{role: string(m.Role)}
		for _, part := range m.Content {
			switch v := part.(type) {
			case sdk.TextPart:
				p.text += v.Text
			case sdk.ReasoningPart:
				p.reasoning = append(p.reasoning, v.Text)
				p.reasoningDialect = append(p.reasoningDialect, string(v.Format)+"/"+v.Model)
			case sdk.ToolCallPart:
				raw, err := json.Marshal(v.Input)
				if err != nil {
					t.Fatal(err)
				}
				p.callIDs = append(p.callIDs, v.ToolCallID)
				p.callNames = append(p.callNames, v.ToolName)
				p.callInput = append(p.callInput, string(raw))
			case sdk.ToolResultPart:
				raw, err := json.Marshal(v.Result)
				if err != nil {
					t.Fatal(err)
				}
				p.resultIDs = append(p.resultIDs, v.ToolCallID)
				p.results = append(p.results, string(raw))
			default:
				p.text += fmt.Sprintf("<%T>", part)
			}
		}
		out = append(out, p)
	}
	return out
}

func (p orderPart) String() string {
	var b strings.Builder
	b.WriteString(p.role)
	if p.text != "" {
		text := strings.ReplaceAll(p.text, "\n", "\\n")
		if len(text) > 160 {
			text = text[:160] + "..."
		}
		fmt.Fprintf(&b, " text=%q", text)
	}
	for i, r := range p.reasoning {
		fmt.Fprintf(&b, " reasoning[%s]=%q", p.reasoningDialect[i], r)
	}
	for i := range p.callIDs {
		fmt.Fprintf(&b, " call=%s(%s)#%s", p.callNames[i], p.callInput[i], p.callIDs[i])
	}
	for i := range p.resultIDs {
		fmt.Fprintf(&b, " result#%s=%s", p.resultIDs[i], p.results[i])
	}
	return b.String()
}

func dumpOrder(parts []orderPart) string {
	var b strings.Builder
	for i, p := range parts {
		fmt.Fprintf(&b, "  [%02d] %s\n", i, p)
	}
	return b.String()
}

// indexOf is the first message the predicate accepts at or after from, or -1.
func indexOf(parts []orderPart, from int, accept func(orderPart) bool) int {
	for i := from; i < len(parts); i++ {
		if accept(parts[i]) {
			return i
		}
	}
	return -1
}

func userSaying(text string) func(orderPart) bool {
	return func(p orderPart) bool { return p.role == "user" && strings.Contains(p.text, text) }
}

func assistantThinking(thought string) func(orderPart) bool {
	return func(p orderPart) bool {
		if p.role != "assistant" {
			return false
		}
		for _, r := range p.reasoning {
			if r == thought {
				return true
			}
		}
		return false
	}
}

func toolResultOf(callID string) func(orderPart) bool {
	return func(p orderPart) bool {
		for _, id := range p.resultIDs {
			if id == callID {
				return true
			}
		}
		return false
	}
}

// sayAt delivers one group message the platform stamped with at.
func (e *discussE2E) sayAt(member, messageID, text string, mentionsBot bool, at time.Time) {
	e.t.Helper()
	msg := e.inboundMessage(member, messageID, text, mentionsBot)
	msg.ReceivedAt = at
	if err := e.processor.HandleInbound(context.Background(), e.cfg, msg, &fakeReplySender{}); err != nil {
		e.t.Fatalf("HandleInbound(%s) error = %v", messageID, err)
	}
}

type orderRow struct {
	position  int64
	seq       int64
	role      string
	source    string
	createdAt time.Time
	hasEvent  bool
}

func (e *discussE2E) historyRows() []orderRow {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		SELECT turn_position, turn_message_seq, role, COALESCE(source_message_id, ''), created_at, event_id IS NOT NULL
		FROM bot_history_messages WHERE session_id = $1
		ORDER BY turn_position, turn_message_seq
	`, e.sessionID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []orderRow
	for rows.Next() {
		var r orderRow
		if err := rows.Scan(&r.position, &r.seq, &r.role, &r.source, &r.createdAt, &r.hasEvent); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func dumpRows(rows []orderRow) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "  pos=%d seq=%d role=%s src=%q event=%v created=%s\n",
			r.position, r.seq, r.role, r.source, r.hasEvent, r.createdAt.UTC().Format("15:04:05.000000"))
	}
	return b.String()
}

func TestDiscussReplyStaysBesideTheMessageItAnswers(t *testing.T) {
	const earlierRounds = 5
	for _, variant := range []struct {
		name string
		// backdateB rewrites the write time of B's rows to an hour before A's,
		// so an ordering that depended on write time would put B first.
		backdateB bool
	}{
		{name: "natural write times"},
		{name: "B written before A", backdateB: true},
	} {
		t.Run(variant.name, func(t *testing.T) {
			var steps []*e2eStep
			for i := range earlierRounds {
				steps = append(steps,
					&e2eStep{send: fmt.Sprintf("earlier answer %d", i), reasoning: fmt.Sprintf("earlier thought %d", i)},
					&e2eStep{})
			}
			first := &e2eStep{
				reached: make(chan struct{}), gate: make(chan struct{}),
				send: "A: the freeze is Friday", reasoning: "A asked about the freeze, which is Friday",
			}
			steps = append(steps, first, &e2eStep{}, &e2eStep{send: "B: Bob owns the release", reasoning: "B asked who owns the release"}, &e2eStep{})
			e := newDiscussE2E(t, e2eEmbedded, steps...)

			// The history before A: whole rounds, each answered through send.
			base := time.Now().Truncate(time.Second)
			for i := range earlierRounds {
				e.sayAt("carol", fmt.Sprintf("pre-%d", i), fmt.Sprintf("earlier question %d", i), false,
					base.Add(-time.Duration(earlierRounds-i)*time.Minute))
				hw := e.position()
				eventually(t, fmt.Sprintf("earlier round %d to complete", i), func() bool { return e.cover() == hw })
			}
			if rows := len(e.historyRows()); rows <= 10 {
				t.Fatalf("history rows before A = %d, want more than 10", rows)
			}

			// A and B are stamped with the same whole second.
			stamp := base
			e.sayAt("alice", "msg-a", "A: is the freeze on this week?", false, stamp)
			waitClosed(t, first.reached, "A's run to call the model")
			e.sayAt("bob", "msg-b", "B: @bot who owns the release?", true, stamp)
			eventually(t, "B's wake to find the session held", func() bool { return e.admitter.count(turn.DiscussBusy) == 1 })

			if variant.backdateB {
				tag, err := e.pool.Exec(context.Background(), `
					UPDATE bot_history_messages SET created_at = (
						SELECT created_at FROM bot_history_messages WHERE session_id = $1 AND source_message_id = 'msg-a'
					) - interval '1 hour'
					WHERE session_id = $1 AND source_message_id = 'msg-b'
				`, e.sessionID)
				if err != nil || tag.RowsAffected() != 1 {
					t.Fatalf("backdate B's history row: rows=%d err=%v", tag.RowsAffected(), err)
				}
				if _, err := e.pool.Exec(context.Background(), `
					UPDATE bot_session_events SET created_at = (
						SELECT created_at FROM bot_session_events WHERE session_id = $1 AND external_message_id = 'msg-a'
					) - interval '1 hour'
					WHERE session_id = $1 AND external_message_id = 'msg-b'
				`, e.sessionID); err != nil {
					t.Fatalf("backdate B's event: %v", err)
				}
			}

			close(first.gate)
			hwB := e.position()
			eventually(t, "the run's end to answer B", func() bool { return e.cover() == hwB })
			eventually(t, "every run to complete", func() bool { return e.runs()["completed"] == earlierRounds+2 })
			if got := e.runs(); len(got) != 1 {
				t.Fatalf("runs = %v, want only completed", got)
			}
			if failures := e.admitter.failures(); len(failures) != 0 {
				t.Fatalf("wakes failed: %v", failures)
			}

			rows := e.historyRows()
			t.Logf("history rows by (turn_position, turn_message_seq):\n%s", dumpRows(rows))

			// What the rows say: A, then the reply, then B, whatever the clock.
			// A reply row is an assistant row whose (turn_position,
			// turn_message_seq) lies between A's and B's.
			var keyA, keyB, keyReply [2]int64
			keyA, keyB = [2]int64{-1, -1}, [2]int64{-1, -1}
			for _, r := range rows {
				switch r.source {
				case "msg-a":
					keyA = [2]int64{r.position, r.seq}
				case "msg-b":
					keyB = [2]int64{r.position, r.seq}
				}
			}
			before := func(x, y [2]int64) bool { return x[0] < y[0] || (x[0] == y[0] && x[1] < y[1]) }
			var replyRows int
			for _, r := range rows {
				key := [2]int64{r.position, r.seq}
				if r.role == "assistant" && before(keyA, key) && before(key, keyB) {
					if replyRows == 0 {
						keyReply = key
					}
					replyRows++
				}
			}
			if keyA[0] < 0 || keyB[0] < 0 || replyRows == 0 {
				t.Fatalf("rows do not put A %v before a reply before B %v:\n%s", keyA, keyB, dumpRows(rows))
			}
			t.Logf("order keys: A=%v first reply row=%v B=%v", keyA, keyReply, keyB)
			runRows, err := e.pool.Query(context.Background(), `
				SELECT turn_position, state, invocation_id, COALESCE(input_json->>'cover_position', '-') FROM session_runs
				WHERE session_id = $1 ORDER BY turn_position
			`, e.sessionID)
			if err != nil {
				t.Fatal(err)
			}
			var runLog strings.Builder
			for runRows.Next() {
				var pos int64
				var state, inv, cover string
				if err := runRows.Scan(&pos, &state, &inv, &cover); err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(&runLog, "  run pos=%d state=%s cover=%s\n", pos, state, cover)
			}
			runRows.Close()
			t.Logf("runs:\n%s", runLog.String())

			if !variant.backdateB {
				// The precondition of the old defect: the reply was written after
				// both messages, and A and B carry the same platform second.
				var aAt, bAt, replyAt time.Time
				for _, r := range rows {
					switch {
					case r.source == "msg-a":
						aAt = r.createdAt
					case r.source == "msg-b":
						bAt = r.createdAt
					case r.role == "assistant" && r.position == keyReply[0] && r.seq == keyReply[1]:
						replyAt = r.createdAt
					}
				}
				if !replyAt.After(bAt) || !bAt.After(aAt) {
					t.Fatalf("write times A=%s B=%s reply=%s, want A < B < reply", aAt, bAt, replyAt)
				}
			}
			var sameSecond int
			if err := e.pool.QueryRow(context.Background(), `
				SELECT count(DISTINCT received_at_ms) FROM bot_session_events WHERE session_id = $1 AND external_message_id IN ('msg-a', 'msg-b')
			`, e.sessionID).Scan(&sameSecond); err != nil || sameSecond != 1 {
				t.Fatalf("A and B received_at_ms distinct = %d (err %v), want the same instant", sameSecond, err)
			}

			calls := e.model.calls()
			if want := 2*earlierRounds + 4; len(calls) != want {
				t.Fatalf("model calls = %d, want %d", len(calls), want)
			}
			// calls[2n] is A's run's first call, calls[2n+1] its follow-up after
			// send returned, calls[2n+2] the second round's first call.
			followUp := orderParts(t, calls[2*earlierRounds+1].Messages)
			second := orderParts(t, calls[2*earlierRounds+2].Messages)
			t.Logf("second round request (%d messages):\n%s", len(second), dumpOrder(second))

			if len(second) <= 10 {
				t.Fatalf("second round request has %d messages, want more than 10 to exercise the history cut", len(second))
			}

			// Earlier rounds are in order and intact: question, thought plus send,
			// the send's result.
			cursor := 0
			for i := range earlierRounds {
				q := indexOf(second, cursor, userSaying(fmt.Sprintf("earlier question %d", i)))
				a := indexOf(second, max(q, 0), assistantThinking(fmt.Sprintf("earlier thought %d", i)))
				if q < 0 || a < 0 {
					t.Fatalf("earlier round %d is missing or out of order (question at %d, thought at %d):\n%s", i, q, a, dumpOrder(second))
				}
				if got := second[a]; len(got.callIDs) != 1 || got.callNames[0] != "send" || !strings.Contains(got.callInput[0], fmt.Sprintf("earlier answer %d", i)) {
					t.Fatalf("earlier round %d lost its send call: %s", i, got)
				}
				r := indexOf(second, a, toolResultOf(second[a].callIDs[0]))
				if r < 0 || strings.TrimSpace(second[r].results[0]) == "" {
					t.Fatalf("earlier round %d lost its send result (at %d):\n%s", i, r, dumpOrder(second))
				}
				cursor = r
			}

			// A, the reply to A with its reasoning and tool exchange, then B.
			iA := indexOf(second, cursor, userSaying("A: is the freeze on this week?"))
			iReply := indexOf(second, max(iA, 0), assistantThinking("A asked about the freeze, which is Friday"))
			if iA < 0 || iReply < 0 {
				t.Fatalf("A at %d, reply to A at %d:\n%s", iA, iReply, dumpOrder(second))
			}
			reply := second[iReply]
			if len(reply.callIDs) != 1 || reply.callNames[0] != "send" || !strings.Contains(reply.callInput[0], "A: the freeze is Friday") {
				t.Fatalf("the reply to A lost its send call: %s", reply)
			}
			if len(reply.reasoningDialect) != 1 || reply.reasoningDialect[0] != string(sdk.ReasoningFormatOpenAIChat)+"/deepseek-reasoner" {
				t.Fatalf("the reply to A lost the dialect of its reasoning: %v", reply.reasoningDialect)
			}
			iResult := indexOf(second, iReply, toolResultOf(reply.callIDs[0]))
			iB := indexOf(second, max(iA, 0), userSaying("B: @bot who owns the release?"))
			if iResult < 0 || iB < 0 {
				t.Fatalf("send result at %d, B at %d:\n%s", iResult, iB, dumpOrder(second))
			}
			if cursor >= iA || iA >= iReply || iReply >= iResult || iResult >= iB {
				t.Fatalf("order is cursor=%d A=%d reply=%d result=%d B=%d, want strictly increasing:\n%s", cursor, iA, iReply, iResult, iB, dumpOrder(second))
			}
			lastUser := -1
			for i, p := range second {
				if p.role == "user" {
					lastUser = i
				}
			}
			if lastUser != iB {
				t.Fatalf("last user message is at %d, B is at %d:\n%s", lastUser, iB, dumpOrder(second))
			}
			if iB != len(second)-1 {
				t.Logf("note: B is at %d of %d messages", iB, len(second))
			}

			// What the run itself saw for its own reply, as the follow-up request
			// carried it, is what the second round replays.
			var liveReply, liveResult string
			for _, p := range followUp {
				if assistantThinking("A asked about the freeze, which is Friday")(p) {
					liveReply = p.String()
				}
				if toolResultOf(reply.callIDs[0])(p) {
					liveResult = p.String()
				}
			}
			if liveReply == "" || liveResult == "" {
				t.Fatalf("A's follow-up request did not carry the run's own exchange:\n%s", dumpOrder(followUp))
			}
			if liveReply != reply.String() || liveResult != second[iResult].String() {
				t.Fatalf("the exchange changed between the run and the replay:\n live  %s\n live  %s\n replay %s\n replay %s",
					liveReply, liveResult, reply, second[iResult])
			}
		})
	}
}
