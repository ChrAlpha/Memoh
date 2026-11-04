package native

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/turn"
)

// MessageProvenance is what one message a run sends renders: the stored
// messages behind it, and whether the run keeps it where it is (its input, a
// steer) when it compacts. A message with no sources is runtime context.
type MessageProvenance struct {
	Sources []turn.ContextMessageSource
	Anchor  bool
}

// ContextSwitch is the compaction event an armed run asks for when the
// request before a step does not fit its allowance. Request is that request
// and Provenance what each of its messages renders (nil for runtime context
// the run rebuilds); the event decides what of it stays verbatim. Summarize
// answers a fork request through the run's own model, with the output
// settings the request carries, so a fork reads only what WindowTokens holds
// beside OutputTokens, the run's output allowance, as every request of the
// run does. Fold applies a pass the event committed to the request and
// reports the request as it then is. UsageTokens, AllowanceTokens and
// WindowTokens are in the units the run's estimate prices: the provider
// counts Density tokens for each of them.
type ContextSwitch struct {
	Request         sdk.Request
	Provenance      []*MessageProvenance
	UsageTokens     int
	AllowanceTokens int
	WindowTokens    int
	OutputTokens    int
	Density         float64
	Summarize       func(context.Context, sdk.Request) (sdk.ModelResult, error)
	Fold            func(ContextFold) ContextSplice
}

// ContextSplice is a request after the passes an event applied to it so far.
type ContextSplice struct {
	Request    sdk.Request
	Provenance []*MessageProvenance
	Tokens     int
}

// ContextFold is one pass an event committed: its summary, and the request
// it leaves by the messages of the request it read, the index of each that
// stays or -1 for the summary.
type ContextFold struct {
	SummaryID string
	Summary   sdk.Message
	Order     []int
}

// contextSwitchState follows an armed run's request across its dispatches:
// what each message renders, and, from a splice until the re-dispatch that
// follows it, where each message of the spliced request was before (-1 for a
// summary). The compiled prefix of a dispatch (after a promoted system
// message) holds still until the next splice, so it is known by position;
// what the loop rebuilds after it at each boundary (committed steps, inputs,
// hook and background messages) is known by what the steps committed. It also
// calibrates the run's estimate against the provider: density is how many
// tokens the provider counts for each token the estimate prices, from the
// latest step that reported its input (dispatched is what the estimate priced
// the request a step sent at), and starts from the session's last such step
// (RunConfig.ContextDensity).
type contextSwitchState struct {
	compiled   []MessageProvenance
	appended   []knownMessage
	moved      []int
	density    float64
	dispatched int
	// mismatch is set when the run compiled a request its provenance does
	// not describe; a fold of it fails rather than guess.
	mismatch error
}

// knownMessage is a message after the compiled prefix the run knows the
// provenance of.
type knownMessage struct {
	message    sdk.Message
	provenance MessageProvenance
}

// maxContextDensity bounds a calibration: a provider count far beyond any
// tokenizer's (a cumulative or misreported usage) must not fold every request
// to nothing.
const maxContextDensity = 4.0

func newContextSwitchState(cfg RunConfig) *contextSwitchState {
	if cfg.SwitchContext == nil {
		return nil
	}
	s := &contextSwitchState{compiled: slices.Clone(cfg.ContextProvenance), density: boundedContextDensity(cfg.ContextDensity)}
	if len(cfg.ContextProvenance) != len(cfg.Messages) {
		s.mismatch = fmt.Errorf("context provenance names %d messages, the request has %d", len(cfg.ContextProvenance), len(cfg.Messages))
	}
	return s
}

// boundedContextDensity is a density a calibration may apply: never below 1,
// where the estimate already holds, nor above maxContextDensity.
func boundedContextDensity(density float64) float64 {
	if !(density > 1) {
		return 1
	}
	return min(density, maxContextDensity)
}

// calibrated is a provider token count in the units the estimate prices: a
// request the estimate prices at calibrated(n) is one the provider counts at
// about n.
func (s *contextSwitchState) calibrated(tokens int) int {
	if s == nil || s.density <= 1 || tokens <= 0 {
		return tokens
	}
	return max(int(float64(tokens)/s.density), 1)
}

// staged records what the estimate prices the request a step sends at.
func (s *contextSwitchState) staged(tokens int) {
	if s == nil {
		return
	}
	s.dispatched = tokens
}

// observe calibrates the estimate by the input tokens the provider counted
// for the request the step sent, and reports what the estimate priced that
// request at; zero when either is unknown.
func (s *contextSwitchState) observe(usage sdk.Usage) int {
	if s == nil || s.dispatched <= 0 || usage.InputTokens <= 0 {
		return 0
	}
	s.density = boundedContextDensity(float64(usage.InputTokens) / float64(s.dispatched))
	return s.dispatched
}

func (s *contextSwitchState) know(message sdk.Message, provenance MessageProvenance) {
	messages := cloneProviderMessages([]sdk.Message{message})
	clearProviderCacheControls(messages)
	s.appended = append(s.appended, knownMessage{message: messages[0], provenance: provenance})
}

// committed records what a step persisted: messages are the ones it added to
// the request, in the order its commit persisted them with provenance.
func (s *contextSwitchState) committed(messages []sdk.Message, provenance []MessageProvenance) {
	if s == nil || len(provenance) == 0 || len(provenance) != len(messages) {
		return
	}
	for i, p := range provenance {
		s.know(messages[i], p)
	}
}

// align names what each message of a request renders: the compiled prefix,
// which starts at offset, by position, and the messages after it by the
// ones the run knows, in order. A message the run knows nothing about is
// runtime context and matches nil.
func (s *contextSwitchState) align(messages []sdk.Message, offset int) ([]*MessageProvenance, error) {
	if s.mismatch != nil {
		return nil, s.mismatch
	}
	end := offset + len(s.compiled)
	if end > len(messages) {
		return nil, fmt.Errorf("context provenance names %d compiled messages, the request has %d", len(s.compiled), len(messages)-offset)
	}
	out := make([]*MessageProvenance, len(messages))
	for i := range s.compiled {
		if p := &s.compiled[i]; len(p.Sources) > 0 || p.Anchor {
			out[offset+i] = p
		}
	}
	rest := cloneProviderMessages(messages[end:])
	clearProviderCacheControls(rest)
	next := 0
	for i := range rest {
		for j := next; j < len(s.appended); j++ {
			if reflect.DeepEqual(s.appended[j].message, rest[i]) {
				if p := &s.appended[j].provenance; len(p.Sources) > 0 || p.Anchor {
					out[end+i] = p
				}
				next = j + 1
				break
			}
		}
	}
	return out, nil
}

// fold runs one compaction event when params, priced at usage, does not fit
// allowance, which is calibrated, and splices each pass it commits into
// params; the event decides what stays verbatim. Every message no pass covers
// stays as it was. offset is where the compiled prefix starts in params:
// after a system message the prompt-cache plan promoted into it. It reports
// whether params changed and, when it did, what the request now costs; an
// event that leaves the request over its allowance fails as the event says.
// The event works in the estimate's units, so the window a fork reads within
// is calibrated too.
func (s *contextSwitchState) fold(ctx context.Context, cfg RunConfig, params *sdk.Request, offset, allowance, usage int) (bool, int, error) {
	if s == nil || allowance <= 0 || usage <= allowance {
		return false, usage, nil
	}
	provenance, err := s.align(params.Messages, offset)
	if err != nil {
		return false, usage, fmt.Errorf("%w: context switch: %w", contextfrag.ErrBudgetUnsatisfied, err)
	}
	window, reserve := providerWindow(cfg)
	window = s.calibrated(window)
	splice := requestSplice{messages: params.Messages, provenance: provenance, from: make([]int, len(params.Messages))}
	for i := range splice.from {
		splice.from[i] = i
	}
	// The system prompt and tools stay as they are; a pass reprices only the
	// messages.
	fixed := contextfrag.ProviderFixedEnvelopeTokens(params.System, params.Tools)
	tokens := usage
	applied := false
	sw := ContextSwitch{
		Request:         *params,
		Provenance:      provenance,
		UsageTokens:     usage,
		AllowanceTokens: allowance,
		WindowTokens:    window,
		OutputTokens:    reserve,
		Density:         s.density,
		Summarize:       forkSummarizer(cfg.Model, cfg.ModelIdleWindow),
		Fold: func(fold ContextFold) ContextSplice {
			splice.apply(fold)
			applied = true
			tokens = fixed + contextfrag.ProviderMessagesTokens(splice.messages)
			request := *params
			request.Messages = splice.messages
			return ContextSplice{Request: request, Provenance: splice.provenance, Tokens: tokens}
		},
	}
	detail, err := cfg.SwitchContext(ctx, sw)
	if err == nil {
		switch call, result := unpairedToolParts(splice.messages); {
		case call != "":
			err, detail = fmt.Errorf("tool call %s lost its result", call), "trigger=in_run reason=postcondition"
		case result != "":
			err, detail = fmt.Errorf("tool result %s lost its call", result), "trigger=in_run reason=postcondition"
		}
	}
	if detail != "" {
		cfg.ContextMutations.Record(contextfrag.MutationContextSwitch, detail)
	}
	if err != nil {
		if ctx.Err() != nil {
			return false, usage, ctx.Err()
		}
		if errors.Is(err, contextfrag.ErrProtectedContextOverflow) || errors.Is(err, contextfrag.ErrBudgetUnsatisfied) {
			return false, usage, err
		}
		return false, usage, fmt.Errorf("%w: context switch: %w", contextfrag.ErrBudgetUnsatisfied, err)
	}
	if !applied {
		return false, usage, nil
	}
	recordFoldedInputImages(cfg.ContextMutations, params.Messages, provenance, splice.from)
	params.Messages = splice.messages
	s.moved = splice.from
	// The next dispatch compiles the spliced request up to where the old
	// compiled prefix ended; the loop rebuilds what follows.
	end := s.movedCount(offset + len(s.compiled))
	s.compiled = make([]MessageProvenance, max(0, end-offset))
	for i := range s.compiled {
		if p := splice.provenance[offset+i]; p != nil {
			s.compiled[i] = *p
		}
	}
	s.appended = nil
	for i := end; i < len(splice.messages); i++ {
		var p MessageProvenance
		if splice.provenance[i] != nil {
			p = *splice.provenance[i]
		}
		s.know(splice.messages[i], p)
	}
	return true, tokens, nil
}

// requestSplice is a request an event rewrites pass by pass: its messages,
// what each renders, and where each was in the request the event started
// from.
type requestSplice struct {
	messages   []sdk.Message
	provenance []*MessageProvenance
	from       []int
}

// apply rewrites the request as the pass left it: the messages fold.Order
// keeps, in its order, with the summary in its place.
func (sp *requestSplice) apply(fold ContextFold) {
	summary := &MessageProvenance{Sources: []turn.ContextMessageSource{{Kind: "summary", ID: fold.SummaryID}}}
	var next requestSplice
	for _, i := range fold.Order {
		if i < 0 {
			next.messages, next.provenance, next.from = append(next.messages, fold.Summary), append(next.provenance, summary), append(next.from, -1)
			continue
		}
		next.messages, next.provenance, next.from = append(next.messages, sp.messages[i]), append(next.provenance, sp.provenance[i]), append(next.from, sp.from[i])
	}
	*sp = next
}

// recordFoldedInputImages records the inputs a fold summarized with their
// images: a summary carries no image, so they are omitted from then on. from
// is where each message of the spliced request was in messages.
func recordFoldedInputImages(ledger *contextfrag.MutationLedger, messages []sdk.Message, provenance []*MessageProvenance, from []int) {
	kept := make(map[int]bool, len(from))
	for _, i := range from {
		kept[i] = true
	}
	var omitted []string
	for i, message := range messages {
		p := provenance[i]
		if kept[i] || p == nil || !p.Anchor || !slices.ContainsFunc(message.Content, func(part sdk.MessagePart) bool {
			_, image := part.(sdk.ImagePart)
			return image
		}) {
			continue
		}
		for _, source := range p.Sources {
			if source.ID != "" {
				omitted = append(omitted, source.ID)
			}
		}
	}
	if len(omitted) > 0 && ledger != nil {
		ledger.Record(contextfrag.MutationCurrentInputImagesOmitted, "sources="+strings.Join(omitted, ","))
	}
}

// remap moves dynamic refs of the request before the last splice to the
// request it left.
func (s *contextSwitchState) remap(refs []dynamicSourceRef) []dynamicSourceRef {
	at := make(map[int]int, len(s.moved))
	for i, from := range s.moved {
		if from >= 0 {
			at[from] = i
		}
	}
	out := make([]dynamicSourceRef, 0, len(refs))
	for _, ref := range refs {
		if index, ok := at[ref.index]; ok {
			ref.index = index
			out = append(out, ref)
		}
	}
	return out
}

// redispatch is the configuration that dispatches params again after a
// splice, the way a mid-stream retry dispatches a failed call again: from the
// request as it now is, with the boundary's inputs admitted, and the prefix,
// cache breakpoints and stable prefix estimate moved with what the splice
// removed. It reports false when no splice happened since the last dispatch.
//
//nolint:gocritic // hugeParam: the run configuration and request are copied into the next dispatch.
func (s *contextSwitchState) redispatch(cfg RunConfig, dispatch generateDispatch, params sdk.Request, admitted []dynamicSourceRef) (RunConfig, bool) {
	if s == nil || s.moved == nil {
		return cfg, false
	}
	offset := 0
	if dispatch.systemPrepended {
		offset = 1
	}
	messages := cloneProviderMessages(params.Messages[offset:])
	clearProviderCacheControls(messages)
	plan := &cfg.ContextCachePlan
	stableBefore := messagesTokens(cfg.Messages[:min(plan.StableMessageCount, len(cfg.Messages))])
	cfg.Messages = messages
	cfg.retryDynamicRefs = shiftDynamicSourceRefs(admitted, -offset)
	cfg.initialProviderPrefixSet = true
	cfg.initialProviderMessageCount = s.movedCount(dispatch.initialMessageCount) - offset
	for _, count := range []*int{&plan.StableMessageCount, &plan.MidStableMessageCount} {
		if *count > 0 {
			*count = s.movedCount(*count+offset) - offset
		}
	}
	// The stable prefix is no longer the one the view compiled: its estimate
	// follows the messages the splice changed.
	if plan.StablePrefixTokenEstimate > 0 {
		plan.StablePrefixTokenEstimate += messagesTokens(messages[:min(plan.StableMessageCount, len(messages))]) - stableBefore
	}
	s.moved = nil
	return cfg, true
}

func messagesTokens(messages []sdk.Message) int {
	tokens := 0
	for _, message := range messages {
		tokens += contextfrag.EstimateSDKMessageTokens(message)
	}
	return tokens
}

// movedCount maps a message count of the request before the last splice,
// such as a prefix or a cache breakpoint, to the request it left: a summary
// counts as before the first message kept at or after count.
func (s *contextSwitchState) movedCount(count int) int {
	for i, from := range s.moved {
		if from >= count {
			return i
		}
	}
	return len(s.moved)
}

// unpairedToolParts names a tool call in messages that no tool result
// answers, and a tool result whose call no message makes.
func unpairedToolParts(messages []sdk.Message) (call, result string) {
	answered, called := map[string]bool{}, map[string]bool{}
	for _, message := range messages {
		for _, part := range message.Content {
			switch part := part.(type) {
			case sdk.ToolResultPart:
				answered[part.ToolCallID] = true
			case sdk.ToolCallPart:
				called[part.ToolCallID] = true
			}
		}
	}
	for _, message := range messages {
		for _, part := range message.Content {
			switch part := part.(type) {
			case sdk.ToolCallPart:
				if call == "" && !answered[part.ToolCallID] {
					call = part.ToolCallID
				}
			case sdk.ToolResultPart:
				if result == "" && !called[part.ToolCallID] {
					result = part.ToolCallID
				}
			}
		}
	}
	return call, result
}
