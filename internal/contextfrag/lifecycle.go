package contextfrag

import "sync"

const MetadataContextLifecycleKey = "context_lifecycle"

type LifecycleHolder struct {
	mu       sync.Mutex
	manifest Manifest
	set      bool
}

func NewLifecycleHolder() *LifecycleHolder {
	return &LifecycleHolder{}
}

func (h *LifecycleHolder) SetManifest(manifest Manifest) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.manifest = manifest
	h.set = true
}

func (h *LifecycleHolder) Snapshot() (LifecycleSnapshot, bool) {
	if h == nil {
		return LifecycleSnapshot{}, false
	}
	h.mu.Lock()
	manifest := h.manifest
	ok := h.set
	h.mu.Unlock()
	if !ok {
		return LifecycleSnapshot{}, false
	}
	return BuildLifecycleSnapshot(manifest), true
}

func BuildLifecycleSnapshot(manifest Manifest) LifecycleSnapshot {
	snapshot := LifecycleSnapshot{
		Version:            1,
		View:               manifest.View,
		Counts:             manifest.Counts,
		Selection:          selectionSnapshot(manifest.Selection),
		RenderPrefixHash:   "",
		CacheReadTokens:    0,
		CacheWriteTokens:   0,
		StablePrefixHash:   "",
		StableMessageCount: 0,
	}
	if manifest.CachePlan != nil {
		snapshot.StablePrefixHash = manifest.CachePlan.StablePrefixHash
		snapshot.StableMessageCount = manifest.CachePlan.StableMessageCount
	}
	if manifest.Mutations != nil {
		snapshot.Mutations = manifest.Mutations.Records()
		snapshot.FinalInputHash = manifest.Mutations.FinalInputHash()
	}
	return snapshot
}

type LifecycleSnapshot struct {
	Version            int              `json:"version"`
	View               ManifestView     `json:"view,omitempty"`
	Counts             ManifestCounts   `json:"counts"`
	Selection          SelectionTrace   `json:"selection"`
	StablePrefixHash   string           `json:"stable_prefix_hash,omitempty"`
	StableMessageCount int              `json:"stable_message_count,omitempty"`
	RenderPrefixHash   string           `json:"rendered_prefix_hash"`
	CacheReadTokens    int              `json:"cache_read_tokens"`
	CacheWriteTokens   int              `json:"cache_write_tokens"`
	Mutations          []MutationRecord `json:"mutations,omitempty"`
	FinalInputHash     string           `json:"final_input_hash,omitempty"`
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
