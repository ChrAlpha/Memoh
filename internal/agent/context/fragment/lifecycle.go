package contextfrag

import (
	"encoding/json"
	"strings"
	"sync"
)

const MetadataContextLifecycleKey = "context_lifecycle"

// LifecycleSnapshotFromMetadata extracts the persisted lifecycle snapshot
// from a message metadata JSON payload, reporting whether one was present.
func LifecycleSnapshotFromMetadata(raw []byte) (LifecycleSnapshot, bool) {
	if len(raw) == 0 {
		return LifecycleSnapshot{}, false
	}
	var metadata struct {
		ContextLifecycle *LifecycleSnapshot `json:"context_lifecycle"`
	}
	if json.Unmarshal(raw, &metadata) != nil || metadata.ContextLifecycle == nil {
		return LifecycleSnapshot{}, false
	}
	return *metadata.ContextLifecycle, true
}

const maxMemoryRecallTraceRefs = 32

type LifecycleHolder struct {
	mu       sync.Mutex
	manifest Manifest
	memory   *MemoryRecallTrace
	set      bool
}

func (h *LifecycleHolder) SetMemoryRecall(trace MemoryRecallTrace) {
	if h == nil {
		return
	}
	trace.Result.Refs = normalizeMemoryRecallRefs(trace.Result.Refs)
	h.mu.Lock()
	h.memory = cloneMemoryRecallTrace(&trace)
	h.set = true
	h.mu.Unlock()
}

func NewLifecycleHolder() *LifecycleHolder {
	return &LifecycleHolder{}
}

func (h *LifecycleHolder) SetManifest(manifest Manifest) {
	if h == nil {
		return
	}
	manifest = cloneLifecycleManifest(manifest)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.manifest = manifest
	h.set = true
}

func cloneLifecycleManifest(manifest Manifest) Manifest {
	out := manifest
	out.Breakdown = append([]KindBreakdown(nil), manifest.Breakdown...)
	out.TrustBreakdown = append([]TrustBreakdown(nil), manifest.TrustBreakdown...)
	out.ToolDefs = append([]ToolDefAccounting(nil), manifest.ToolDefs...)
	if manifest.Selection != nil {
		selection := *manifest.Selection
		if manifest.Selection.DropReasons != nil {
			selection.DropReasons = make(map[string]int, len(manifest.Selection.DropReasons))
			for reason, count := range manifest.Selection.DropReasons {
				selection.DropReasons[reason] = count
			}
		}
		out.Selection = &selection
	}
	if manifest.CachePlan != nil {
		cachePlan := *manifest.CachePlan
		out.CachePlan = &cachePlan
	}
	return out
}

func (h *LifecycleHolder) Snapshot() (LifecycleSnapshot, bool) {
	if h == nil {
		return LifecycleSnapshot{}, false
	}
	h.mu.Lock()
	manifest := h.manifest
	memory := cloneMemoryRecallTrace(h.memory)
	ok := h.set
	h.mu.Unlock()
	if !ok {
		return LifecycleSnapshot{}, false
	}
	snapshot := BuildLifecycleSnapshot(manifest)
	snapshot.MemoryRecall = memory
	return snapshot, true
}

func BuildLifecycleSnapshot(manifest Manifest) LifecycleSnapshot {
	snapshot := LifecycleSnapshot{
		Version:                     1,
		View:                        manifest.View,
		Counts:                      manifest.Counts,
		Breakdown:                   append([]KindBreakdown(nil), manifest.Breakdown...),
		TrustBreakdown:              append([]TrustBreakdown(nil), manifest.TrustBreakdown...),
		ToolDefs:                    append([]ToolDefAccounting(nil), manifest.ToolDefs...),
		Selection:                   selectionSnapshot(manifest.Selection),
		CacheComparatorPrefixHash:   "",
		DecoratedProviderPrefixHash: "",
		CacheReadTokens:             0,
		CacheWriteTokens:            0,
		StablePrefixHash:            "",
		StableMessageCount:          0,
	}
	if manifest.CachePlan != nil {
		snapshot.StablePrefixHash = manifest.CachePlan.StablePrefixHash
		snapshot.StableMessageCount = manifest.CachePlan.StableMessageCount
		snapshot.CacheComparatorPrefixHash = manifest.CachePlan.CacheComparatorPrefixHash
		snapshot.DecoratedProviderPrefixHash = manifest.CachePlan.DecoratedProviderPrefixHash
	}
	if manifest.Mutations != nil {
		snapshot.Mutations = manifest.Mutations.Records()
		snapshot.FinalInputHash = manifest.Mutations.FinalInputHash()
		snapshot.CacheComparison = manifest.Mutations.CacheComparisonValue()
		snapshot.CacheUsage = manifest.Mutations.CacheUsageRecords()
		for _, record := range snapshot.CacheUsage {
			snapshot.CacheReadTokens += record.CacheReadTokens
			snapshot.CacheWriteTokens += record.CacheWriteTokens
		}
		snapshot.Model, snapshot.ClientType = manifest.Mutations.ModelInfo()
		snapshot.LoopSelectionMode = manifest.Mutations.LoopSelectionMode()
		snapshot.Steps = manifest.Mutations.StepSnapshots()
	}
	return snapshot
}

type LifecycleSnapshot struct {
	Version                     int                 `json:"version"`
	View                        ManifestView        `json:"view,omitempty"`
	Counts                      ManifestCounts      `json:"counts"`
	Breakdown                   []KindBreakdown     `json:"breakdown,omitempty"`
	TrustBreakdown              []TrustBreakdown    `json:"trust_breakdown,omitempty"`
	ToolDefs                    []ToolDefAccounting `json:"tool_defs,omitempty"`
	Selection                   SelectionTrace      `json:"selection"`
	StablePrefixHash            string              `json:"stable_prefix_hash,omitempty"`
	StableMessageCount          int                 `json:"stable_message_count,omitempty"`
	CacheComparatorPrefixHash   string              `json:"cache_comparator_prefix_hash"`
	DecoratedProviderPrefixHash string              `json:"decorated_provider_prefix_hash,omitempty"`
	CacheReadTokens             int                 `json:"cache_read_tokens"`
	CacheWriteTokens            int                 `json:"cache_write_tokens"`
	CacheUsage                  []CacheUsageRecord  `json:"cache_usage,omitempty"`
	CacheComparison             *CacheComparison    `json:"cache_comparison,omitempty"`
	Mutations                   []MutationRecord    `json:"mutations,omitempty"`
	FinalInputHash              string              `json:"final_input_hash,omitempty"`
	Model                       string              `json:"model,omitempty"`
	ClientType                  string              `json:"client_type,omitempty"`
	LoopSelectionMode           string              `json:"loop_selection_mode,omitempty"`
	Steps                       []StepSnapshot      `json:"steps,omitempty"`
	MemoryRecall                *MemoryRecallTrace  `json:"memory_recall,omitempty"`
}

type MemoryRecallTrace struct {
	ProviderID     string                  `json:"provider_id"`
	MemoryVersion  string                  `json:"memory_version,omitempty"`
	CacheState     string                  `json:"cache_state"`
	RetrievalMode  string                  `json:"retrieval_mode,omitempty"`
	FallbackReason string                  `json:"fallback_reason,omitempty"`
	Query          MemoryRecallQueryTrace  `json:"query"`
	Result         MemoryRecallResultTrace `json:"result"`
}

type MemoryRecallQueryTrace struct {
	Source         string `json:"source"`
	RecentMessages int    `json:"recent_messages"`
	Truncated      bool   `json:"truncated"`
}

type MemoryRecallResultTrace struct {
	Count        int      `json:"count"`
	Refs         []string `json:"refs,omitempty"`
	ContextBytes int      `json:"context_bytes"`
}

func cloneMemoryRecallTrace(trace *MemoryRecallTrace) *MemoryRecallTrace {
	if trace == nil {
		return nil
	}
	out := *trace
	out.Result.Refs = append([]string(nil), trace.Result.Refs...)
	return &out
}

func normalizeMemoryRecallRefs(refs []string) []string {
	out := make([]string, 0, min(len(refs), maxMemoryRecallTraceRefs))
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
		if len(out) == maxMemoryRecallTraceRefs {
			break
		}
	}
	return out
}

func selectionSnapshot(selection *SelectionTrace) SelectionTrace {
	if selection == nil {
		return SelectionTrace{}
	}
	out := *selection
	if len(selection.DropReasons) > 0 {
		out.DropReasons = make(map[string]int, len(selection.DropReasons))
		for reason, count := range selection.DropReasons {
			out.DropReasons[reason] = count
		}
	}
	return out
}
