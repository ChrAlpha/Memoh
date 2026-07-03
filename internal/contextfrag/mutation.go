package contextfrag

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	MutationInjectedMessage     MutationKind = "injected_message"
)

// MutationRecord is one ledger entry describing a post-render mutation.
type MutationRecord struct {
	Kind   MutationKind `json:"kind"`
	Detail string       `json:"detail,omitempty"`
}

// MutationLedger collects the post-render mutations applied to a run's
// context together with the hash of the first provider input, so the
// manifest chain from rendered payload to final model input stays auditable.
// All methods are nil-safe.
type MutationLedger struct {
	mu             sync.Mutex
	records        []MutationRecord
	finalInputHash string
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
	raw, err := json.Marshal(struct {
		System   string `json:"system"`
		Messages any    `json:"messages"`
	}{System: system, Messages: messages})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// MarshalJSON serializes the ledger snapshot so a manifest carrying it can be
// persisted or logged as one lifecycle document.
func (l *MutationLedger) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Records        []MutationRecord `json:"records,omitempty"`
		FinalInputHash string           `json:"final_input_hash,omitempty"`
	}{
		Records:        l.Records(),
		FinalInputHash: l.FinalInputHash(),
	})
}
