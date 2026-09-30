package client

import (
	"encoding/json"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/messageconv"
)

func TestPromptUsageFromACPMapsTokenDetails(t *testing.T) {
	cacheRead := 3
	cacheWrite := 2
	thought := 5

	got := promptUsageFromACP(&acp.Usage{
		InputTokens:       10,
		OutputTokens:      7,
		TotalTokens:       17,
		CachedReadTokens:  &cacheRead,
		CachedWriteTokens: &cacheWrite,
		ThoughtTokens:     &thought,
	})

	if got == nil {
		t.Fatal("usage = nil")
	}
	if got.InputTokens != 10 || got.OutputTokens != 7 || got.TotalTokens != 17 {
		t.Fatalf("usage totals = %+v", got)
	}
	if got.CachedInputTokens != 3 || got.InputTokenDetails.CacheReadTokens != 3 || got.InputTokenDetails.CacheWriteTokens != 2 {
		t.Fatalf("input token details = %+v", got)
	}
	if got.ReasoningTokens != 5 || got.OutputTokenDetails.ReasoningTokens != 5 {
		t.Fatalf("reasoning token details = %+v", got)
	}
}

func TestAttachUsageToLastAssistant(t *testing.T) {
	usage := &sdk.Usage{InputTokens: 10, OutputTokens: 7, TotalTokens: 17}
	output := []sdk.Message{
		sdk.UserMessage("question"),
		{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{sdk.TextPart{Text: "first"}}},
		{Role: sdk.MessageRoleTool, Content: []sdk.MessagePart{sdk.ToolResultPart{ToolCallID: "call-1", ToolName: "exec", Result: toolexec.OutputFromValue("ok")}}},
		{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{sdk.TextPart{Text: "final"}}},
	}

	got := attachUsageToLastAssistant(output, usage)

	if got[1].Usage != nil {
		t.Fatalf("first assistant usage = %+v, want nil", got[1].Usage)
	}
	if got[3].Usage != usage {
		t.Fatalf("final assistant usage = %+v, want mapped usage", got[3].Usage)
	}
}

func TestPromptUsageFromACPPreservesCacheReporting(t *testing.T) {
	for _, tt := range []struct {
		name string
		read *int
	}{
		{name: "absent"},
		{name: "zero", read: acp.Ptr(0)},
		{name: "positive", read: acp.Ptr(3)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			usage := promptUsageFromACP(&acp.Usage{InputTokens: 10, OutputTokens: 7, TotalTokens: 17, CachedReadTokens: tt.read})
			output := attachUsageToLastAssistant([]sdk.Message{{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{sdk.TextPart{Text: "ok"}}}}, usage)
			stored := messageconv.SDKMessagesToModelMessages(output)
			var got sdk.Usage
			if err := json.Unmarshal(stored[0].Usage, &got); err != nil {
				t.Fatal(err)
			}
			if got.CacheReadTokensReported != (tt.read != nil) {
				t.Fatalf("cache reporting lost: %s", stored[0].Usage)
			}
			wantRead := 0
			if tt.read != nil {
				wantRead = *tt.read
			}
			if got.InputTokens != 10 || got.OutputTokens != 7 || got.TotalTokens != 17 || got.InputTokenDetails.CacheReadTokens != wantRead {
				t.Fatalf("usage lost: %+v", got)
			}
		})
	}
}
