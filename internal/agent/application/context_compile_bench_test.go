package application

import (
	"context"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/contextview"
)

var compileSink native.RunConfig

// compileBenchConfig is what resolve hands prepareRunConfig for an armed
// chat turn over rounds of history: a checkpoint summary, then rounds of a
// question, a tool call, its result and an answer.
func compileBenchConfig(rounds int) native.RunConfig {
	records := []historyfrag.HistoryRecord{historyfrag.SummaryRecord("summary-0", "earlier work "+strings.Repeat("s", 2000), contextfrag.Scope{})}
	messages := []ModelMessage{records[0].ModelMessage}
	row := func(id string, message sdk.Message) {
		converted := sdkMessagesToModelMessages([]sdk.Message{message})[0]
		converted.ContextSource = &turn.ContextMessageSource{Kind: "history", ID: id}
		messages = append(messages, converted)
	}
	for i := range rounds {
		call := fmt.Sprintf("call-%d", i)
		row(fmt.Sprintf("q-%d", i), sdk.UserMessage(fmt.Sprintf("question %d %s", i, strings.Repeat("q", 300))))
		row(fmt.Sprintf("c-%d", i), sdk.Message{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{sdk.ToolCallPart{
			ToolCallID: call, ToolName: "exec", Input: toolexec.ArgumentsFromValue(map[string]any{"command": "ls"}),
		}}})
		row(fmt.Sprintf("r-%d", i), sdk.ToolMessage(sdk.ToolResultPart{ToolCallID: call, ToolName: "exec", Result: toolexec.OutputFromValue(strings.Repeat("output line\n", 160))}))
		row(fmt.Sprintf("a-%d", i), sdk.AssistantMessage(fmt.Sprintf("answer %d %s", i, strings.Repeat("a", 400))))
	}
	cfg := native.RunConfig{
		System:                       "system",
		Query:                        "current question",
		ContextBudgetMaxTokens:       1_000_000,
		ContextTrimmableMessages:     len(messages),
		ContextFrags:                 historyContextFragsForMessages(messages, records),
		ContextProvenance:            contextProvenance(messages, nil),
		Messages:                     modelMessagesToSDKMessages(messages),
		ForkContextSourceMessageIDs:  make([]string, len(messages)),
		ContextHistoryTokenEstimates: make([]int, len(messages)),
		SwitchContext: func(context.Context, native.ContextSwitch) (string, error) {
			return "", nil
		},
	}
	return cfg.RefreshContextFrag()
}

// BenchmarkTurnContextCompile measures what one armed chat turn spends
// compiling its request before the first dispatch: prepareRunConfig, which
// collects the source fragments, and the provider view.
func BenchmarkTurnContextCompile(b *testing.B) {
	apply := contextview.ProviderRunConfigApplier(nil)
	for _, rounds := range []int{50, 500} {
		base := compileBenchConfig(rounds)
		b.Run(fmt.Sprintf("rounds%d/prepare", rounds), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				compileSink = (&Service{}).prepareRunConfig(context.Background(), base)
			}
		})
		prepared := (&Service{}).prepareRunConfig(context.Background(), base)
		b.Run(fmt.Sprintf("rounds%d/view", rounds), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				got, err := apply(context.Background(), prepared)
				if err != nil {
					b.Fatal(err)
				}
				compileSink = got
			}
		})
	}
}
