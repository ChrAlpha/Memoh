package builtin

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"testing"

	"github.com/memohai/memoh/internal/mcp"
	adapters "github.com/memohai/memoh/internal/memory/adapters"
)

func TestGraphRuntimeAddPersistsSourceMessageIDs(t *testing.T) {
	t.Parallel()
	store := newFakeWikiStore()
	rt := NewGraphRuntime(nil, store, newFakeStore())

	resp, err := rt.Add(context.Background(), adapters.AddRequest{
		Message:          "User prefers oolong tea",
		BotID:            "bot-1",
		SourceMessageIDs: []string{"sess-1/msg-1", "sess-1/msg-1", "sess-1/msg-2"},
	})
	if err != nil {
		t.Fatalf("Add error: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(resp.Results))
	}
	want := []string{"sess-1/msg-1", "sess-1/msg-2"}
	node, ok := store.nodes[resp.Results[0].ID]
	if !ok {
		t.Fatalf("node %q not found in store", resp.Results[0].ID)
	}
	if !slices.Equal(node.SourceMessageIDs, want) {
		t.Fatalf("stored SourceMessageIDs = %v, want %v", node.SourceMessageIDs, want)
	}
	if !slices.Equal(resp.Results[0].SourceMessageIDs, want) {
		t.Fatalf("returned SourceMessageIDs = %v, want %v", resp.Results[0].SourceMessageIDs, want)
	}
}

func TestGraphRuntimeUpdateUnionsSourceMessageIDs(t *testing.T) {
	t.Parallel()
	store := newFakeWikiStore()
	rt := NewGraphRuntime(nil, store, newFakeStore())

	added, err := rt.Add(context.Background(), adapters.AddRequest{
		Message:          "User prefers oolong tea",
		BotID:            "bot-1",
		SourceMessageIDs: []string{"sess-1/msg-1", "sess-1/msg-2"},
	})
	if err != nil {
		t.Fatalf("Add error: %v", err)
	}
	memoryID := added.Results[0].ID

	updated, err := rt.Update(context.Background(), adapters.UpdateRequest{
		MemoryID:         memoryID,
		Memory:           "User prefers strong oolong tea",
		SourceMessageIDs: []string{"sess-1/msg-2", "sess-2/msg-3"},
	})
	if err != nil {
		t.Fatalf("Update error: %v", err)
	}
	want := []string{"sess-1/msg-1", "sess-1/msg-2", "sess-2/msg-3"}
	if !slices.Equal(updated.SourceMessageIDs, want) {
		t.Fatalf("updated SourceMessageIDs = %v, want %v", updated.SourceMessageIDs, want)
	}
	node := store.nodes[memoryID]
	if !slices.Equal(node.SourceMessageIDs, want) {
		t.Fatalf("stored SourceMessageIDs = %v, want %v", node.SourceMessageIDs, want)
	}
}

type capturingRuntime struct {
	Runtime
	addReqs    []adapters.AddRequest
	updateReqs []adapters.UpdateRequest
}

func (c *capturingRuntime) Add(_ context.Context, req adapters.AddRequest) (adapters.SearchResponse, error) {
	c.addReqs = append(c.addReqs, req)
	return adapters.SearchResponse{}, nil
}

func (c *capturingRuntime) Update(_ context.Context, req adapters.UpdateRequest) (adapters.MemoryItem, error) {
	c.updateReqs = append(c.updateReqs, req)
	return adapters.MemoryItem{}, nil
}

func (*capturingRuntime) Search(_ context.Context, _ adapters.SearchRequest) (adapters.SearchResponse, error) {
	return adapters.SearchResponse{}, nil
}

func (*capturingRuntime) GetAll(_ context.Context, _ adapters.GetAllRequest) (adapters.SearchResponse, error) {
	return adapters.SearchResponse{}, nil
}

func TestFormationCarriesSourceMessageIDs(t *testing.T) {
	t.Parallel()
	runtime := &capturingRuntime{}
	llm := &fakeLLM{
		extractFacts: []string{"User likes oolong tea"},
		decideActions: []adapters.DecisionAction{
			{Event: "ADD", Text: "User likes oolong tea"},
			{Event: "UPDATE", ID: "bot-1:mem_existing", Text: "User likes strong oolong tea"},
		},
	}
	refs := []string{"sess-1/msg-1", "sess-1/msg-2"}

	runFormation(context.Background(), slog.Default(), llm, runtime, adapters.AfterChatRequest{
		BotID:            "bot-1",
		Messages:         []adapters.Message{{Role: "user", Content: "I like oolong tea"}},
		SourceMessageIDs: refs,
	})

	if len(runtime.addReqs) != 1 {
		t.Fatalf("expected 1 add request, got %d", len(runtime.addReqs))
	}
	if !slices.Equal(runtime.addReqs[0].SourceMessageIDs, refs) {
		t.Fatalf("ADD SourceMessageIDs = %v, want %v", runtime.addReqs[0].SourceMessageIDs, refs)
	}
	if len(runtime.updateReqs) != 1 {
		t.Fatalf("expected 1 update request, got %d", len(runtime.updateReqs))
	}
	if !slices.Equal(runtime.updateReqs[0].SourceMessageIDs, refs) {
		t.Fatalf("UPDATE SourceMessageIDs = %v, want %v", runtime.updateReqs[0].SourceMessageIDs, refs)
	}
}

func callSearchMemory(t *testing.T, p *BuiltinProvider, query string) []map[string]any {
	t.Helper()
	result, err := p.CallTool(context.Background(), mcp.ToolSessionContext{BotID: "bot-1"}, adapters.ToolSearchMemory, map[string]any{
		"query": query,
	})
	if err != nil {
		t.Fatalf("CallTool error: %v", err)
	}
	structured, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("missing structuredContent in %v", result)
	}
	rawResults, ok := structured["results"].([]map[string]any)
	if !ok {
		t.Fatalf("unexpected results shape: %T", structured["results"])
	}
	return rawResults
}

func TestCallToolSearchMemoryReturnsSourceRefs(t *testing.T) {
	t.Parallel()
	p := NewBuiltinProvider(slog.Default(), NewGraphRuntime(nil, newFakeWikiStore(), newFakeStore()))

	if _, err := p.Add(context.Background(), adapters.AddRequest{
		Message:          "User prefers oolong tea",
		BotID:            "bot-1",
		SourceMessageIDs: []string{"sess-1/msg-1", "msg-2"},
	}); err != nil {
		t.Fatalf("Add error: %v", err)
	}

	results := callSearchMemory(t, p, "oolong tea")
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	refs, ok := results[0]["source_refs"].([]map[string]any)
	if !ok {
		t.Fatalf("unexpected source_refs shape: %T", results[0]["source_refs"])
	}
	want := []map[string]any{
		{"session_id": "sess-1", "message_id": "msg-1"},
		{"message_id": "msg-2"},
	}
	if len(refs) != len(want) {
		t.Fatalf("source_refs = %v, want %v", refs, want)
	}
	for i := range want {
		if fmt.Sprint(refs[i]) != fmt.Sprint(want[i]) {
			t.Fatalf("source_refs[%d] = %v, want %v", i, refs[i], want[i])
		}
	}
}

func TestCallToolSearchMemoryCapsSourceRefs(t *testing.T) {
	t.Parallel()
	p := NewBuiltinProvider(slog.Default(), NewGraphRuntime(nil, newFakeWikiStore(), newFakeStore()))

	refs := make([]string, 0, 10)
	for i := 1; i <= 10; i++ {
		refs = append(refs, fmt.Sprintf("sess-1/msg-%02d", i))
	}
	if _, err := p.Add(context.Background(), adapters.AddRequest{
		Message:          "User prefers oolong tea",
		BotID:            "bot-1",
		SourceMessageIDs: refs,
	}); err != nil {
		t.Fatalf("Add error: %v", err)
	}

	results := callSearchMemory(t, p, "oolong tea")
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	got, ok := results[0]["source_refs"].([]map[string]any)
	if !ok {
		t.Fatalf("unexpected source_refs shape: %T", results[0]["source_refs"])
	}
	if len(got) != 8 {
		t.Fatalf("expected 8 source_refs, got %d", len(got))
	}
	if got[0]["message_id"] != "msg-03" || got[7]["message_id"] != "msg-10" {
		t.Fatalf("expected most recent 8 refs, got first=%v last=%v", got[0], got[7])
	}
}

func TestCallToolSearchMemoryOmitsEmptySourceRefs(t *testing.T) {
	t.Parallel()
	p := NewBuiltinProvider(slog.Default(), NewGraphRuntime(nil, newFakeWikiStore(), newFakeStore()))

	if _, err := p.Add(context.Background(), adapters.AddRequest{
		Message: "User prefers oolong tea",
		BotID:   "bot-1",
	}); err != nil {
		t.Fatalf("Add error: %v", err)
	}

	results := callSearchMemory(t, p, "oolong tea")
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if _, present := results[0]["source_refs"]; present {
		t.Fatalf("expected source_refs to be omitted, got %v", results[0]["source_refs"])
	}
}
