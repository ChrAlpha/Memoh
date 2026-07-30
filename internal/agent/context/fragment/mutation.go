package contextfrag

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sync"
	"time"
)

// MutationKind identifies an auditable context assembly decision or
// post-render provider-payload mutation.
type MutationKind string

const (
	MutationBeforeModelCallHook   MutationKind = "before_model_call_hook"
	MutationBackgroundSummary     MutationKind = "background_summary"
	MutationMidTaskPrune          MutationKind = "mid_task_prune"
	MutationLoopStepReselection   MutationKind = "loop_step_reselection"
	MutationInjectedMessage       MutationKind = "injected_message"
	MutationContextViewFallback   MutationKind = "context_view_fallback"
	MutationContextBudgetFailure  MutationKind = "context_budget_failure"
	MutationContextBudgetDisabled MutationKind = "context_budget_disabled"
	MutationCapabilityGate        MutationKind = "capability_gate"
	MutationReadMedia             MutationKind = "read_media"
	MutationMidStreamRetry        MutationKind = "mid_stream_retry"
)

// MutationRecord is one entry in the context mutation ledger.
type MutationRecord struct {
	Kind   MutationKind `json:"kind"`
	Detail string       `json:"detail,omitempty"`
}

// Cache comparison outcomes: how this run's rendered stable prefix relates
// to the previous run of the same session.
const (
	CacheOutcomeFirstObservation = "first_observation"
	CacheOutcomeHit              = "hit"
	CacheOutcomeMissSamePrefix   = "miss_same_prefix"
	CacheOutcomeExpired          = "expired"
	CacheOutcomePrefixChanged    = "prefix_changed"
	CacheOutcomeModelChanged     = "model_changed"
)

// CacheComparison links the rendered prefix of this run to the previous run
// in the same session, attributing prompt cache hits and misses in-process.
type CacheComparison struct {
	Outcome                  string `json:"outcome"`
	PrevAgeMs                int64  `json:"prev_age_ms,omitempty"`
	FirstStepCacheReadTokens int    `json:"first_step_cache_read_tokens,omitempty"`
}

type CacheUsageRecord struct {
	Attempt            int `json:"attempt,omitempty"`
	StepIndex          int `json:"step_index"`
	NoCacheTokens      int `json:"no_cache_tokens,omitempty"`
	CacheReadTokens    int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens   int `json:"cache_write_tokens,omitempty"`
	CacheWrite5mTokens int `json:"cache_write_5m_tokens,omitempty"`
	CacheWrite1hTokens int `json:"cache_write_1h_tokens,omitempty"`
}

// Loop-selection modes: which strategy governed mid-run context reselection
// for this agent invocation.
const (
	LoopSelectionSuffixOnly  = "suffix_only"
	LoopSelectionLegacyPrune = "legacy_prune"
	// LoopSelectionSuffixOnlyShadow marks a run where the step reselector was
	// invoked but its result was never applied to the provider payload:
	// legacy mid-task pruning performed the actual mutation instead. Each
	// step's StepSnapshot still carries the reselector's would-be
	// Dropped/Truncated/DropReasons verdict with ReselectionApplied=false;
	// the paired MutationMidTaskPrune record carries the real change.
	LoopSelectionSuffixOnlyShadow = "suffix_only_shadow"
)

// StepSnapshot is one prepare-step's provider-input hash-chain entry: the
// hash of the payload actually sent after mid-task pruning/reselection ran
// for that step, plus what that step's reselection (or fallback prune) did,
// so a manifest reader can reconstruct step N's input and its provenance
// instead of only seeing the last surviving hash of the run.
type StepSnapshot struct {
	Attempt              int            `json:"attempt,omitempty"`
	StepIndex            int            `json:"step_index"`
	PostPrepareInputHash string         `json:"post_prepare_input_hash,omitempty"`
	ReselectionApplied   bool           `json:"reselection_applied,omitempty"`
	Dropped              int            `json:"dropped,omitempty"`
	Truncated            int            `json:"truncated,omitempty"`
	DropReasons          map[string]int `json:"drop_reasons,omitempty"`
}

// MutationLedger collects auditable context assembly decisions and post-render
// mutations together with the hash of the first provider input, so the manifest
// chain from rendered payload to final model input stays auditable.
// All methods are nil-safe.
type MutationLedger struct {
	mu                           sync.Mutex
	records                      []MutationRecord
	cacheUsage                   []CacheUsageRecord
	cacheComparison              *CacheComparison
	finalInputHash               string
	prevBoundaryHash             string
	comparatorPrefixMessageCount int
	peekedPrevCacheEntry         PeekedPrevCacheEntry
	steps                        []StepSnapshot
	attempt                      int
	model                        string
	clientType                   string
	loopSelectionMode            string
}

// PeekedPrevCacheEntry carries the previous-turn prefix-cache tracker entry
// observed via a non-mutating peek at build time (see
// agent.recordPrefixCacheBoundary), so the end-of-run comparison in
// agent.observePrefixCache compares against the snapshot this run actually
// saw when it started, rather than re-reading the tracker at the end of the
// run — which could have been overwritten by a concurrent run of the same
// session between this run's peek and its observe.
type PeekedPrevCacheEntry struct {
	Found       bool
	Hash        string
	Model       string
	StableCount int
	At          time.Time
}

func NewMutationLedger() *MutationLedger {
	return &MutationLedger{}
}

func (l *MutationLedger) Record(kind MutationKind, detail string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, MutationRecord{Kind: kind, Detail: detail})
}

func (l *MutationLedger) Records() []MutationRecord {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]MutationRecord, len(l.records))
	copy(out, l.records)
	return out
}

func (l *MutationLedger) RecordCacheUsage(record CacheUsageRecord) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	record.Attempt = l.attempt
	l.cacheUsage = append(l.cacheUsage, record)
}

func (l *MutationLedger) CacheUsageRecords() []CacheUsageRecord {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]CacheUsageRecord, len(l.cacheUsage))
	copy(out, l.cacheUsage)
	return out
}

func (l *MutationLedger) SetCacheComparison(comparison CacheComparison) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cacheComparison = &comparison
}

func (l *MutationLedger) CacheComparisonValue() *CacheComparison {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cacheComparison == nil {
		return nil
	}
	out := *l.cacheComparison
	return &out
}

func (l *MutationLedger) SetFinalInputHash(hash string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.finalInputHash = hash
}

func (l *MutationLedger) FinalInputHash() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.finalInputHash
}

// SetPrevBoundaryHash carries the previous turn's rendered stable-prefix
// hash, re-hashed against this turn's messages, from buildGenerateOptions to
// observePrefixCache within the same run. It is same-run, in-memory only and
// intentionally excluded from MarshalJSON: it has no lifecycle/swagger
// surface, it only feeds the in-process cache comparator.
func (l *MutationLedger) SetPrevBoundaryHash(hash string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prevBoundaryHash = hash
}

func (l *MutationLedger) PrevBoundaryHash() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.prevBoundaryHash
}

// SetPeekedPrevCacheEntry stores the previous-turn tracker entry this run
// observed via peek at build time. Same-run, in-memory only; excluded from
// MarshalJSON like PrevBoundaryHash above.
func (l *MutationLedger) SetPeekedPrevCacheEntry(entry PeekedPrevCacheEntry) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.peekedPrevCacheEntry = entry
}

func (l *MutationLedger) PeekedPrevCacheEntry() PeekedPrevCacheEntry {
	if l == nil {
		return PeekedPrevCacheEntry{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.peekedPrevCacheEntry
}

// SetComparatorPrefixMessageCount carries this turn's cache-comparator
// prefix message count from buildGenerateOptions to observePrefixCache
// within the same run. Same-run, in-memory only; excluded from MarshalJSON
// like PrevBoundaryHash above.
func (l *MutationLedger) SetComparatorPrefixMessageCount(count int) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.comparatorPrefixMessageCount = count
}

func (l *MutationLedger) ComparatorPrefixMessageCount() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.comparatorPrefixMessageCount
}

// AppendStepSnapshot records one prepare-step's hash-chain entry, stamping
// it with the ledger's current attempt so steps across a mid-stream retry
// stay distinguishable from the pre-retry attempt.
func (l *MutationLedger) AppendStepSnapshot(s StepSnapshot) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s.Attempt = l.attempt
	l.steps = append(l.steps, s)
}

func (l *MutationLedger) StepSnapshots() []StepSnapshot {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]StepSnapshot, len(l.steps))
	copy(out, l.steps)
	return out
}

// AdvanceAttempt marks the start of a new mid-stream retry attempt: every
// StepSnapshot and CacheUsageRecord recorded afterward is stamped with the
// new attempt number, so records from different attempts sharing the same
// StepIndex/step_index stay distinguishable. Returns the new attempt number.
func (l *MutationLedger) AdvanceAttempt() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempt++
	return l.attempt
}

func (l *MutationLedger) SetModelInfo(model, clientType string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.model = model
	l.clientType = clientType
}

func (l *MutationLedger) ModelInfo() (string, string) {
	if l == nil {
		return "", ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.model, l.clientType
}

func (l *MutationLedger) SetLoopSelectionMode(mode string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loopSelectionMode = mode
}

func (l *MutationLedger) LoopSelectionMode() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.loopSelectionMode
}

// ProviderInputHash hashes the assembled provider payload (system prompt
// plus message stream) deterministically.
func ProviderInputHash(system string, messages any) string {
	hash, _ := ProviderPayloadHashAndBytes(system, messages, nil)
	return hash
}

func ProviderPayloadHashAndBytes(system string, messages any, tools any) (string, int) {
	tools = nilIfEmptyValue(tools)
	raw, err := json.Marshal(struct {
		System   string `json:"system"`
		Messages any    `json:"messages"`
		Tools    any    `json:"tools,omitempty"`
	}{System: system, Messages: messages, Tools: tools})
	if err != nil {
		return "", 0
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), len(raw)
}

func nilIfEmptyValue(value any) any {
	if value == nil {
		return nil
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if rv.IsNil() {
			return nil
		}
	}
	return value
}

// MarshalJSON serializes the ledger snapshot so a manifest carrying it can be
// persisted or logged as one lifecycle document.
func (l *MutationLedger) MarshalJSON() ([]byte, error) {
	model, clientType := l.ModelInfo()
	return json.Marshal(struct {
		Records           []MutationRecord   `json:"records,omitempty"`
		CacheUsage        []CacheUsageRecord `json:"cache_usage,omitempty"`
		CacheComparison   *CacheComparison   `json:"cache_comparison,omitempty"`
		FinalInputHash    string             `json:"final_input_hash,omitempty"`
		Steps             []StepSnapshot     `json:"steps,omitempty"`
		Model             string             `json:"model,omitempty"`
		ClientType        string             `json:"client_type,omitempty"`
		LoopSelectionMode string             `json:"loop_selection_mode,omitempty"`
	}{
		Records:           l.Records(),
		CacheUsage:        l.CacheUsageRecords(),
		CacheComparison:   l.CacheComparisonValue(),
		FinalInputHash:    l.FinalInputHash(),
		Steps:             l.StepSnapshots(),
		Model:             model,
		ClientType:        clientType,
		LoopSelectionMode: l.LoopSelectionMode(),
	})
}
