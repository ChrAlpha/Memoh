// Package migrate contains utilities that convert existing markdown-backed
// memory content into PostgreSQL wiki nodes (memory_nodes) and derive the
// relations between them. The conversion logic is backend-agnostic: it
// produces plain NodeSpec/EdgeSpec values so it can be exercised by unit tests
// without a live database, and persisted by any concrete store implementation.
package migrate

import (
	"fmt"
	"sort"
	"strings"
	"time"

	memslug "github.com/felinics/memoh/internal/memory/slug"
	storefs "github.com/felinics/memoh/internal/memory/storefs"
)

// MemoryLayer is the canonical layer a memory node belongs to. Existing flat
// markdown entries that carry no layer hint are classified as LayerNote.
type MemoryLayer string

const (
	LayerPreference MemoryLayer = "preference"
	LayerIdentity   MemoryLayer = "identity"
	LayerContext    MemoryLayer = "context"
	LayerExperience MemoryLayer = "experience"
	LayerActivity   MemoryLayer = "activity"
	LayerPersona    MemoryLayer = "persona"
	LayerNote       MemoryLayer = "note"
)

// EdgeRel is the canonical relationship type between two memory nodes.
type EdgeRel string

const (
	EdgeSameProfile EdgeRel = "same_profile"
	EdgeSameTopic   EdgeRel = "same_topic"
	EdgeSameDay     EdgeRel = "same_day"
	EdgeRefs        EdgeRel = "refs"
)

// NodeSpec is a backend-agnostic description of a memory_nodes row, produced
// from a storefs.MemoryItem. Concrete store (PostgreSQL) translates this into
// their sqlc UpsertMemoryNodeParams.
type NodeSpec struct {
	ID               string
	BotID            string
	Body             string
	Hash             string
	Layer            MemoryLayer
	FactType         string
	Subject          string
	Confidence       float32
	Metadata         map[string]any
	SourceMessageIDs []string
	ProfileRef       string
	Topic            string
	CapturedAt       time.Time
	ExpiresAt        time.Time // zero value means "no expiry".
}

// EdgeSpec is one related pair of memory nodes, as expanded by
// Relations.Edges.
type EdgeSpec struct {
	BotID   string
	SrcNode string
	DstNode string
	Rel     EdgeRel
	Weight  float32
}

// Result summarises a wiki backfill pass for one bot.
type Result struct {
	BotID      string
	NodeCount  int
	EdgeCount  int
	LayerBreak map[MemoryLayer]int
}

// Plan converts a bot's markdown memory items into wiki node specs plus the
// derived edges from shared profile_ref/topic/captured day and explicit wiki
// cross-references in node bodies.
// It performs no I/O and is safe to call in dry-run mode.
//
// Layer classification is intentionally conservative: items without any hint
// fall back to LayerNote. This keeps the backfill deterministic and reversible
// until the typed-facts formation (P1) starts emitting explicit layers.
func Plan(botID string, items []storefs.MemoryItem) ([]NodeSpec, []EdgeSpec) {
	nodes := make([]NodeSpec, 0, len(items))
	for _, item := range items {
		nodes = append(nodes, nodeFromItem(botID, item))
	}
	return nodes, PlanFromNodes(nodes)
}

// PlanFromNodes expands the relations of an existing node set into one edge per
// related pair, for views that draw every edge. Recall works on DeriveRelations
// directly: a shared profile, topic, or day relates every pair of its nodes, so
// the pairwise form grows quadratically with the group.
func PlanFromNodes(nodes []NodeSpec) []EdgeSpec {
	return DeriveRelations(nodes).Edges(nodes)
}

// Summarise returns a Result for a planned node/edge set, suitable for CLI
// dry-run reporting.
func Summarise(botID string, nodes []NodeSpec, edges []EdgeSpec) Result {
	r := Result{BotID: botID, NodeCount: len(nodes), EdgeCount: len(edges), LayerBreak: map[MemoryLayer]int{}}
	for _, n := range nodes {
		layer := n.Layer
		if layer == "" {
			layer = LayerNote
		}
		r.LayerBreak[layer]++
	}
	return r
}

func nodeFromItem(botID string, item storefs.MemoryItem) NodeSpec {
	body := strings.TrimSpace(item.Memory)
	layer := classifyLayer(item)
	topic := metadataString(item.Metadata, "topic")
	profileRef := metadataString(item.Metadata, "profile_ref")
	if profileRef == "" {
		profileRef = metadataString(item.Metadata, "profile_user_id")
	}
	captured := parseTime(item.CreatedAt)
	if captured.IsZero() {
		captured = parseTime(item.UpdatedAt)
	}
	if captured.IsZero() {
		captured = time.Now().UTC()
	}
	return NodeSpec{
		ID:         strings.TrimSpace(item.ID),
		BotID:      botID,
		Body:       body,
		Hash:       strings.TrimSpace(item.Hash),
		Layer:      layer,
		Subject:    metadataString(item.Metadata, "subject"),
		Confidence: metadataFloat(item.Metadata, "confidence", 0.5),
		Metadata:   cloneMetadata(item.Metadata),
		ProfileRef: profileRef,
		Topic:      topic,
		CapturedAt: captured,
	}
}

// classifyLayer maps a flat memory item to a canonical layer using light
// metadata heuristics. Items that already declare a `layer` metadata key are
// honoured (validated against the known set); otherwise the item defaults to
// LayerNote. This is deliberately non-magical: real classification happens
// later in the typed-facts formation (P1).
func classifyLayer(item storefs.MemoryItem) MemoryLayer {
	if raw := metadataString(item.Metadata, "layer"); raw != "" {
		switch MemoryLayer(strings.ToLower(strings.TrimSpace(raw))) {
		case LayerPreference, LayerIdentity, LayerContext, LayerExperience, LayerActivity, LayerPersona, LayerNote:
			return MemoryLayer(raw)
		}
	}
	return LayerNote
}

// Relation weights. A shared profile or an explicit reference relates two
// memories most strongly, a shared day least.
const (
	sameProfileWeight float32 = 1.0
	sameTopicWeight   float32 = 0.8
	sameDayWeight     float32 = 0.5
	refsWeight        float32 = 1.0
)

// Relations is the relation structure derived from a node set. Nodes are
// referred to by their index in that set.
type Relations struct {
	// Groups relate every pair of their members: the nodes sharing one
	// profile_ref, one topic, or one capture day. Each node belongs to at most
	// one group per relation.
	Groups []RelationGroup
	// Refs are the [[slug]] cross-references from one node body to another
	// node, one per (src, dst) pair.
	Refs []RelationRef
}

// RelationGroup is a set of at least two nodes that are pairwise related.
type RelationGroup struct {
	Rel     EdgeRel
	Weight  float32
	Members []int
}

// RelationRef is a reference from the node at Src to the node at Dst.
type RelationRef struct {
	Src, Dst int
	Weight   float32
}

// DeriveRelations derives the relations of nodes from their profile_ref,
// topic, capture day, and body links. It runs in time linear in the node
// count and body size.
func DeriveRelations(nodes []NodeSpec) Relations {
	if len(nodes) < 2 {
		return Relations{}
	}
	var rel Relations
	rel.Groups = appendGroups(rel.Groups, nodes, EdgeSameProfile, sameProfileWeight, func(n NodeSpec) string { return n.ProfileRef })
	rel.Groups = appendGroups(rel.Groups, nodes, EdgeSameTopic, sameTopicWeight, func(n NodeSpec) string { return n.Topic })
	rel.Groups = appendGroups(rel.Groups, nodes, EdgeSameDay, sameDayWeight, func(n NodeSpec) string { return n.CapturedAt.UTC().Format("2006-01-02") })

	slugToIndex := make(map[string]int, len(nodes))
	for i, node := range nodes {
		if slug := NodeSlug(node.ID, node.Subject, node.Topic); slug != "" {
			slugToIndex[slug] = i
		}
	}
	seen := map[[2]int]struct{}{}
	for src, node := range nodes {
		for _, raw := range ParseMemoryLinks(node.Body) {
			dst, ok := slugToIndex[Slugify(raw)]
			if !ok || nodes[dst].ID == node.ID {
				continue
			}
			key := [2]int{src, dst}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			rel.Refs = append(rel.Refs, RelationRef{Src: src, Dst: dst, Weight: refsWeight})
		}
	}
	return rel
}

// appendGroups appends one group per non-empty key shared by at least two
// nodes, in the order the keys first appear.
func appendGroups(groups []RelationGroup, nodes []NodeSpec, rel EdgeRel, weight float32, key func(NodeSpec) string) []RelationGroup {
	byKey := map[string]int{}
	start := len(groups)
	for i, n := range nodes {
		k := strings.TrimSpace(key(n))
		if k == "" {
			continue
		}
		if at, ok := byKey[k]; ok {
			groups[at].Members = append(groups[at].Members, i)
			continue
		}
		byKey[k] = len(groups)
		groups = append(groups, RelationGroup{Rel: rel, Weight: weight, Members: []int{i}})
	}
	kept := groups[:start]
	for _, g := range groups[start:] {
		if len(g.Members) >= 2 {
			kept = append(kept, g)
		}
	}
	return kept
}

// EdgeCount is the number of edges Edges returns for node sets with unique
// IDs, computed without expanding the groups.
func (r Relations) EdgeCount() int {
	count := len(r.Refs)
	for _, g := range r.Groups {
		count += len(g.Members) * (len(g.Members) - 1) / 2
	}
	return count
}

// Edges expands the relations into one edge per related pair and relation, in
// a deterministic order. Group edges are stored undirected as src < dst by node
// ID; refs keep their direction.
func (r Relations) Edges(nodes []NodeSpec) []EdgeSpec {
	edges := make([]EdgeSpec, 0, r.EdgeCount())
	seen := map[string]struct{}{}
	for _, g := range r.Groups {
		for i := 0; i < len(g.Members); i++ {
			for j := i + 1; j < len(g.Members); j++ {
				a, b := nodes[g.Members[i]], nodes[g.Members[j]]
				if a.ID == b.ID {
					continue
				}
				src, dst := a.ID, b.ID
				if dst < src {
					src, dst = dst, src
				}
				key := src + "\x00" + dst + "\x00" + string(g.Rel)
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				edges = append(edges, EdgeSpec{BotID: a.BotID, SrcNode: src, DstNode: dst, Rel: g.Rel, Weight: g.Weight})
			}
		}
	}
	for _, ref := range r.Refs {
		src, dst := nodes[ref.Src], nodes[ref.Dst]
		key := src.ID + "\x00" + dst.ID + "\x00" + string(EdgeRefs)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		edges = append(edges, EdgeSpec{BotID: src.BotID, SrcNode: src.ID, DstNode: dst.ID, Rel: EdgeRefs, Weight: ref.Weight})
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Rel != edges[j].Rel {
			return edges[i].Rel < edges[j].Rel
		}
		if edges[i].SrcNode != edges[j].SrcNode {
			return edges[i].SrcNode < edges[j].SrcNode
		}
		return edges[i].DstNode < edges[j].DstNode
	})
	return edges
}

// NodeSlug returns the human/LLM-friendly slug used in wiki cross-references.
func NodeSlug(id, subject, topic string) string {
	return memslug.NodeSlug(id, subject, topic)
}

// Slugify normalizes a user/LLM-facing memory label for [[slug]] links.
func Slugify(s string) string {
	return memslug.Slugify(s)
}

// ParseMemoryLinks extracts referenced slugs from a memory body. It supports
// [[slug]] and markdown links. HTTP(S) links are ignored.
func ParseMemoryLinks(body string) []string {
	return memslug.ParseMemoryLinks(body)
}

func metadataString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch s := v.(type) {
	case string:
		return strings.TrimSpace(s)
	default:
		return strings.TrimSpace(toString(v))
	}
}

func metadataFloat(m map[string]any, key string, def float32) float32 {
	if m == nil {
		return def
	}
	v, ok := m[key]
	if !ok || v == nil {
		return def
	}
	switch n := v.(type) {
	case float64:
		return clamp32(float32(n), def)
	case float32:
		return clamp32(n, def)
	case int:
		return clamp32(float32(n), def)
	case int64:
		return clamp32(float32(n), def)
	case string:
		return def
	default:
		return def
	}
}

func clamp32(v, def float32) float32 {
	if v < 0 || v > 1 {
		return def
	}
	return v
}

func cloneMetadata(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}
