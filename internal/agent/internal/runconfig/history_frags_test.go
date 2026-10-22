package runconfig

import (
	"context"
	"reflect"
	"testing"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/internal/transcript"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/contextview"
)

func TestHistoryContextFragsForMessagesCarriesActiveSummaryCoverage(t *testing.T) {
	t.Parallel()

	covered := []contextfrag.ContextRef{
		{Namespace: "bot_history_message", ID: "row-1", Schema: contextfrag.SchemaContextRef, Durability: contextfrag.RefDurable},
	}
	summary := historyfrag.SummaryRecord("compact-1", "condensed", covered, contextfrag.Scope{BotID: "bot-1"})
	records := []historyfrag.HistoryRecord{
		summary,
		agenttest.HistoryRecord("row-2", runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent("new")}, nil),
	}
	messages := []runreq.ModelMessage{
		summary.ModelMessage,
		{Role: "user", Content: runreq.NewTextContent("new")},
	}

	frags := transcript.ContextFragsForMessages(messages, records)

	if len(frags) != 1 {
		t.Fatalf("summary frags = %d, want 1: %#v", len(frags), frags)
	}
	if frags[0].ID != "message.000" || frags[0].Provenance.Index != 0 {
		t.Fatalf("summary frag should align with final message index: %#v", frags[0])
	}
	if frags[0].Kind != contextfrag.KindConversationSummary || frags[0].Coverage == nil {
		t.Fatalf("summary frag lost kind/coverage: %#v", frags[0])
	}

	cfg := native.RunConfig{
		Messages:     runreq.ModelMessagesToSDKMessages(messages),
		ContextFrags: frags,
	}.RefreshContextFrag()
	if len(cfg.ContextManifest.CoverageTrace) != 1 {
		t.Fatalf("run config manifest lost summary coverage: %#v", cfg.ContextManifest)
	}
	summaryItems := 0
	for _, item := range cfg.ContextManifest.Items {
		if item.Kind == contextfrag.KindConversationSummary {
			summaryItems++
		}
	}
	if summaryItems != 1 {
		t.Fatalf("run config manifest summary items = %d, want 1: %#v", summaryItems, cfg.ContextManifest.Items)
	}

	cfg.ContextQueryMaterialized = true
	cfg.ContextSourceFrags = buildProviderSourceFrags(context.Background(), cfg, nil, nil)
	providerCfg := contextview.ApplyProviderRunConfig(context.Background(), nil, cfg)
	if !reflect.DeepEqual(providerCfg.Messages, cfg.Messages) {
		t.Fatalf("provider messages changed: got %#v want %#v", providerCfg.Messages, cfg.Messages)
	}
	if len(providerCfg.ContextManifest.CoverageTrace) != 1 {
		t.Fatalf("provider manifest lost summary coverage: %#v", providerCfg.ContextManifest)
	}
	providerSummaryItems := 0
	for _, item := range providerCfg.ContextManifest.Items {
		if item.Kind == contextfrag.KindConversationSummary {
			providerSummaryItems++
			if item.Ref.Namespace != summary.Ref.Namespace || item.Ref.ID != summary.Ref.ID ||
				item.Ref.Version != summary.Ref.Version || item.Ref.Durability != contextfrag.RefDurable ||
				item.Ref.ContentHash == "" {
				t.Fatalf("provider summary ref lost durable identity: got %#v source %#v", item.Ref, summary.Ref)
			}
		}
	}
	if providerSummaryItems != 1 {
		t.Fatalf("provider manifest summary items = %d, want 1: %#v", providerSummaryItems, providerCfg.ContextManifest.Items)
	}
}
