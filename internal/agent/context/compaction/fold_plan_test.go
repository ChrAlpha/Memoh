package compaction

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/jsonschema-go/jsonschema"
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
	price := pricePass(ev, read)
	var smaller error
	for end := ev.cut; end > 0; end-- {
		if end < summariesEnd || !foldBoundary(ev.Request.Messages, end) {
			continue
		}
		plan := spanPlan(ev, kept, end)
		if !plan.folds() {
			continue
		}
		plan.rewrite, plan.fork = rewrite, read[:end]
		err := plan.size(ev, price)
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
		if plan.answers(ev, price) {
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
		switch rng.IntN(5) {
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
		if got.end != want.end || got.maxTokens != want.maxTokens || ReasonOf(gotErr) != ReasonOf(wantErr) || (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("event %d (%d messages, cut %d, window %d, fixed %v): planned end=%d max_tokens=%d err=%v, want end=%d max_tokens=%d err=%v",
				i, len(ev.Request.Messages), ev.cut, ev.Window, ev.FixedOutput, got.end, got.maxTokens, gotErr, want.end, want.maxTokens, wantErr)
		}
	}
}

// A pass prices every read it weighs by what it priced once: the request,
// its room, what a read replaces, what it leaves and the fork it sends take
// what forkTokens prices those payloads at.
func TestPassPricesEveryReadAsForkTokensDoes(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // a fixed seed replays the same requests
	tools := []sdk.ToolDefinition{{Name: "read", Description: "read <path> & \"quote\"", Parameters: &jsonschema.Schema{
		Type:       "object",
		Properties: map[string]*jsonschema.Schema{"path": {Type: "string", Description: "the file to read"}},
	}}}
	for i := range 60 {
		ev := randomEvent(rng)
		ev.Request.Tools = tools
		read, err := forkRead(ev)
		if err != nil {
			continue
		}
		price := pricePass(ev, read)
		if price.request() != forkTokens(ev.Request) || price.room != fixedRoom(ev) || price.summaryBudget() != summaryBudget(ev) {
			t.Fatalf("event %d: priced request=%d room=%d summary=%d, want %d %d %d",
				i, price.request(), price.room, price.summaryBudget(), forkTokens(ev.Request), fixedRoom(ev), summaryBudget(ev))
		}
		kept := map[int]bool{}
		for j := range ev.cut {
			kept[j] = ev.keeps(ev.Sources[j])
		}
		for end := 0; end <= ev.cut; end++ {
			p := spanPlan(ev, kept, end)
			p.fork = read[:end]
			_ = p.size(ev, price)
			var replaced, left []sdk.Message
			for j, message := range ev.Request.Messages {
				if j < end && len(ev.Sources[j].Sources) > 0 && !p.keep[j] {
					replaced = append(replaced, message)
				} else {
					left = append(left, message)
				}
			}
			room := ev.Allowance - forkTokens(sdk.Request{System: ev.Request.System, Messages: left, Tools: ev.Request.Tools})
			fork := forkTokens(sdk.Request{System: ev.Request.System, Messages: p.fork, Tools: ev.Request.Tools})
			if p.replaced != forkTokens(sdk.Request{Messages: replaced}) || p.room != room || price.fork(end) != fork {
				t.Fatalf("event %d, read %d: priced replaced=%d room=%d fork=%d, want %d %d %d",
					i, end, p.replaced, p.room, price.fork(end), forkTokens(sdk.Request{Messages: replaced}), room, fork)
			}
		}
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
