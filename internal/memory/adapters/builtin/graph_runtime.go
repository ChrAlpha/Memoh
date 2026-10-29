//nolint:sloglint // graph runtime ops logs use inline key/value pairs for readability
package builtin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/felinics/memoh/internal/config"
	adapters "github.com/felinics/memoh/internal/memory/adapters"
	"github.com/felinics/memoh/internal/memory/migrate"
	memseg "github.com/felinics/memoh/internal/memory/segment"
	"github.com/felinics/memoh/internal/memory/wikistore"
)

// ModeGraph is the memory mode identifier for the graph runtime. It is the
// only supported mode: PG memory nodes/edges are the source of truth, with
// Markdown as a derived agent-facing view.
const ModeGraph = "graph"

// graphRuntime implements Runtime over the PostgreSQL wiki graph. The wiki
// store (wikistore.Store) is authoritative; the filesystem store (memoryStore)
// holds the derived Markdown view the agent reads.
type graphRuntime struct {
	store    wikistore.Store
	fs       memoryStore
	cache    *graphCache
	syncer   *graphSync
	semantic *pgvectorIndex
	retry    *semanticRetryQueue
	logger   *slog.Logger
}

// NewGraphRuntime constructs a graphRuntime. wikiStore is required; fs is the
// derived-view filesystem store (may be nil in tests, which disables sync).
func NewGraphRuntime(logger *slog.Logger, wikiStore wikistore.Store, fs memoryStore) *graphRuntime {
	if logger == nil {
		logger = slog.Default()
	}
	return &graphRuntime{
		store:  wikiStore,
		fs:     fs,
		cache:  newGraphCache(),
		syncer: newGraphSync(fs, logger),
		retry:  newSemanticRetryQueue(logger),
		logger: logger.With("runtime", "graph"),
	}
}

// SetSemanticIndex wires an optional Postgres pgvector seed index. It never
// owns the memory source of truth; failures only degrade to graph lexical recall.
func (r *graphRuntime) SetSemanticIndex(ctx context.Context, index *pgvectorIndex) {
	if index == nil {
		return
	}
	r.semantic = index
	// Failed upserts go to a small per-runtime retry queue; the background
	// loop drains it so the seed index converges without failing writes.
	r.retry.start(ctx, index)
}

func (*graphRuntime) Mode() string { return string(ModeGraph) }

func (r *graphRuntime) Close() error {
	if r == nil {
		return nil
	}
	if r.retry != nil {
		r.retry.stop()
	}
	return nil
}

func (r *graphRuntime) MemoryVersion(_ context.Context, botID string) string {
	if r == nil || r.cache == nil {
		return ""
	}
	return r.cache.version(strings.TrimSpace(botID))
}

// ---- helpers ----

// OverviewStaleSince reports whether MEMORY.md lags behind memory written
// during a conversation and, if so, since when.
func (r *graphRuntime) OverviewStaleSince(botID string) (time.Time, bool) {
	if r == nil || r.syncer == nil {
		return time.Time{}, false
	}
	return r.syncer.overviewStaleSince(botID)
}

// RefreshOverview rewrites a MEMORY.md that lags behind memory written during
// a conversation.
func (r *graphRuntime) RefreshOverview(ctx context.Context, botID string) error {
	if r == nil || r.syncer == nil {
		return nil
	}
	return r.syncer.refreshOverview(ctx, botID)
}

func (r *graphRuntime) syncAndInvalidate(ctx context.Context, botID string) {
	// Regenerate the derived Markdown view from the authoritative PG nodes.
	// Best-effort: PG is the source of truth, so a sync failure is logged, not
	// propagated to the caller.
	if r.syncer == nil || r.fs == nil {
		r.cache.invalidate(botID)
		return
	}
	nodes, err := r.store.ListNodes(ctx, botID)
	if err != nil {
		r.logger.WarnContext(ctx, "graph: list nodes for sync failed", "bot_id", botID, "err", err)
	} else if err := r.syncer.syncMarkdownFromNodes(ctx, botID, nodes); err != nil {
		r.logger.WarnContext(ctx, "graph: markdown sync failed", "bot_id", botID, "err", err)
	}
	r.cache.invalidate(botID)
}

// semanticIndexBestEffort indexes node bodies in the pgvector seed index.
// Nodes that fail to index are queued for retry; the rest supersede any stale
// pending retry.
func (r *graphRuntime) semanticIndexBestEffort(botID string, nodes ...migrate.NodeSpec) {
	if r.semantic == nil || len(nodes) == 0 {
		return
	}
	docs := make([]semanticDoc, 0, len(nodes))
	for _, n := range nodes {
		docs = append(docs, semanticDoc{nodeID: n.ID, body: n.Body, hash: n.Hash})
	}
	// Index bounds each embedding batch with its own timeout.
	failed, err := r.semantic.Index(context.Background(), botID, docs)
	if err != nil {
		r.logger.Debug("graph: pgvector index failed; queued for retry", "bot_id", botID, "failed", len(failed), "err", err)
	}
	failedIDs := make(map[string]bool, len(failed))
	for _, doc := range failed {
		failedIDs[doc.nodeID] = true
		r.retry.enqueue(semanticRetryEntry{botID: botID, nodeID: doc.nodeID, body: doc.body, hash: doc.hash})
	}
	indexed := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if !failedIDs[strings.TrimSpace(n.ID)] {
			indexed = append(indexed, n.ID)
		}
	}
	r.retry.discard(botID, indexed)
}

// ---- Runtime: CRUD ----

func (r *graphRuntime) Add(ctx context.Context, req adapters.AddRequest) (adapters.SearchResponse, error) {
	if r.store == nil {
		return adapters.SearchResponse{}, errors.New("graph runtime: wiki store not configured")
	}
	botID, err := runtimeBotID(req.BotID, req.Filters)
	if err != nil {
		return adapters.SearchResponse{}, err
	}
	text := runtimeText(req.Message, req.Messages)
	if text == "" {
		return adapters.SearchResponse{}, errors.New("graph runtime: message is required")
	}
	now := time.Now().UTC()
	spec := memoryItemToNodeSpec(adapters.MemoryItem{
		ID:               runtimeMemoryID(botID, now),
		Memory:           text,
		Hash:             runtimeHash(text),
		CreatedAt:        now.Format(time.RFC3339),
		UpdatedAt:        now.Format(time.RFC3339),
		Metadata:         req.Metadata,
		SourceMessageIDs: adapters.NormalizeSourceRefs(req.SourceMessageIDs),
	}, botID)

	saved, err := r.store.UpsertNode(ctx, spec)
	if err != nil {
		return adapters.SearchResponse{}, fmt.Errorf("graph runtime: upsert node: %w", err)
	}
	r.semanticIndexBestEffort(botID, saved) //nolint:contextcheck // semantic indexing bounds its own requests
	r.syncAndInvalidate(ctx, botID)
	return adapters.SearchResponse{Results: []adapters.MemoryItem{nodeSpecToMemoryItem(saved)}}, nil
}

func (r *graphRuntime) Search(ctx context.Context, req adapters.SearchRequest) (adapters.SearchResponse, error) {
	if r.store == nil {
		return adapters.SearchResponse{}, errors.New("graph runtime: wiki store not configured")
	}
	botID, err := runtimeBotID(req.BotID, req.Filters)
	if err != nil {
		return adapters.SearchResponse{}, err
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}

	// Primary path: graph seed-then-expand over the cached PG graph.
	resp, graphErr := r.searchGraph(ctx, botID, req.Query, limit)
	if graphErr == nil {
		return resp, nil
	}

	// Reliability fallback: degrade to file-lexical over the derived Markdown.
	r.logger.WarnContext(ctx, "graph search failed, falling back to file lexical", "bot_id", botID, "err", graphErr)
	fallback, err := r.searchFileFallback(ctx, botID, req.Query, limit)
	if fallback.FallbackReason == "" {
		fallback.FallbackReason = "graph_error"
	}
	return fallback, err
}

// searchGraph runs seed-then-expand: score nodes against the query, keep the
// best as seeds, and expand two relation hops from them (botGraph.expand).
func (r *graphRuntime) searchGraph(ctx context.Context, botID, query string, limit int) (adapters.SearchResponse, error) {
	graph, err := r.cache.getOrBuild(ctx, botID, r.store)
	if err != nil {
		return adapters.SearchResponse{}, err
	}

	overfetch := max(limit*3, 10)
	var semanticSeeds map[string]float64
	if r.semantic != nil {
		seeds, semanticErr := r.semantic.SearchSeeds(ctx, botID, query, overfetch)
		if semanticErr != nil {
			r.logger.DebugContext(ctx, "graph: pgvector seed search failed, using lexical seeds", "bot_id", botID, "err", semanticErr)
		} else {
			semanticSeeds = seeds
		}
	}
	seeds, err := graph.seeds(ctx, memseg.NewQuery(query), strings.TrimSpace(query) == "", semanticSeeds, overfetch)
	if err != nil {
		return adapters.SearchResponse{}, err
	}
	hits, err := graph.expand(ctx, seeds)
	if err != nil {
		return adapters.SearchResponse{}, err
	}
	hits = graph.top(hits, limit)
	results := make([]adapters.MemoryItem, 0, len(hits))
	for _, hit := range hits {
		item := nodeSpecToMemoryItem(graph.nodes[hit.node].spec)
		item.Score = hit.score
		results = append(results, item)
	}
	return adapters.SearchResponse{Results: results, RetrievalMode: "graph"}, nil
}

// searchFileFallback is the reliability fallback: read the derived Markdown via
// the bridge and score lexically, exactly like fileRuntime. Used when the PG
// graph is unavailable.
func (r *graphRuntime) searchFileFallback(ctx context.Context, botID, query string, limit int) (adapters.SearchResponse, error) {
	if r.fs == nil {
		return adapters.SearchResponse{}, nil
	}
	items, err := r.fs.ReadAllMemoryFiles(ctx, botID)
	if err != nil {
		return adapters.SearchResponse{}, fmt.Errorf("graph runtime: file fallback read: %w", err)
	}
	q := memseg.NewQuery(query)
	emptyQuery := strings.TrimSpace(query) == ""
	results := make([]adapters.MemoryItem, 0, len(items))
	for _, it := range items {
		score := q.ScoreLower(strings.ToLower(it.Memory))
		if !emptyQuery && score <= 0 {
			continue
		}
		m := memoryItemFromStore(it)
		m.BotID = botID
		m.Score = score
		results = append(results, m)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].UpdatedAt > results[j].UpdatedAt
		}
		return results[i].Score > results[j].Score
	})
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return adapters.SearchResponse{Results: results, RetrievalMode: "file_fallback"}, nil
}

func (r *graphRuntime) GetAll(ctx context.Context, req adapters.GetAllRequest) (adapters.SearchResponse, error) {
	if r.store == nil {
		return adapters.SearchResponse{}, errors.New("graph runtime: wiki store not configured")
	}
	botID, err := runtimeBotID(req.BotID, req.Filters)
	if err != nil {
		return adapters.SearchResponse{}, err
	}
	nodes, err := r.store.ListNodes(ctx, botID)
	if err != nil {
		// Fallback to derived files if the store is unavailable.
		r.logger.WarnContext(ctx, "graph GetAll failed, falling back to files", "bot_id", botID, "err", err)
		fallback, fallbackErr := r.searchFileFallback(ctx, botID, "", req.Limit)
		if fallback.FallbackReason == "" {
			fallback.FallbackReason = "graph_error"
		}
		return fallback, fallbackErr
	}
	out := make([]adapters.MemoryItem, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, nodeSpecToMemoryItem(n))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	if req.Limit > 0 && len(out) > req.Limit {
		out = out[:req.Limit]
	}
	return adapters.SearchResponse{Results: out, RetrievalMode: "graph"}, nil
}

func (r *graphRuntime) Update(ctx context.Context, req adapters.UpdateRequest) (adapters.MemoryItem, error) {
	if r.store == nil {
		return adapters.MemoryItem{}, errors.New("graph runtime: wiki store not configured")
	}
	memoryID := strings.TrimSpace(req.MemoryID)
	if memoryID == "" {
		return adapters.MemoryItem{}, errors.New("graph runtime: memory_id is required")
	}
	text := strings.TrimSpace(req.Memory)
	if text == "" {
		return adapters.MemoryItem{}, errors.New("graph runtime: memory is required")
	}
	botID := strings.TrimSpace(req.BotID)
	if err := adapters.ValidateMemoryScope(botID, memoryID); err != nil {
		return adapters.MemoryItem{}, err
	}
	existing, storedID, err := r.resolveNodeByMemoryID(ctx, botID, memoryID)
	if err != nil {
		return adapters.MemoryItem{}, fmt.Errorf("graph runtime: get node: %w", err)
	}
	existing.ID = memoryID
	existing.Body = text
	existing.Hash = runtimeHash(text)
	existing.SourceMessageIDs = adapters.MergeSourceRefs(existing.SourceMessageIDs, req.SourceMessageIDs)
	saved, err := r.store.UpsertNode(ctx, existing)
	if err != nil {
		return adapters.MemoryItem{}, fmt.Errorf("graph runtime: update node: %w", err)
	}
	if storedID != memoryID {
		if err := r.store.DeleteNode(ctx, botID, storedID); err != nil {
			return adapters.MemoryItem{}, fmt.Errorf("graph runtime: delete legacy node: %w", err)
		}
		if r.semantic != nil {
			r.discardSemanticNodes(ctx, botID, []string{storedID})
		}
	}
	r.semanticIndexBestEffort(botID, saved) //nolint:contextcheck // semantic indexing bounds its own requests
	r.syncAndInvalidate(ctx, botID)
	return nodeSpecToMemoryItem(saved), nil
}

func (r *graphRuntime) Delete(ctx context.Context, botID string, memoryID string) (adapters.DeleteResponse, error) {
	return r.DeleteBatch(ctx, botID, []string{memoryID})
}

func (r *graphRuntime) DeleteBatch(ctx context.Context, botID string, memoryIDs []string) (adapters.DeleteResponse, error) {
	botID = strings.TrimSpace(botID)
	// Validate the complete batch before the first mutation.
	if err := adapters.ValidateMemoryScope(botID, memoryIDs...); err != nil {
		return adapters.DeleteResponse{}, err
	}
	if r.store == nil {
		return adapters.DeleteResponse{}, errors.New("graph runtime: wiki store not configured")
	}
	// The projection and the semantic index follow the nodes this call
	// removed, including those removed before a later deletion failed.
	seen := map[string]bool{}
	var deleted []string
	var deleteErr error
	for _, rawID := range memoryIDs {
		memoryID := strings.TrimSpace(rawID)
		if memoryID == "" || seen[memoryID] {
			continue
		}
		seen[memoryID] = true
		_, storedID, err := r.resolveNodeByMemoryID(ctx, botID, memoryID)
		if err != nil {
			if !errors.Is(err, wikistore.ErrNodeNotFound) {
				deleteErr = fmt.Errorf("graph runtime: get node for delete: %w", err)
				break
			}
			storedID = memoryID
		}
		if err := r.store.DeleteNode(ctx, botID, storedID); err != nil {
			deleteErr = fmt.Errorf("graph runtime: delete node: %w", err)
			break
		}
		deleted = append(deleted, storedID)
		if storedID != memoryID {
			deleted = append(deleted, memoryID)
		}
	}
	if len(deleted) > 0 {
		r.syncAndInvalidate(ctx, botID)
		r.retry.discard(botID, deleted)
		r.discardSemanticNodes(ctx, botID, deleted)
	}
	if deleteErr != nil {
		return adapters.DeleteResponse{}, deleteErr
	}
	return adapters.DeleteResponse{Message: "Memories deleted successfully!"}, nil
}

func (r *graphRuntime) resolveNodeByMemoryID(ctx context.Context, botID, memoryID string) (migrate.NodeSpec, string, error) {
	memoryID = strings.TrimSpace(memoryID)
	node, err := r.store.GetNode(ctx, botID, memoryID)
	if err == nil {
		return node, memoryID, nil
	}
	if !errors.Is(err, wikistore.ErrNodeNotFound) {
		return migrate.NodeSpec{}, "", err
	}
	legacyID := runtimeLocalMemoryID(memoryID)
	if legacyID == "" || legacyID == memoryID {
		return migrate.NodeSpec{}, "", err
	}
	node, legacyErr := r.store.GetNode(ctx, botID, legacyID)
	if legacyErr != nil {
		if errors.Is(legacyErr, wikistore.ErrNodeNotFound) {
			return migrate.NodeSpec{}, "", err
		}
		return migrate.NodeSpec{}, "", legacyErr
	}
	return node, legacyID, nil
}

func (r *graphRuntime) discardSemanticNodes(ctx context.Context, botID string, nodeIDs []string) {
	if r.semantic == nil || len(nodeIDs) == 0 {
		return
	}
	semanticCtx, cancel := context.WithTimeout(ctx, semanticEmbedTimeout)
	defer cancel()
	if err := r.semantic.DeleteNodes(semanticCtx, botID, nodeIDs); err != nil {
		r.logger.DebugContext(ctx, "graph: pgvector delete failed", "bot_id", botID, "err", err)
	}
}

func (r *graphRuntime) DeleteAll(ctx context.Context, req adapters.DeleteAllRequest) (adapters.DeleteResponse, error) {
	if r.store == nil {
		return adapters.DeleteResponse{}, errors.New("graph runtime: wiki store not configured")
	}
	botID, err := runtimeBotID(req.BotID, req.Filters)
	if err != nil {
		return adapters.DeleteResponse{}, err
	}
	if err := r.store.DeleteAllNodes(ctx, botID); err != nil {
		return adapters.DeleteResponse{}, fmt.Errorf("graph runtime: delete all nodes: %w", err)
	}
	if r.fs != nil {
		if err := r.fs.RemoveAllMemories(ctx, botID); err != nil {
			r.logger.WarnContext(ctx, "graph: remove derived markdown failed", "bot_id", botID, "err", err)
		}
	}
	r.retry.discardBot(botID)
	if r.semantic != nil {
		auxCtx, cancel := context.WithTimeout(ctx, semanticEmbedTimeout)
		defer cancel()
		if err := r.semantic.DeleteBot(auxCtx, botID); err != nil {
			r.logger.DebugContext(ctx, "graph: pgvector delete bot failed", "bot_id", botID, "err", err)
		}
	}
	r.cache.invalidate(botID)
	return adapters.DeleteResponse{Message: "All memories deleted successfully!"}, nil
}

// ---- Runtime: usage / status / rebuild ----

func (r *graphRuntime) Usage(ctx context.Context, filters map[string]any) (adapters.UsageResponse, error) {
	if r.store == nil {
		return adapters.UsageResponse{}, errors.New("graph runtime: wiki store not configured")
	}
	botID, err := runtimeBotID("", filters)
	if err != nil {
		return adapters.UsageResponse{}, err
	}
	nodes, err := r.store.ListNodes(ctx, botID)
	if err != nil {
		return adapters.UsageResponse{}, fmt.Errorf("graph runtime: usage list: %w", err)
	}
	var usage adapters.UsageResponse
	usage.Count = len(nodes)
	for _, n := range nodes {
		usage.TotalTextBytes += int64(len(n.Body))
	}
	if usage.Count > 0 {
		usage.AvgTextBytes = usage.TotalTextBytes / int64(usage.Count)
	}
	usage.EstimatedStorageBytes = usage.TotalTextBytes
	return usage, nil
}

func (r *graphRuntime) Status(ctx context.Context, botID string) (adapters.MemoryStatusResponse, error) {
	if r.store == nil {
		return adapters.MemoryStatusResponse{}, errors.New("graph runtime: wiki store not configured")
	}
	nodeCount, _ := r.store.CountNodes(ctx, botID)
	edgeCount := 0
	if graph, err := r.cache.getOrBuild(ctx, botID, r.store); err == nil {
		edgeCount = graph.edgeCount
	}
	resp := adapters.MemoryStatusResponse{
		ProviderType:  BuiltinType,
		MemoryMode:    string(ModeGraph),
		CanManualSync: true,
		SourceDir:     path.Join(config.DefaultDataMount, "memory"),
		OverviewPath:  path.Join(config.DefaultDataMount, "MEMORY.md"),
		SourceCount:   nodeCount,
		EdgeCount:     edgeCount,
		IndexedCount:  0,
	}
	if r.semantic != nil {
		resp.VectorIndex = r.semantic.Name()
		if err := r.semantic.Health(ctx); err != nil {
			resp.Pgvector = &adapters.HealthStatus{Error: err.Error()}
		} else {
			resp.Pgvector = &adapters.HealthStatus{OK: true}
			if count, err := r.semantic.Count(ctx, botID); err == nil {
				resp.IndexedCount = count
			}
		}
		resp.RetryQueueDepth = r.retry.depth(botID)
		resp.Degraded = resp.RetryQueueDepth > 0 || resp.Pgvector == nil || !resp.Pgvector.OK
	}
	if r.fs != nil {
		if fc, err := r.fs.CountMemoryFiles(ctx, botID); err == nil {
			resp.MarkdownFileCount = fc
		}
	}
	return resp, nil
}

func (r *graphRuntime) Rebuild(ctx context.Context, botID string) (adapters.RebuildResult, error) {
	if r.store == nil {
		return adapters.RebuildResult{}, errors.New("graph runtime: wiki store not configured")
	}
	// Ingest agent-authored /data/memory/*.md BEFORE the destructive
	// derived-view rebuild so their content becomes DB nodes and survives the
	// regeneration. Without this, a rebuild would silently drop any file the
	// agent wrote directly that has not yet been ingested.
	if r.fs != nil {
		if _, _, err := r.ingestMarkdownFiles(ctx, botID); err != nil {
			r.logger.WarnContext(ctx, "graph: rebuild ingest failed", "bot_id", botID, "err", err)
		}
	}
	nodes, err := r.store.ListNodes(ctx, botID)
	if err != nil {
		return adapters.RebuildResult{}, fmt.Errorf("graph runtime: rebuild list: %w", err)
	}
	r.syncAndInvalidate(ctx, botID)
	count, _ := r.store.CountNodes(ctx, botID)
	result := adapters.RebuildResult{FsCount: len(nodes), StorageCount: count}
	if r.semantic != nil {
		// Rebuild re-embeds every node, so a changed embedding behind the
		// same model is picked up; the ingest above embedded nothing.
		r.retry.discardBot(botID)
		if err := r.semantic.DeleteBot(ctx, botID); err != nil {
			r.logger.DebugContext(ctx, "graph: pgvector rebuild clear failed", "bot_id", botID, "err", err)
		}
		r.semanticIndexBestEffort(botID, nodes...) //nolint:contextcheck // semantic indexing bounds its own requests
		if indexed, err := r.semantic.Count(ctx, botID); err == nil {
			result.StorageCount = indexed
		}
	}
	return result, nil
}

// ---- node/memory item conversion ----

// memoryItemToNodeSpec derives a NodeSpec from a MemoryItem, classifying the
// layer from metadata (defaulting to note) and pulling profile_ref/topic out.
func memoryItemToNodeSpec(item adapters.MemoryItem, botID string) migrate.NodeSpec {
	body := strings.TrimSpace(item.Memory)
	layer := migrate.LayerNote
	if raw, ok := item.Metadata["layer"].(string); ok && raw != "" {
		switch migrate.MemoryLayer(strings.ToLower(strings.TrimSpace(raw))) {
		case migrate.LayerPreference, migrate.LayerIdentity, migrate.LayerContext,
			migrate.LayerExperience, migrate.LayerActivity, migrate.LayerPersona, migrate.LayerNote:
			layer = migrate.MemoryLayer(raw)
		}
	}
	profileRef := metadataStringVal(item.Metadata, "profile_ref")
	if profileRef == "" {
		profileRef = metadataStringVal(item.Metadata, "profile_user_id")
	}
	return migrate.NodeSpec{
		ID:               strings.TrimSpace(item.ID),
		BotID:            botID,
		Body:             body,
		Hash:             strings.TrimSpace(item.Hash),
		Layer:            layer,
		Subject:          metadataStringVal(item.Metadata, "subject"),
		Confidence:       metadataFloatVal(item.Metadata, "confidence", 0.5),
		Metadata:         item.Metadata,
		SourceMessageIDs: adapters.NormalizeSourceRefs(item.SourceMessageIDs),
		ProfileRef:       profileRef,
		Topic:            metadataStringVal(item.Metadata, "topic"),
		CapturedAt:       parseGraphTime(item.CreatedAt),
	}
}

func metadataStringVal(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func metadataFloatVal(m map[string]any, key string, def float32) float32 {
	if m == nil {
		return def
	}
	v, ok := m[key]
	if !ok || v == nil {
		return def
	}
	switch n := v.(type) {
	case float64:
		f := float32(n)
		if f < 0 || f > 1 {
			return def
		}
		return f
	case float32:
		if n < 0 || n > 1 {
			return def
		}
		return n
	}
	return def
}

func parseGraphTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Now().UTC()
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Now().UTC()
}
