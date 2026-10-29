package storefs

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/workspace/bridge"
	pb "github.com/felinics/memoh/internal/workspace/bridgepb"
)

// memoryWorkspace is an in-memory workspace that stamps each write with the
// next second and counts the calls the Service makes.
type memoryWorkspace struct {
	files   map[string]string
	modTime map[string]time.Time
	clock   time.Time

	lists, reads, writes, deletes int
}

func newMemoryWorkspace() *memoryWorkspace {
	return &memoryWorkspace{
		files:   map[string]string{},
		modTime: map[string]time.Time{},
		clock:   time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}
}

func (w *memoryWorkspace) service() *Service {
	s := New(nil, nil)
	s.files = func(context.Context, string) (workspaceFiles, error) { return w, nil }
	return s
}

// put writes as someone other than the Service would, e.g. the agent.
func (w *memoryWorkspace) put(path, content string) {
	w.clock = w.clock.Add(time.Second)
	w.files[path] = content
	w.modTime[path] = w.clock
}

func (w *memoryWorkspace) resetCounts() { w.lists, w.reads, w.writes, w.deletes = 0, 0, 0, 0 }

func (w *memoryWorkspace) ListDirAll(_ context.Context, dir string, _ bool) ([]*pb.FileEntry, error) {
	w.lists++
	paths := make([]string, 0, len(w.files))
	for path := range w.files {
		if strings.HasPrefix(path, dir+"/") {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return nil, bridge.ErrNotFound
	}
	sort.Strings(paths)
	entries := make([]*pb.FileEntry, 0, len(paths))
	for _, path := range paths {
		entries = append(entries, &pb.FileEntry{
			Path:    strings.TrimPrefix(path, dir+"/"),
			Size:    int64(len(w.files[path])),
			ModTime: w.modTime[path].Format(time.RFC3339),
		})
	}
	return entries, nil
}

func (w *memoryWorkspace) ReadRaw(_ context.Context, path string) (io.ReadCloser, error) {
	w.reads++
	content, ok := w.files[path]
	if !ok {
		return nil, bridge.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

func (w *memoryWorkspace) WriteFile(_ context.Context, path string, content []byte) error {
	w.writes++
	w.put(path, string(content))
	return nil
}

func (w *memoryWorkspace) DeleteFile(_ context.Context, path string, recursive bool) error {
	w.deletes++
	for p := range w.files {
		if p == path || (recursive && strings.HasPrefix(p, path+"/")) {
			delete(w.files, p)
			delete(w.modTime, p)
		}
	}
	return nil
}

func projectionItems(botID string, n int) []MemoryItem {
	items := make([]MemoryItem, 0, n)
	for i := range n {
		items = append(items, MemoryItem{
			ID:        fmt.Sprintf("%s:mem_%04d", botID, i),
			Memory:    fmt.Sprintf("Memory %d mentions [[topic-%d]].", i, (i+1)%n),
			CreatedAt: time.Date(2026, 9, 1, 0, i, 0, 0, time.UTC).Format(time.RFC3339),
			Metadata:  map[string]any{"subject": fmt.Sprintf("topic-%d", i), "layer": "note"},
		})
	}
	return items
}

// fullProjection is what a from-scratch rebuild leaves on disk.
func fullProjection(t *testing.T, items []MemoryItem) map[string]string {
	t.Helper()
	w := newMemoryWorkspace()
	if err := w.service().RebuildFiles(context.Background(), "bot", items, nil); err != nil {
		t.Fatalf("RebuildFiles: %v", err)
	}
	return w.files
}

func assertFiles(t *testing.T, got, want map[string]string) {
	t.Helper()
	for path, content := range want {
		if got[path] != content {
			t.Fatalf("%s = %q, want %q", path, got[path], content)
		}
	}
	for path := range got {
		if _, ok := want[path]; !ok {
			t.Fatalf("unexpected file %s", path)
		}
	}
}

func TestRebuildFilesWritesOnlyChangedProjection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newMemoryWorkspace()
	s := w.service()
	items := projectionItems("bot", 20)
	if err := s.RebuildFiles(ctx, "bot", items, nil); err != nil {
		t.Fatalf("first RebuildFiles: %v", err)
	}
	// A later write reads each file it has not seen yet, once.
	w.resetCounts()
	if err := s.RebuildFiles(ctx, "bot", items, nil); err != nil {
		t.Fatalf("second RebuildFiles: %v", err)
	}

	items[3].Memory = "Memory 3 changed."
	w.resetCounts()
	if err := s.RebuildFiles(WithConversationWrite(ctx), "bot", items, nil); err != nil {
		t.Fatalf("RebuildFiles after an update: %v", err)
	}
	if w.lists != 1 || w.reads != 0 || w.writes != 1 || w.deletes != 0 {
		t.Fatalf("calls = %d lists, %d reads, %d writes, %d deletes; want one list and one write",
			w.lists, w.reads, w.writes, w.deletes)
	}
	assertFiles(t, w.files, withoutOverview(fullProjection(t, items), w.files))
}

// withoutOverview takes MEMORY.md from got: a conversation write defers it.
func withoutOverview(want, got map[string]string) map[string]string {
	out := make(map[string]string, len(want))
	for path, content := range want {
		out[path] = content
	}
	out[memoryOverviewPath()] = got[memoryOverviewPath()]
	return out
}

func TestRebuildFilesRemovesStaleProjectionAndKeepsAgentFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newMemoryWorkspace()
	s := w.service()
	items := projectionItems("bot", 5)
	if err := s.RebuildFiles(ctx, "bot", items, nil); err != nil {
		t.Fatalf("RebuildFiles: %v", err)
	}
	agentNote := memoryDirPath() + "/notes/scratch.md"
	w.put(agentNote, "free-form notes the agent keeps")

	items = items[1:]
	if err := s.RebuildFiles(ctx, "bot", items, nil); err != nil {
		t.Fatalf("RebuildFiles after a delete: %v", err)
	}
	want := fullProjection(t, items)
	want[agentNote] = "free-form notes the agent keeps"
	delete(want, memoryOverviewPath())
	overview := w.files[memoryOverviewPath()]
	delete(w.files, memoryOverviewPath())
	assertFiles(t, w.files, want)
	if strings.Contains(overview, "mem_0000") {
		t.Fatalf("overview still lists the deleted memory:\n%s", overview)
	}
}

func TestRebuildFilesRestoresProjectionChangedOutsideTheService(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newMemoryWorkspace()
	s := w.service()
	items := projectionItems("bot", 4)
	if err := s.RebuildFiles(ctx, "bot", items, nil); err != nil {
		t.Fatalf("RebuildFiles: %v", err)
	}
	want := map[string]string{}
	for path, content := range w.files {
		want[path] = content
	}
	var edited string
	for path := range w.files {
		if path != memoryOverviewPath() {
			edited = path
			break
		}
	}
	w.put(edited, "hand edited")

	if err := s.RebuildFiles(WithConversationWrite(ctx), "bot", items, nil); err != nil {
		t.Fatalf("RebuildFiles: %v", err)
	}
	assertFiles(t, w.files, want)
}

func TestReadAllMemoryFilesRereadsOnlyChangedFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newMemoryWorkspace()
	s := w.service()
	if err := s.RebuildFiles(ctx, "bot", projectionItems("bot", 10), nil); err != nil {
		t.Fatalf("RebuildFiles: %v", err)
	}
	if _, err := s.ReadAllMemoryFiles(ctx, "bot"); err != nil {
		t.Fatalf("ReadAllMemoryFiles: %v", err)
	}
	w.put(memoryDirPath()+"/inbox.md", "---\ntype: memory\nlayer: note\n---\n\nAn agent-written memory without an id.\n")

	w.resetCounts()
	items, err := s.ReadAllMemoryFilesForIngest(ctx, "bot")
	if err != nil {
		t.Fatalf("ReadAllMemoryFilesForIngest: %v", err)
	}
	if w.reads != 1 {
		t.Fatalf("reads = %d, want only the new file read", w.reads)
	}
	if len(items) != 11 {
		t.Fatalf("items = %d, want 10 projected + 1 agent-written", len(items))
	}
	want := synthIngestID("inbox.md", "An agent-written memory without an id.")
	found := false
	for _, item := range items {
		found = found || item.ID == want
	}
	if !found {
		t.Fatalf("agent-written memory did not get the synthesized id %s", want)
	}
}

// BenchmarkRebuildFilesAfterOneChange is the conversation write path: one
// memory changed, the projection resynced, MEMORY.md deferred.
func BenchmarkRebuildFilesAfterOneChange(b *testing.B) {
	for _, n := range []int{200, 1000} {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			ctx := WithConversationWrite(context.Background())
			w := newMemoryWorkspace()
			s := w.service()
			items := projectionItems("bot", n)
			// The second sync reads what the first wrote; measure from there.
			for range 2 {
				if err := s.RebuildFiles(ctx, "bot", items, nil); err != nil {
					b.Fatalf("RebuildFiles: %v", err)
				}
			}
			w.resetCounts()
			b.ReportAllocs()
			b.ResetTimer()
			i := 0
			for b.Loop() {
				items[i%n].Memory = fmt.Sprintf("Memory %d revised %d.", i%n, i)
				i++
				if err := s.RebuildFiles(ctx, "bot", items, nil); err != nil {
					b.Fatalf("RebuildFiles: %v", err)
				}
			}
			ops := float64(i)
			b.ReportMetric(float64(w.lists+w.reads+w.writes+w.deletes)/ops, "rpcs/op")
			b.ReportMetric(float64(w.writes)/ops, "writes/op")
		})
	}
}

func TestRebuildFilesRerendersLinksWhenTheirTargetChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newMemoryWorkspace()
	s := w.service()
	items := []MemoryItem{
		{ID: "bot:a", Memory: "Ask about [[berlin-home]] and [[helix]].", CreatedAt: "2026-09-01T00:00:00Z", Metadata: map[string]any{"subject": "alice"}},
		{ID: "bot:b", Memory: "Home details.", CreatedAt: "2026-09-01T00:01:00Z", Metadata: map[string]any{"subject": "Berlin Home"}},
	}
	steps := []func(){
		// The link to helix resolves once a concept takes that slug.
		func() {
			items = append(items, MemoryItem{ID: "bot:c", Memory: "Editor.", CreatedAt: "2026-09-01T00:02:00Z", Metadata: map[string]any{"subject": "helix", "layer": "preference"}})
		},
		// The berlin-home concept moves to another layer, so its path changes.
		func() { items[1].Metadata = map[string]any{"subject": "Berlin Home", "layer": "context"} },
		// The helix concept goes away again.
		func() { items = items[:2] },
	}
	for i, step := range steps {
		if err := s.RebuildFiles(ctx, "bot", items, nil); err != nil {
			t.Fatalf("step %d: RebuildFiles: %v", i, err)
		}
		step()
		if err := s.RebuildFiles(ctx, "bot", items, nil); err != nil {
			t.Fatalf("step %d: RebuildFiles: %v", i, err)
		}
		assertFiles(t, w.files, fullProjection(t, items))
	}
}
