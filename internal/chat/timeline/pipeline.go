package timeline

import (
	"container/list"
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

const (
	defaultPipelineMaxSessions     = 256
	defaultPipelineMaxResidentByte = 256 << 20
	defaultPipelineTTL             = 6 * time.Hour
)

// PipelineOptions bounds the in-process projection cache. Zero values select
// safe defaults; negative limits disable that individual limit for tests.
type PipelineOptions struct {
	MaxSessions     int
	MaxResidentByte int64
	TTL             time.Duration
	Logger          *slog.Logger
	Now             func() time.Time
}

// PipelineStats is a point-in-time snapshot of replay/cache counters.
type PipelineStats struct {
	ResidentSessions int
	ResidentBytes    int64
	ReplayEvents     int64
	ReplayBytes      int64
	Evictions        int64
	EvictedBytes     int64
}

type pipelineSession struct {
	botID         string
	ic            IntermediateContext
	rc            RenderedContext
	residentBytes int64
	lastAccess    time.Time
	lru           *list.Element
}

// Pipeline manages per-thread IC/RC state. It is goroutine-safe. Its render
// params are fixed at construction, so a replay renders without the lock.
type Pipeline struct {
	renderParams RenderParams

	mu       sync.Mutex
	sessions map[string]*pipelineSession
	lru      *list.List
	options  PipelineOptions
	stats    PipelineStats

	// drops counts explicit drops. While replays are in flight, the count at
	// which each session and bot was last dropped is kept, so a replay that
	// began before a drop does not bring back what the drop removed.
	drops          uint64
	replaying      int
	droppedSession map[string]uint64
	droppedBot     map[string]uint64
}

// NewPipeline creates a bounded Pipeline with production defaults.
func NewPipeline(params RenderParams) *Pipeline {
	return NewPipelineWithOptions(params, PipelineOptions{})
}

// NewPipelineWithOptions creates a Pipeline with explicit cache controls.
func NewPipelineWithOptions(params RenderParams, options PipelineOptions) *Pipeline {
	if options.MaxSessions == 0 {
		options.MaxSessions = defaultPipelineMaxSessions
	}
	if options.MaxResidentByte == 0 {
		options.MaxResidentByte = defaultPipelineMaxResidentByte
	}
	if options.TTL == 0 {
		options.TTL = defaultPipelineTTL
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Pipeline{
		renderParams:   params,
		sessions:       make(map[string]*pipelineSession),
		lru:            list.New(),
		options:        options,
		droppedSession: make(map[string]uint64),
		droppedBot:     make(map[string]uint64),
	}
}

// PushEvent applies one event and renders only dirty/new nodes. Existing
// rendered slices are detached before replacement so snapshots already queued
// to a discuss worker remain immutable.
// A session it makes resident belongs to botID.
func (p *Pipeline) PushEvent(botID, sessionID string, event CanonicalEvent) RenderedContext {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.options.Now()
	p.evictExpired(now)
	state := p.sessions[sessionID]
	if state == nil {
		state = &pipelineSession{botID: botID, ic: NewEmptyIC(sessionID), lastAccess: now}
		state.lru = p.lru.PushFront(sessionID)
		p.sessions[sessionID] = state
	}
	return p.pushLocked(state, event, now)
}

// PushResidentEvent applies one event to a resident session as PushEvent does.
// A session that is not resident stays that way: the event renders on its own,
// so the session's next access still finds it cold and loads its history.
// This is for a session whose history failed to load, which a projection must
// not make resident with that history missing.
func (p *Pipeline) PushResidentEvent(sessionID string, event CanonicalEvent) RenderedContext {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.options.Now()
	p.evictExpired(now)
	if state := p.sessions[sessionID]; state != nil {
		return p.pushLocked(state, event, now)
	}
	ic := NewEmptyIC(sessionID)
	reduceInPlace(&ic, event)
	return Render(ic, p.renderParams)
}

func (p *Pipeline) pushLocked(state *pipelineSession, event CanonicalEvent, now time.Time) RenderedContext {
	p.touch(state, now)
	existingDirty := dirtyUnique(dirtyIndexes(state.ic, event))
	var oldDirtyNodeBytes int64
	for _, idx := range existingDirty {
		oldDirtyNodeBytes += estimateJSONBytes(state.ic.Nodes[idx])
	}
	dirty := dirtyUnique(reduceInPlace(&state.ic, event))
	state.rc, state.residentBytes = renderDirty(state.ic, state.rc, state.residentBytes, dirty, p.renderParams)
	var newDirtyNodeBytes int64
	for _, idx := range existingDirty {
		newDirtyNodeBytes += estimateJSONBytes(state.ic.Nodes[idx])
	}
	state.residentBytes += newDirtyNodeBytes - oldDirtyNodeBytes
	p.recountResidentBytes()
	snapshot := state.rc
	p.enforceBounds("capacity")
	return snapshot
}

// Replay rebuilds one cold session of a bot from its persisted events. Begin
// it before reading the events: a drop of the session or its bot after that
// means what was read is stale, and Install then leaves the session cold.
// Close it when done; Close after Install does nothing.
type Replay struct {
	p         *Pipeline
	botID     string
	sessionID string
	drops     uint64
	ended     bool
}

// BeginReplay starts a replay of sessionID, a session of botID.
func (p *Pipeline) BeginReplay(botID, sessionID string) *Replay {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.replaying++
	return &Replay{p: p, botID: botID, sessionID: sessionID, drops: p.drops}
}

// Install rebuilds the session from events and makes it resident in place of
// what was there, reporting whether it did. It does not when the session or
// its bot was dropped since the replay began. The rebuild runs without the
// lock, so a long history does not hold up the other sessions; only the swap
// takes it. It records replay volume for operational sizing.
func (r *Replay) Install(events []CanonicalEvent) (RenderedContext, bool) {
	p := r.p
	ic := NewEmptyIC(r.sessionID)
	var replayBytes int64
	for _, event := range events {
		reduceInPlace(&ic, event)
		if encoded, err := json.Marshal(event); err == nil {
			replayBytes += int64(len(encoded))
		}
	}
	rc := Render(ic, p.renderParams)
	residentBytes := estimateICBytes(ic) + estimateRCBytes(rc)

	if !p.install(r, &pipelineSession{botID: r.botID, ic: ic, rc: rc, residentBytes: residentBytes}, len(events), replayBytes) {
		return nil, false
	}
	p.options.Logger.Info("timeline replay completed",
		slog.String("session_id", r.sessionID),
		slog.Int("event_count", len(events)),
		slog.Int64("event_bytes", replayBytes),
		slog.Int64("resident_bytes", residentBytes))
	return rc, true
}

// Close ends a replay that did not install, such as one whose events failed
// to load.
func (r *Replay) Close() {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.p.endReplay(r)
}

func (p *Pipeline) install(r *Replay, state *pipelineSession, replayEvents int, replayBytes int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	defer p.endReplay(r)
	if r.ended || p.droppedSession[r.sessionID] > r.drops || p.droppedBot[r.botID] > r.drops {
		return false
	}
	now := p.options.Now()
	p.evictExpired(now)
	if old := p.sessions[r.sessionID]; old != nil {
		p.lru.Remove(old.lru)
	}
	state.lastAccess = now
	state.lru = p.lru.PushFront(r.sessionID)
	p.sessions[r.sessionID] = state
	p.stats.ReplayEvents += int64(replayEvents)
	p.stats.ReplayBytes += replayBytes
	p.recountResidentBytes()
	p.enforceBounds("capacity")
	return true
}

func (p *Pipeline) endReplay(r *Replay) {
	if r.ended {
		return
	}
	r.ended = true
	p.replaying--
	if p.replaying == 0 {
		clear(p.droppedSession)
		clear(p.droppedBot)
	}
}

// GetRC returns an immutable rendered snapshot, or nil if not loaded.
func (p *Pipeline) GetRC(sessionID string) RenderedContext {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.options.Now()
	p.evictExpired(now)
	state := p.sessions[sessionID]
	if state == nil {
		return nil
	}
	p.touch(state, now)
	return state.rc
}

// GetIC returns an ownership-safe copy of the current projection.
func (p *Pipeline) GetIC(sessionID string) (IntermediateContext, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.options.Now()
	p.evictExpired(now)
	state := p.sessions[sessionID]
	if state == nil {
		return IntermediateContext{}, false
	}
	p.touch(state, now)
	return cloneIC(state.ic), true
}

// HasSession reports whether a session is resident without cloning its IC.
func (p *Pipeline) HasSession(sessionID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.options.Now()
	p.evictExpired(now)
	state := p.sessions[sessionID]
	if state == nil {
		return false
	}
	p.touch(state, now)
	return true
}

// SessionIDs returns all loaded session IDs.
func (p *Pipeline) SessionIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.evictExpired(p.options.Now())
	ids := make([]string, 0, len(p.sessions))
	for id := range p.sessions {
		ids = append(ids, id)
	}
	return ids
}

// DropSession removes a session's state from the pipeline.
// A replay of it already in flight does not install.
func (p *Pipeline) DropSession(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.drops++
	if p.replaying > 0 {
		p.droppedSession[sessionID] = p.drops
	}
	p.drop(sessionID, "explicit")
}

// DropBot removes every resident session of a bot. A replay of one of them
// already in flight does not install.
func (p *Pipeline) DropBot(botID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.drops++
	if p.replaying > 0 {
		p.droppedBot[botID] = p.drops
	}
	for sessionID, state := range p.sessions {
		if state.botID == botID {
			p.drop(sessionID, "explicit_bot")
		}
	}
}

// Stats returns replay/cache counters without exposing mutable state.
func (p *Pipeline) Stats() PipelineStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.evictExpired(p.options.Now())
	p.recountResidentBytes()
	return p.stats
}

func (p *Pipeline) touch(state *pipelineSession, now time.Time) {
	state.lastAccess = now
	p.lru.MoveToFront(state.lru)
}

func (p *Pipeline) evictExpired(now time.Time) {
	if p.options.TTL < 0 {
		return
	}
	for elem := p.lru.Back(); elem != nil; {
		previous := elem.Prev()
		id := elem.Value.(string)
		state := p.sessions[id]
		if state == nil || now.Sub(state.lastAccess) < p.options.TTL {
			break
		}
		p.drop(id, "ttl")
		elem = previous
	}
}

func (p *Pipeline) enforceBounds(reason string) {
	for p.lru.Len() > 0 && ((p.options.MaxSessions >= 0 && len(p.sessions) > p.options.MaxSessions) ||
		(p.options.MaxResidentByte >= 0 && p.stats.ResidentBytes > p.options.MaxResidentByte)) {
		p.drop(p.lru.Back().Value.(string), reason)
	}
}

func (p *Pipeline) drop(sessionID, reason string) {
	state := p.sessions[sessionID]
	if state == nil {
		return
	}
	delete(p.sessions, sessionID)
	p.lru.Remove(state.lru)
	p.stats.Evictions++
	p.stats.EvictedBytes += state.residentBytes
	p.stats.ResidentBytes -= state.residentBytes
	if p.stats.ResidentBytes < 0 {
		p.stats.ResidentBytes = 0
	}
	p.stats.ResidentSessions = len(p.sessions)
	p.options.Logger.Info("timeline cache eviction",
		slog.String("session_id", sessionID),
		slog.String("reason", reason),
		slog.Int64("resident_bytes", state.residentBytes))
}

func (p *Pipeline) recountResidentBytes() {
	var total int64
	for _, state := range p.sessions {
		total += state.residentBytes
	}
	p.stats.ResidentSessions = len(p.sessions)
	p.stats.ResidentBytes = total
}

func dirtyUnique(indexes []int) []int {
	seen := make(map[int]struct{}, len(indexes))
	out := indexes[:0]
	for _, idx := range indexes {
		if _, ok := seen[idx]; ok {
			continue
		}
		seen[idx] = struct{}{}
		out = append(out, idx)
	}
	return out
}

func dirtyIndexes(ic IntermediateContext, event CanonicalEvent) []int {
	var dirty []int
	switch typed := event.(type) {
	case MessageEvent:
		if idx := findMessageIndex(ic.Nodes, typed.MessageID); idx >= 0 {
			dirty = append(dirty, idx)
		}
	case EditEvent:
		if idx := findMessageIndex(ic.Nodes, typed.MessageID); idx >= 0 {
			dirty = append(dirty, idx)
		}
	case DeleteEvent:
		for _, messageID := range typed.MessageIDs {
			if idx := findMessageIndex(ic.Nodes, messageID); idx >= 0 {
				dirty = append(dirty, idx)
			}
		}
	}
	return dirty
}

func renderDirty(ic IntermediateContext, rc RenderedContext, residentBytes int64, dirty []int, params RenderParams) (RenderedContext, int64) {
	detached := false
	for _, idx := range dirty {
		if idx < 0 || idx >= len(ic.Nodes) {
			continue
		}
		segment := renderNode(ic.Nodes[idx], params)
		if idx < len(rc) {
			if !detached {
				rc = append(RenderedContext(nil), rc...)
				detached = true
			}
			residentBytes -= estimateJSONBytes(rc[idx])
			rc[idx] = segment
			residentBytes += estimateJSONBytes(segment)
			continue
		}
		if idx == len(rc) {
			rc = append(rc, segment)
			residentBytes += estimateJSONBytes(ic.Nodes[idx]) + estimateJSONBytes(segment)
		}
	}
	return rc, residentBytes
}

func renderNode(node ICNode, params RenderParams) RenderedSegment {
	if node.Message != nil {
		return renderMessage(node.Message, params)
	}
	if node.SystemEvent != nil {
		return renderSystemEvent(node.SystemEvent, params)
	}
	return RenderedSegment{}
}

func estimateICBytes(ic IntermediateContext) int64 {
	var total int64
	for _, node := range ic.Nodes {
		total += estimateJSONBytes(node)
	}
	return total
}

func estimateRCBytes(rc RenderedContext) int64 {
	var total int64
	for _, segment := range rc {
		total += estimateJSONBytes(segment)
	}
	return total
}

func estimateJSONBytes(value any) int64 {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return int64(len(encoded))
}
