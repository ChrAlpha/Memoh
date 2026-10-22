package agent

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/compact"
	"github.com/felinics/memoh/internal/agent/pending"
)

// backlog is a discuss session whose model window is small enough that a dozen
// messages written while nothing answered cannot all be sent, with the real
// compaction service behind it and a scripted summarizer.
type backlog struct {
	t       *testing.T
	env     discussEnv
	proc    *discussProcess
	session *agenttest.CompactionSession
	alice   discussParticipant
}

func newBacklog(t *testing.T, steps ...*discussStep) *backlog {
	t.Helper()
	env := newDiscussEnv(t, "group")
	session := agenttest.NewCompactionSessionFor(t, context.Background(), env.pool, env.botID, env.sessionID)
	proc := newDiscussProcessWith(t, env, []discussOption{
		discussContextWindow(8000),
		discussCompaction(session),
	}, steps...)
	return &backlog{t: t, env: env, proc: proc, session: session, alice: env.participant(t, false)}
}

func (b *backlog) input(id, text string) passiveInput {
	return passiveInput{from: b.alice, externalID: id, text: text}
}

// write stores the inputs as one arrival and returns the newest position.
func (b *backlog) write(inputs ...passiveInput) int64 {
	b.t.Helper()
	positions := b.env.writeAll(b.t, inputs...)
	return positions[len(positions)-1]
}

func (b *backlog) backlogInputs(count, size int) []passiveInput {
	inputs := make([]passiveInput, count)
	for i := range inputs {
		inputs[i] = b.input(fmt.Sprintf("backlog-%d", i), fmt.Sprintf("backlog-%d ", i)+strings.Repeat("x", size))
	}
	return inputs
}

// answer has the batch ending at hw answered, and returns the request the
// model received for it.
func (b *backlog) answer(hw int64) string {
	b.t.Helper()
	before := len(b.proc.provider.recorded())
	b.proc.answer(b.t, b.env, hw)
	requests := b.proc.provider.recorded()
	if len(requests) == before {
		b.t.Fatal("the batch was answered without reaching the model")
	}
	return string(marshalDriveRequests(b.t, requests[len(requests)-1:]))
}

// rows counts the session's rows whose external id starts with prefix, raw and
// covered by a summary.
func (b *backlog) rows(prefix string) (raw, compacted int) {
	b.t.Helper()
	if err := b.env.pool.QueryRow(context.Background(), `
		SELECT count(*) FILTER (WHERE compact_id IS NULL), count(*) FILTER (WHERE compact_id IS NOT NULL)
		FROM bot_history_messages WHERE session_id = $1 AND source_message_id LIKE $2
	`, b.env.sessionID, prefix+"%").Scan(&raw, &compacted); err != nil {
		b.t.Fatal(err)
	}
	return raw, compacted
}

// A backlog larger than the model's window does not stall the session: the
// older input is summarized, the newest input stays raw, and the round after
// it needs no further summary.
func TestDiscussBacklogOverBudgetIsAnsweredAndNextRoundContinues(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	b := newBacklog(t)
	hw := b.write(append(b.backlogInputs(12, 2800), b.input("backlog-newest", "backlog-newest newest"))...)

	request := b.answer(hw)

	if !strings.Contains(request, "SUMMARY-1") || strings.Contains(request, "backlog-0 ") || strings.Count(request, "backlog-newest newest") != 1 {
		t.Fatalf("request must carry the summary, not the oldest input, and the newest input once:\n%.2000s", request)
	}
	if raw, compacted := b.rows("backlog-"); raw == 0 || compacted == 0 {
		t.Fatalf("backlog rows raw=%d compacted=%d, want it summarized behind a raw newest input", raw, compacted)
	}

	calls := b.session.Summarizer.CallCount()
	hw = b.write(b.input("next-round", "next-round small message"))
	request = b.answer(hw)
	if got := b.session.Summarizer.CallCount(); got != calls {
		t.Fatalf("summarizer calls = %d after the next round, want the %d it needed before", got, calls)
	}
	if strings.Count(request, "next-round small message") != 1 {
		t.Fatalf("the next round's input is not in its request exactly once:\n%.2000s", request)
	}
}

// When the backlog cannot be summarized the context goes to the model as it
// is: every message is still there, the newest once, and the next round is
// answered too.
func TestDiscussBacklogIsAnsweredWhenItCannotBeSummarized(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	b := newBacklog(t)
	b.session.Summarizer.RefuseWith(400)
	hw := b.write(append(b.backlogInputs(12, 2800), b.input("backlog-newest", "backlog-newest newest"))...)

	request := b.answer(hw)

	for i := range 12 {
		if strings.Count(request, fmt.Sprintf("backlog-%d ", i)) != 1 {
			t.Fatalf("backlog-%d is not in the request exactly once:\n%.2000s", i, request)
		}
	}
	if strings.Count(request, "backlog-newest newest") != 1 {
		t.Fatalf("the newest input is not in the request exactly once:\n%.2000s", request)
	}
	hw = b.write(b.input("next-round", "next-round small message"))
	if request = b.answer(hw); strings.Count(request, "next-round small message") != 1 {
		t.Fatalf("the next round's input is not in its request exactly once:\n%.2000s", request)
	}
}

// An input too large for the provider fails its run and leaves the batch
// pending; once a later message arrives it is older history, summarized, and
// the session is answered again. The input is within what the compaction
// service can read.
func TestDiscussOversizedInputIsSummarizedOnceALaterMessageArrives(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	const oversizedChars = 70_000
	b := newBacklog(t, &discussStep{fail: true})
	hw := b.write(b.input("oversized", "oversized "+strings.Repeat("y", oversizedChars)))
	outcome, done := b.proc.admit(t, b.env)
	if outcome != pending.DiscussAdmitted {
		t.Fatalf("outcome = %v, want admitted", outcome)
	}
	waitDiscussRun(t, done)
	b.env.waitRunState(t, b.env.invocation(hw), "failed")
	if cover := b.env.cover(t); cover != 0 {
		t.Fatalf("cover position = %d, want the failed batch left pending", cover)
	}

	hw = b.write(b.input("next-round", "next-round small message"))
	request := b.answer(hw)

	if strings.Contains(request, strings.Repeat("y", 1000)) {
		t.Fatal("the demoted oversized input reached the provider raw")
	}
	if strings.Count(request, "next-round small message") != 1 {
		t.Fatalf("the next round's input is not in its request exactly once:\n%.2000s", request)
	}
	if _, compacted := b.rows("oversized"); compacted != 1 {
		t.Fatalf("oversized row compacted = %d, want it summarized as history", compacted)
	}
}

// Over many rounds with compaction at work, each round's input reaches the
// model once and raw, the newest input is never summarized in its own round,
// and the compaction left behind is complete: no artifact is stuck in
// progress.
func TestDiscussRoundsKeepTheNewestInputRawWhileHistoryIsSummarized(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	const rounds = 8
	b := newBacklog(t)
	for round := range rounds {
		prefix := fmt.Sprintf("round-%d-", round)
		hw := b.write(b.input(prefix+"a", prefix+"a "+strings.Repeat("x", 1800)), b.input(prefix+"b", prefix+"b "+strings.Repeat("x", 1800)))

		request := b.answer(hw)

		if strings.Count(request, prefix+"b ") != 1 {
			t.Fatalf("round %d: the newest input is not in its request exactly once:\n%.2000s", round, request)
		}
		if _, compacted := b.rows(prefix + "b"); compacted != 0 {
			t.Fatalf("round %d: the newest input was summarized in its own round", round)
		}
	}
	var unfinished int
	if err := b.env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM bot_history_message_compacts WHERE session_id = $1 AND status <> 'ok'
	`, b.env.sessionID).Scan(&unfinished); err != nil {
		t.Fatal(err)
	}
	if unfinished != 0 {
		t.Fatalf("%d compaction artifacts are not complete", unfinished)
	}
	if b.session.Summarizer.CallCount() == 0 {
		t.Fatalf("history of %d rounds was never summarized", rounds)
	}
}

// discussContextWindow sets the context window of the run's model.
func discussContextWindow(tokens int) discussOption {
	return func(s *discussSetup) { s.contextWindow = tokens }
}

// discussCompaction summarizes the session's history with the real compaction
// service, the summarizer being the session's stand-in.
func discussCompaction(session *agenttest.CompactionSession) discussOption {
	return discussService(withCompact(func(d *compact.Deps) {
		d.Runner = withoutModelRecord{session.Compaction}
		d.HTTPClient = &http.Client{Transport: session.Summarizer}
	}))
}
