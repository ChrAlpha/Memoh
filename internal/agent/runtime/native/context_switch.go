package native

import (
	"context"
	"fmt"
	"sort"
	"strings"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	sdk "github.com/felinics/twilight/sdk"
)

type ContextSwitchSource struct {
	Ref      contextfrag.ContextRef
	Coverage []contextfrag.ContextRef
}

type ContextSwitchInput struct {
	RunID                string
	StepIndex            int
	Request              sdk.Request
	Sources              []ContextSwitchSource
	Protected            []bool
	InputAllowanceTokens int
}

type ContextSwitchResult struct {
	Messages      []sdk.Message
	SourceIndexes []int
	Sources       []ContextSwitchSource
	OnAdopted     func()
}

type contextSwitchOrigin struct {
	source     ContextSwitchSource
	protected  bool
	step       int
	background bool
}

type contextSwitchState struct {
	messages      []sdk.Message
	origins       []contextSwitchOrigin
	latestStep    int
	nextStep      int
	promoted      bool
	admitted      []dynamicSourceRef
	pending       []contextSwitchOrigin
	onAdopted     func()
	pendingDetail string
}

func newContextSwitchState(cfg RunConfig) *contextSwitchState {
	s := &contextSwitchState{messages: cloneProviderMessages(cfg.Messages), latestStep: -1, nextStep: cfg.StepIndexOffset}
	for i := range cfg.Messages {
		origin := contextSwitchOrigin{protected: true, step: -1}
		if len(cfg.ContextMessageSources) == len(cfg.Messages) {
			origin.source = cloneContextSwitchSource(cfg.ContextMessageSources[i])
			origin.protected = origin.source.Ref.ID == ""
		}
		if cfg.ContextCurrentUserMessageIndex != nil && i == *cfg.ContextCurrentUserMessageIndex {
			origin.protected = true
		}
		s.origins = append(s.origins, origin)
	}
	return s
}

func cloneContextSwitchRef(ref contextfrag.ContextRef) contextfrag.ContextRef {
	if ref.Range != nil {
		value := *ref.Range
		ref.Range = &value
	}
	return ref
}

func cloneContextSwitchSource(source ContextSwitchSource) ContextSwitchSource {
	source.Ref = cloneContextSwitchRef(source.Ref)
	coverage := make([]contextfrag.ContextRef, len(source.Coverage))
	for i := range coverage {
		coverage[i] = cloneContextSwitchRef(source.Coverage[i])
	}
	source.Coverage = coverage
	return source
}

func (s *contextSwitchState) align(messages []sdk.Message, promoted bool) []contextSwitchOrigin {
	origins := make([]contextSwitchOrigin, len(messages))
	for i := range origins {
		origins[i] = contextSwitchOrigin{protected: true, step: -1}
	}
	shift := 0
	if promoted && !s.promoted {
		shift = 1
	}
	if !promoted && s.promoted {
		shift = -1
	}
	for i := range s.messages {
		j := i + shift
		if j >= 0 && j < len(messages) && providerMessagesEqual(s.messages[i:i+1], messages[j:j+1]) {
			origins[j] = s.origins[i]
		}
	}
	return origins
}

func (s *contextSwitchState) prepare(ctx context.Context, cfg RunConfig, params *sdk.Request, promoted bool, allowance int, refs []dynamicSourceRef) ([]dynamicSourceRef, error) {
	origins := s.align(params.Messages, promoted)
	s.pending = origins
	s.onAdopted = nil
	s.pendingDetail = ""
	beforeTokens := contextfrag.ProviderEnvelopeTokens(params.System, params.Messages, params.Tools)
	if s.latestStep < 0 || allowance <= 0 || beforeTokens < allowance-allowance/5 {
		return refs, nil
	}
	input := ContextSwitchInput{RunID: cfg.RunID, StepIndex: s.nextStep, Request: *params, InputAllowanceTokens: allowance}
	input.Request.Messages = cloneProviderMessages(params.Messages)
	input.Request.Tools = append([]sdk.ToolDefinition(nil), params.Tools...)
	for _, origin := range origins {
		input.Sources = append(input.Sources, cloneContextSwitchSource(origin.source))
		input.Protected = append(input.Protected, origin.protected || origin.source.Ref.ID == "" || origin.step == s.latestStep)
	}
	result, err := cfg.SwitchContext(ctx, input)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result.Messages == nil {
		return refs, nil
	}
	if len(result.SourceIndexes) != len(result.Messages) || len(result.Sources) != len(result.Messages) {
		return nil, fmt.Errorf("%w: invalid context switch source vector", contextfrag.ErrBudgetUnsatisfied)
	}
	retained := make([]bool, len(origins))
	next := make([]contextSwitchOrigin, len(result.Messages))
	previous := -1
	var summaryRefs []string
	coverageCount := 0
	for i, source := range result.SourceIndexes {
		if source == -1 {
			if result.Sources[i].Ref.ID == "" {
				return nil, fmt.Errorf("%w: context switch summary has no source", contextfrag.ErrBudgetUnsatisfied)
			}
			next[i] = contextSwitchOrigin{source: cloneContextSwitchSource(result.Sources[i]), step: -1}
			summaryRefs = append(summaryRefs, result.Sources[i].Ref.StableKey())
			coverageCount += len(result.Sources[i].Coverage)
			continue
		}
		if source <= previous || source < 0 || source >= len(origins) || !providerMessagesEqual(params.Messages[source:source+1], result.Messages[i:i+1]) {
			return nil, fmt.Errorf("%w: context switch changed an input source", contextfrag.ErrBudgetUnsatisfied)
		}
		previous = source
		retained[source] = true
		next[i] = origins[source]
	}
	for i, origin := range origins {
		protected := origin.protected || origin.source.Ref.ID == "" || origin.step == s.latestStep
		if protected && !retained[i] {
			return nil, fmt.Errorf("%w: context switch removed protected input", contextfrag.ErrProtectedContextOverflow)
		}
	}
	afterTokens := contextfrag.ProviderEnvelopeTokens(params.System, result.Messages, params.Tools)
	if afterTokens >= beforeTokens || afterTokens > allowance {
		return refs, nil
	}
	refs = remapDynamicRefs(refs, params.Messages, result.Messages, 0, ContextStepSelectionResult{MessageSourceIndexes: result.SourceIndexes, MessageSourceIndexesKnown: true})
	params.Messages = cloneProviderMessages(result.Messages)
	s.pending = next
	s.onAdopted = result.OnAdopted
	s.pendingDetail = fmt.Sprintf("summary_refs=%s coverage_count=%d", strings.Join(summaryRefs, ","), coverageCount)
	return refs, nil
}

func (s *contextSwitchState) publish(params sdk.Request, promoted bool, admitted []dynamicSourceRef, ledger *contextfrag.MutationLedger) {
	if s == nil {
		return
	}
	s.messages = cloneProviderMessages(params.Messages)
	s.origins = s.pending
	s.pending = nil
	s.promoted = promoted
	s.admitted = cloneDynamicSourceRefs(admitted)
	if s.pendingDetail != "" {
		ledger.Record(contextfrag.MutationContextSwitch, fmt.Sprintf("step=%d input_tokens=%d %s", s.nextStep, contextfrag.ProviderEnvelopeTokens(params.System, params.Messages, params.Tools), s.pendingDetail))
		s.pendingDetail = ""
	}
	if s.onAdopted != nil {
		s.onAdopted()
		s.onAdopted = nil
	}
}

func (s *contextSwitchState) sourceIDs() []string {
	if s == nil {
		return nil
	}
	ids := make([]string, len(s.pending))
	for i, origin := range s.pending {
		if origin.source.Ref.Namespace == "bot_history_message" {
			ids[i] = origin.source.Ref.ID
		}
	}
	return ids
}

func (s *contextSwitchState) extend(messages []sdk.Message, dir StepDirective, additions, index int, complete bool) {
	if s == nil {
		return
	}
	valid := complete && len(dir.CommittedSources) == additions+len(messages)
	if valid && additions == len(s.admitted) {
		refs := cloneDynamicSourceRefs(s.admitted)
		sort.Slice(refs, func(i, j int) bool { return refs[i].recordID < refs[j].recordID })
		for i, ref := range refs {
			if ref.index >= 0 && ref.index < len(s.origins) {
				s.origins[ref.index].source = ContextSwitchSource{Ref: cloneContextSwitchRef(dir.CommittedSources[i])}
			}
		}
	}
	if complete {
		s.latestStep = index
	}
	s.nextStep = index + 1
	s.messages = append(s.messages, cloneProviderMessages(messages)...)
	for i := range messages {
		origin := contextSwitchOrigin{step: index, protected: !complete}
		if valid {
			origin.source.Ref = cloneContextSwitchRef(dir.CommittedSources[additions+i])
		}
		s.origins = append(s.origins, origin)
	}
}

func (s *contextSwitchState) removeBackground(messages []sdk.Message) []sdk.Message {
	if s == nil {
		return messages
	}
	origins := s.align(messages, s.promoted)
	kept := make([]sdk.Message, 0, len(messages))
	keptOrigins := make([]contextSwitchOrigin, 0, len(messages))
	for i, message := range messages {
		if origins[i].background {
			continue
		}
		kept = append(kept, message)
		keptOrigins = append(keptOrigins, origins[i])
	}
	s.messages = cloneProviderMessages(kept)
	s.origins = keptOrigins
	return kept
}

func (s *contextSwitchState) noteBackground(messages []sdk.Message) {
	if s == nil || len(messages) == 0 {
		return
	}
	s.origins = s.align(messages, s.promoted)
	s.origins[len(messages)-1].background = true
	s.messages = cloneProviderMessages(messages)
}
