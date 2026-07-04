package contextfrag

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sync"
)

// MutationKind identifies a post-render context mutator: code that changes
// the provider payload after the context view rendered it.
type MutationKind string

const (
	MutationToolUsageAppend     MutationKind = "tool_usage_append"
	MutationBeforeModelCallHook MutationKind = "before_model_call_hook"
	MutationBackgroundSummary   MutationKind = "background_summary"
	MutationMidTaskPrune        MutationKind = "mid_task_prune"
	MutationLoopStepReselection MutationKind = "loop_step_reselection"
	MutationInjectedMessage     MutationKind = "injected_message"
)

// MutationRecord is one ledger entry describing a post-render mutation.
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
)

// CacheComparison links the rendered prefix of this run to the previous run
// in the same session, attributing prompt cache hits and misses in-process.
type CacheComparison struct {
	Outcome                  string `json:"outcome"`
	PrevAgeMs                int64  `json:"prev_age_ms,omitempty"`
	FirstStepCacheReadTokens int    `json:"first_step_cache_read_tokens,omitempty"`
}

type CacheUsageRecord struct {
	StepIndex          int `json:"step_index"`
	NoCacheTokens      int `json:"no_cache_tokens,omitempty"`
	CacheReadTokens    int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens   int `json:"cache_write_tokens,omitempty"`
	CacheWrite5mTokens int `json:"cache_write_5m_tokens,omitempty"`
	CacheWrite1hTokens int `json:"cache_write_1h_tokens,omitempty"`
}

// MutationLedger collects the post-render mutations applied to a run's
// context together with the hash of the first provider input, so the
// manifest chain from rendered payload to final model input stays auditable.
// All methods are nil-safe.
type MutationLedger struct {
	mu              sync.Mutex
	records         []MutationRecord
	cacheUsage      []CacheUsageRecord
	cacheComparison *CacheComparison
	finalInputHash  string
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
	return json.Marshal(struct {
		Records         []MutationRecord   `json:"records,omitempty"`
		CacheUsage      []CacheUsageRecord `json:"cache_usage,omitempty"`
		CacheComparison *CacheComparison   `json:"cache_comparison,omitempty"`
		FinalInputHash  string             `json:"final_input_hash,omitempty"`
	}{
		Records:         l.Records(),
		CacheUsage:      l.CacheUsageRecords(),
		CacheComparison: l.CacheComparisonValue(),
		FinalInputHash:  l.FinalInputHash(),
	})
}
