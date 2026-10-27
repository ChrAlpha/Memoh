package compaction

import (
	"fmt"
	"slices"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/turn"
)

const (
	// summaryShareOfAllowance bounds a summary to this fraction of what the
	// next request may take, so a fold leaves room for the turns after it
	// instead of refilling the request it just emptied.
	summaryShareOfAllowance = 4
	// anchorCapTokens is the most a fold keeps of a run's input and steers
	// verbatim, newest first; older anchors fold like any other message. A
	// small window keeps at most a quarter of its allowance.
	anchorCapTokens = 20000
	// minForkOutputTokens is the least output a fork request asks for. A pass
	// reads less of the request rather than ask for less.
	minForkOutputTokens = 1024
	// instructionNumberSlack covers the instruction growing by the digits of
	// the numbers it states.
	instructionNumberSlack = 8
	// forkInstructionReserve is what a fork keeps for its instruction when
	// it bounds the messages it reads.
	forkInstructionReserve = 1024
)

// AnchorPolicy is how much of a request's anchors a fold keeps verbatim,
// newest first; the rest fold like any other message.
type AnchorPolicy int

const (
	// AnchorsCapped keeps a run's input and steers within anchorCapTokens, so
	// the summary keeps room for the steps after it.
	AnchorsCapped AnchorPolicy = iota
	// AnchorsFirst keeps the inputs a run answers, a Discuss batch, while they
	// leave a tenth of the room beside the tail for the summary.
	AnchorsFirst
)

// anchorCap is the most a fold keeps of ev's anchors.
func anchorCap(ev Event) int {
	if ev.Anchors != AnchorsFirst {
		return min(anchorCapTokens, ev.Allowance/4)
	}
	var fixed []sdk.Message
	for i, message := range ev.Request.Messages {
		if i >= ev.Cut || len(ev.Sources[i].Sources) == 0 {
			fixed = append(fixed, message)
		}
	}
	room := ev.Allowance - forkTokens(sdk.Request{System: ev.Request.System, Messages: fixed, Tools: ev.Request.Tools})
	return room - room/10
}

// foldPlan is one pass worked out before the summarizer is asked: what it
// reads and folds, and how long its summary may be.
type foldPlan struct {
	// end is how much of the request the pass reads; cut is where the
	// caller's tail starts. A pass that reads less than the cut leaves the
	// rest to a later pass.
	end int
	cut int
	// folded are the sources the summary stands for, read every source of
	// the messages the pass reads, absorbed the summaries it replaces.
	folded   []turn.ContextMessageSource
	read     []turn.ContextMessageSource
	absorbed []string
	// replaced is what the folded messages and absorbed summaries take in a
	// request, room what the next request has left for the summary.
	replaced int
	room     int
	// maxTokens is the fork's output cap and stated the summary length the
	// instruction asks for, below it.
	maxTokens   int
	stated      int
	instruction string
	// keep marks the messages before end the pass leaves verbatim: the
	// anchors it keeps and the messages no context row holds.
	keep map[int]bool
	// rewrite marks a request with nothing left to fold but its summaries:
	// a pass only rewrites them to fit the room the next request has.
	rewrite bool
	// fork is what the pass sends its summarizer of the request before the
	// instruction (forkRead).
	fork []sdk.Message
}

func (p foldPlan) folds() bool { return len(p.folded) > 0 || len(p.absorbed) > 0 }

// order is ev's request after the pass, as Pass.Order names it, and where the
// tail starts in it. The pass replaces every message it reads that renders a
// source and that it does not keep: what it folds and the summaries it
// absorbs. The tail and runtime context stay.
func (p foldPlan) order(ev Event) ([]int, int) {
	order := make([]int, len(ev.Request.Messages))
	for i := range order {
		order[i] = i
	}
	order, _ = turn.PlaceSummary(order, -1, func(i int) bool {
		return i >= 0 && i < p.end && len(ev.Sources[i].Sources) > 0 && !p.keep[i]
	})
	cut := slices.Index(order, ev.Cut)
	if cut < 0 {
		cut = len(order)
	}
	return order, cut
}

// final reports whether the pass reads up to the tail, so its summary is the
// one the caller's next request carries.
func (p foldPlan) final() bool { return p.end == p.cut }

// bounded reports whether the pass's summary must fit the room the next
// request has for it: the final pass's, and a rewrite's, which makes
// progress only by leaving a summary that fits.
func (p foldPlan) bounded() bool { return p.final() || p.rewrite }

// planFold decides one pass over ev. Every message before the cut folds
// except runtime context, which has no sources, the messages no context row
// holds, and the anchors ev's policy keeps; summaries are absorbed. When
// nothing but summaries is left, a pass rewrites them only if they do not fit
// the room the next request has. The pass reads the longest prefix of the
// request, ending at a tool-group boundary, that leaves the fork room to
// answer: at least half of the summary what it reads may need, and never
// less than minForkOutputTokens.
func planFold(ev Event, unfoldable map[int]bool) (foldPlan, error) {
	messages, sources := ev.Request.Messages, ev.Sources
	kept := keptAnchors(messages, sources, ev.Cut, anchorCap(ev), unfoldable)
	for i := range unfoldable {
		kept[i] = true
	}
	capability := ev.MaxOutputTokens
	// A checkpoint absorbs every summary of its parent, so a pass reads past
	// the last one the request renders.
	summariesEnd := 0
	for i, source := range sources[:ev.Cut] {
		if isSummarySource(source) {
			summariesEnd = i + 1
		}
	}
	rewrite := len(spanPlan(ev, kept, ev.Cut).folded) == 0
	read, err := forkRead(ev)
	if err != nil {
		return foldPlan{}, err
	}
	var smaller error
	for end := ev.Cut; end > 0; end-- {
		if end < summariesEnd || !foldBoundary(messages, end) {
			continue
		}
		plan := spanPlan(ev, kept, end)
		if !plan.folds() {
			continue
		}
		plan.rewrite, plan.fork = rewrite, read[:end]
		err := plan.size(ev, capability)
		switch {
		case ReasonOf(err) == ReasonProtectedOverflow:
			return foldPlan{}, err
		case rewrite && plan.replaced <= plan.room:
			// Only summaries are left, and they fit: folding them again
			// would only rewrite them.
			return foldPlan{}, nil
		case err != nil:
			if smaller == nil {
				smaller = err
			}
			continue
		}
		need := max(minForkOutputTokens, min(ev.Allowance/summaryShareOfAllowance, plan.replaced)/2)
		if plan.maxTokens >= min(capability, need) {
			return plan, nil
		}
	}
	if smaller != nil {
		// What is left is too little for any summary to replace; once the
		// request fits, the event is done.
		if forkTokens(ev.Request) <= ev.Allowance {
			return foldPlan{}, nil
		}
		return foldPlan{}, smaller
	}
	if !spanPlan(ev, kept, ev.Cut).folds() {
		return foldPlan{}, nil
	}
	return foldPlan{}, failed(ReasonInputOverflow, fmt.Errorf("%w: no prefix of the request leaves the summarizer room to answer in a %d-token window", errCompactionInputOverflow, ev.Window))
}

// keptAnchors marks the anchors before the cut a fold keeps verbatim: the
// newest ones, while they fit the cap. An anchor the fold cannot stand for
// stays anyway and takes none of it.
func keptAnchors(messages []sdk.Message, sources []Source, cut, capTokens int, unfoldable map[int]bool) map[int]bool {
	kept := map[int]bool{}
	tokens := 0
	for i := cut - 1; i >= 0; i-- {
		if !sources[i].Anchor || unfoldable[i] {
			continue
		}
		cost := forkTokens(sdk.Request{Messages: messages[i : i+1]})
		if tokens+cost > capTokens {
			break
		}
		tokens += cost
		kept[i] = true
	}
	return kept
}

// foldBoundary reports whether a pass may end its read before message end: a
// tool result never starts what a pass leaves, so a call and its results
// stay on one side.
func foldBoundary(messages []sdk.Message, end int) bool {
	if end <= 0 || end > len(messages) {
		return false
	}
	if end == len(messages) {
		return true
	}
	for _, part := range messages[end].Content {
		if _, ok := part.(sdk.ToolResultPart); ok {
			return false
		}
	}
	return true
}

// spanPlan classifies the messages a pass reading up to end folds.
func spanPlan(ev Event, kept map[int]bool, end int) foldPlan {
	plan := foldPlan{end: end, cut: ev.Cut, keep: map[int]bool{}}
	read, folded := map[turn.ContextMessageSource]bool{}, map[turn.ContextMessageSource]bool{}
	for i := range end {
		source := ev.Sources[i]
		switch {
		case len(source.Sources) == 0:
			continue
		case isSummarySource(source):
			plan.absorbed = append(plan.absorbed, source.Sources[0].ID)
			continue
		}
		for _, s := range source.Sources {
			key := turn.ContextMessageSource{Kind: s.Kind, ID: s.ID}
			if !read[key] {
				read[key] = true
				plan.read = append(plan.read, key)
			}
		}
		if kept[i] {
			plan.keep[i] = true
			continue
		}
		for _, s := range source.Sources {
			key := turn.ContextMessageSource{Kind: s.Kind, ID: s.ID}
			if !folded[key] {
				folded[key] = true
				plan.folded = append(plan.folded, key)
			}
		}
	}
	return plan
}

// size prices the pass: what it replaces, the room the next request leaves
// its summary, the fork's output cap and the summary length to ask for.
func (p *foldPlan) size(ev Event, capability int) error {
	messages := ev.Request.Messages
	var replaced, keptMessages []sdk.Message
	for i, message := range messages {
		if i < p.end && len(ev.Sources[i].Sources) > 0 && !p.keep[i] {
			replaced = append(replaced, message)
		} else {
			keptMessages = append(keptMessages, message)
		}
	}
	p.replaced = forkTokens(sdk.Request{Messages: replaced})
	p.room = ev.Allowance - forkTokens(sdk.Request{System: ev.Request.System, Messages: keptMessages, Tools: ev.Request.Tools})
	prefix := forkTokens(sdk.Request{System: ev.Request.System, Messages: p.fork, Tools: ev.Request.Tools})
	p.instruction = forkInstruction(ev, p)
	p.maxTokens = min(capability, ev.Window-prefix-instructionTokens(p.instruction)-instructionNumberSlack)
	p.stated = min(p.maxTokens*3/4, ev.Allowance/summaryShareOfAllowance, summaryOutputLimit(p.replaced-1, capability))
	if p.bounded() {
		p.stated = min(p.stated, summaryOutputLimit(p.room, capability))
	}
	if p.bounded() && summaryOutputLimit(p.room, capability) < minSummaryTokens {
		return failed(ReasonProtectedOverflow, fmt.Errorf("%w: allowance=%d room=%d", errNoSummaryRoom, ev.Allowance, p.room))
	}
	if p.stated < minSummaryTokens && p.maxTokens >= minForkOutputTokens {
		return failed(ReasonIneffective, fmt.Errorf("%w: replaced_tokens=%d", errIneffectiveSummary, p.replaced))
	}
	p.instruction = forkInstruction(ev, p)
	return nil
}

func isSummarySource(source Source) bool {
	return len(source.Sources) > 0 && source.Sources[0].Kind == "summary"
}
