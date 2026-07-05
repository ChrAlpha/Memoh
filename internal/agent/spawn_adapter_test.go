package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/agent/sessionmode"
	"github.com/memohai/memoh/internal/agent/tools"
)

// currentTimeLine extracts the "Current time: ..." line a materialized spawn
// query must be prefixed with, failing the test if the prefix is missing.
func currentTimeLine(t *testing.T, text string) string {
	t.Helper()
	const prefix = "Current time: "
	if !strings.HasPrefix(text, prefix) {
		t.Fatalf("expected message to start with %q, got %q", prefix, text)
	}
	line, _, _ := strings.Cut(text, "\n")
	return strings.TrimPrefix(line, prefix)
}

func TestSpawnAdapterPrefixesQueryWithCurrentTime(t *testing.T) {
	t.Parallel()
	modelProvider := &usageRecordingProvider{}
	recorder := &applierRecorder{}
	a := newApplierTestAgent(recorder)
	adapter := NewSpawnAdapter(a)

	before := time.Now()
	if _, err := adapter.Generate(context.Background(), tools.SpawnRunConfig{
		Model: &sdk.Model{
			ID:       "spawn-time-model",
			Provider: modelProvider,
			Type:     sdk.ModelTypeChat,
		},
		System: "subagent system",
		Query:  "do the task",
		Identity: tools.SpawnIdentity{
			BotID:      "bot-1",
			SessionID:  "session-1",
			IsSubagent: true,
		},
	}); err != nil {
		t.Fatalf("spawn Generate error: %v", err)
	}
	after := time.Now()

	_, seen := recorder.snapshot()
	if len(seen.Messages) == 0 {
		t.Fatal("expected the spawn query to be materialized into RunConfig.Messages")
	}
	text := textOfMessage(seen.Messages[len(seen.Messages)-1])

	if !strings.Contains(text, "do the task") {
		t.Fatalf("expected message to retain the original query text, got %q", text)
	}

	timeLine := currentTimeLine(t, text)
	parsed, err := time.Parse(time.RFC3339, timeLine)
	if err != nil {
		t.Fatalf("expected current time line to parse as RFC3339, got %q: %v", timeLine, err)
	}
	if parsed.Before(before.Add(-time.Second)) || parsed.After(after.Add(time.Second)) {
		t.Fatalf("parsed time %v not within window [%v, %v]", parsed, before, after)
	}
}

func TestSpawnAdapterUsesIdentityTimezoneForCurrentTime(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	modelProvider := &usageRecordingProvider{}
	recorder := &applierRecorder{}
	a := newApplierTestAgent(recorder)
	adapter := NewSpawnAdapter(a)

	if _, err := adapter.Generate(context.Background(), tools.SpawnRunConfig{
		Model: &sdk.Model{
			ID:       "spawn-tz-model",
			Provider: modelProvider,
			Type:     sdk.ModelTypeChat,
		},
		System: "subagent system",
		Query:  "do the task",
		Identity: tools.SpawnIdentity{
			BotID:            "bot-1",
			SessionID:        "session-1",
			IsSubagent:       true,
			TimezoneLocation: loc,
		},
	}); err != nil {
		t.Fatalf("spawn Generate error: %v", err)
	}

	_, seen := recorder.snapshot()
	text := textOfMessage(seen.Messages[len(seen.Messages)-1])
	timeLine := currentTimeLine(t, text)
	if !strings.HasSuffix(timeLine, "+08:00") {
		t.Fatalf("expected current time line to carry the Asia/Shanghai offset, got %q", timeLine)
	}
}

func TestSpawnSystemPromptOmitsCurrentTime(t *testing.T) {
	t.Parallel()
	prompt := SpawnSystemPrompt(sessionmode.Subagent)
	if strings.Contains(prompt, "Current time") {
		t.Fatalf("expected subagent system prompt to stay free of current time, got:\n%s", prompt)
	}
}
