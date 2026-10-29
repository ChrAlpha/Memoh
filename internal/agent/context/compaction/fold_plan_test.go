package compaction

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/turn"
)

// planFoldByEveryRead is planFold pricing every read, longest first: the
// pass it finds is the one planFold must find.
func planFoldByEveryRead(ev Event, unfoldable map[int]bool) (foldPlan, error) {
	kept := map[int]bool{}
	for i := range ev.cut {
		kept[i] = unfoldable[i] || ev.keeps(ev.Sources[i])
	}
	summariesEnd := 0
	for i, source := range ev.Sources[:ev.cut] {
		if isSummarySource(source) {
			summariesEnd = i + 1
		}
	}
	rewrite := len(spanPlan(ev, kept, ev.cut).folded) == 0
	read, err := forkRead(ev)
	if err != nil {
		return foldPlan{}, err
	}
	var smaller error
	boundaries := foldBoundaries(ev.Request.Messages, ev.Sources)
	for end := ev.cut; end > 0; end-- {
		if end < summariesEnd || !boundaries[end] {
			continue
		}
		plan := spanPlan(ev, kept, end)
		if !plan.folds() {
			continue
		}
		plan.rewrite, plan.fork = rewrite, read[:end]
		err := plan.size(ev)
		switch {
		case ReasonOf(err) == ReasonProtectedOverflow:
			return foldPlan{}, err
		case rewrite && plan.replaced <= plan.room:
			return foldPlan{}, nil
		case err != nil:
			if smaller == nil {
				smaller = err
			}
			continue
		}
		if plan.answers(ev) {
			return plan, nil
		}
	}
	if smaller != nil {
		if forkTokens(ev.Request) <= ev.Allowance {
			return foldPlan{}, nil
		}
		return foldPlan{}, smaller
	}
	if !spanPlan(ev, kept, ev.cut).folds() {
		return foldPlan{}, nil
	}
	return foldPlan{}, failed(ReasonInputOverflow, errCompactionInputOverflow)
}

// randomEvent is a request of summaries, inputs, runtime context and call
// groups of random sizes, in a random window, with its retention decided.
func randomEvent(rng *rand.Rand) Event {
	var messages []sdk.Message
	var sources []Source
	add := func(message sdk.Message, source Source) {
		messages, sources = append(messages, message), append(sources, source)
	}
	row := func() []turn.ContextMessageSource {
		return []turn.ContextMessageSource{{Kind: "history", ID: uuid.NewString()}}
	}
	scale := 1 + rng.IntN(100)
	text := func(most int) string { return strings.Repeat("word ", rng.IntN(most*scale/100+1)+1) }
	for range rng.IntN(3) {
		add(SummaryMessage(text(2000)), Source{Sources: []turn.ContextMessageSource{{Kind: "summary", ID: uuid.NewString()}}})
	}
	for i := range rng.IntN(40) + 2 {
		switch rng.IntN(6) {
		case 5:
			// A decision result moved behind its projection renders the same
			// row as the rest of that row, after another call.
			result := row()
			projection, call := fmt.Sprintf("decision_%d", i), fmt.Sprintf("call_%d", i)
			add(callMessage(projection), Source{Sources: row()})
			add(resultMessage(projection, text(3000)), Source{Sources: result})
			add(callMessage(call), Source{Sources: row()})
			add(resultMessage(call, text(3000)), Source{Sources: result})
		case 0:
			add(sdk.UserMessage("INPUT "+text(800)), Source{Sources: row(), Anchor: rng.IntN(2) == 0})
		case 1:
			add(sdk.UserMessage("RUNTIME "+text(200)), Source{})
		case 2:
			add(sdk.AssistantMessage(text(1500)), Source{Sources: row()})
		default:
			id := fmt.Sprintf("call_%d", i)
			add(callMessage(id), Source{Sources: row()})
			add(resultMessage(id, text(6000)), Source{Sources: row()})
		}
	}
	window := 1000 + rng.IntN(60000)
	ev := Event{
		Request:         sdk.Request{System: "You are the bot. " + text(300), Messages: messages},
		Sources:         sources,
		Window:          window,
		Allowance:       200 + rng.IntN(window),
		MaxOutputTokens: 64 + rng.IntN(window/2),
		FixedOutput:     rng.IntN(3) == 0,
		AnchorPolicy:    AnchorPolicy(rng.IntN(2)),
	}
	ev.retention = retain(ev, nil)
	return ev
}

// Halving finds the pass that pricing every read finds, whatever the request
// and window: the same read, the same output, the same failure.
func TestPlanFoldFindsThePassEveryReadFinds(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // a fixed seed replays the same requests
	for i := range 100 {
		ev := randomEvent(rng)
		got, gotErr := planFold(ev, nil)
		want, wantErr := planFoldByEveryRead(ev, nil)
		assertRowsFoldWhole(t, ev, got)
		if got.end != want.end || got.maxTokens != want.maxTokens || ReasonOf(gotErr) != ReasonOf(wantErr) || (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("event %d (%d messages, cut %d, window %d, fixed %v): planned end=%d max_tokens=%d err=%v, want end=%d max_tokens=%d err=%v",
				i, len(ev.Request.Messages), ev.cut, ev.Window, ev.FixedOutput, got.end, got.maxTokens, gotErr, want.end, want.maxTokens, wantErr)
		}
	}
}

// assertRowsFoldWhole fails when plan folds a row it also leaves rendered:
// committing it would drop the part the request keeps.
func assertRowsFoldWhole(t *testing.T, ev Event, plan foldPlan) {
	t.Helper()
	folded := map[turn.ContextMessageSource]bool{}
	for _, source := range plan.folded {
		folded[source] = true
	}
	for i, source := range ev.Sources {
		for _, s := range source.Sources {
			if folded[turn.ContextMessageSource{Kind: s.Kind, ID: s.ID}] && (i >= plan.end || plan.keep[i]) {
				t.Fatalf("plan folds %s:%s but keeps message %d that renders it (end %d)", s.Kind, s.ID, i, plan.end)
			}
		}
	}
}

// A row the read path renders at two places, a moved decision result and
// the rest of the row after another call, folds whole or not at all: a pass
// that ended between them would commit the row as folded while the request
// keeps its second part, and a reload would lose that result.
func TestPlanFoldKeepsARowRenderedTwiceWhole(t *testing.T) {
	t.Parallel()
	source := func(id string) Source {
		return Source{Sources: []turn.ContextMessageSource{{Kind: "history", ID: id}}}
	}
	messages := []sdk.Message{
		callMessage("decision"), resultMessage("decision", strings.Repeat("approved output ", 600)),
		callMessage("call"), resultMessage("call", strings.Repeat("later output ", 600)),
		sdk.UserMessage("continue"),
	}
	sources := []Source{source("projection-row"), source("result-row"), source("call-row"), source("result-row"), {Sources: source("input-row").Sources, Anchor: true}}
	boundaries := foldBoundaries(messages, sources)
	for end, want := range []bool{false, false, false, false, true, true} {
		if boundaries[end] != want {
			t.Fatalf("boundary before message %d = %v, want %v", end, boundaries[end], want)
		}
	}
	ev := Event{
		Request:   sdk.Request{System: "You are the bot.", Messages: messages},
		Sources:   sources,
		Window:    8000,
		Allowance: 2500,

		MaxOutputTokens: 1000,
		AnchorPolicy:    AnchorsCapped,
	}
	ev.retention = retain(ev, nil)
	plan, err := planFold(ev, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertRowsFoldWhole(t, ev, plan)
	if plan.end != 4 {
		t.Fatalf("pass ends before message %d, want 4: the moved result's row reaches the second call group", plan.end)
	}
}

// planEvent is a long request of small messages in a small window: most
// reads leave the summarizer no room, so a planner that prices every read
// prices most of them.
func planEvent(n int) Event {
	messages := make([]sdk.Message, 0, n)
	sources := make([]Source, 0, n)
	for i := range n {
		messages = append(messages, sdk.UserMessage(fmt.Sprintf("message %d ", i)+strings.Repeat("x", 180)))
		sources = append(sources, Source{Sources: []turn.ContextMessageSource{{Kind: "history", ID: uuid.NewString()}}})
	}
	ev := Event{Request: sdk.Request{System: "You are the bot.", Messages: messages}, Sources: sources, Window: 8000, Allowance: 6000, MaxOutputTokens: 2000}
	ev.retention = retain(ev, nil)
	return ev
}

func BenchmarkPlanFold(b *testing.B) {
	for _, n := range []int{500, 1000, 2000, 4000} {
		ev := planEvent(n)
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			for b.Loop() {
				if _, err := planFold(ev, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
