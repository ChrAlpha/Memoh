package contextview

import (
	"context"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
)

func TestHistoryCollectorPreservesMessagesAndSeparatesCurrent(t *testing.T) {
	t.Parallel()

	messages := []sdk.Message{sdk.UserMessage("history"), sdk.AssistantMessage("answer"), sdk.UserMessage("  current \n")}
	current := 2
	cfg := HistoryMessagesConfig{Messages: messages, CurrentUserMessageIndex: &current}
	history, err := (&HistoryMessagesCollector{}).Collect(context.Background(), CollectRequest{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	currentFrags, err := (&materializedCurrentUserCollector{}).Collect(context.Background(), CollectRequest{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || len(currentFrags) != 1 || currentFrags[0].Slot != contextfrag.SlotCurrentUser {
		t.Fatalf("history/current = %#v / %#v", history, currentFrags)
	}
	msg := contextfrag.FragMessage(currentFrags[0])
	text := msg.Content[0].(sdk.TextPart)
	if text.Text != "  current \n" {
		t.Fatalf("current text = %q", text.Text)
	}
}

func TestMaterializedCurrentIndexFallsBackToLatestUser(t *testing.T) {
	t.Parallel()

	stale := 99
	frags, err := (&materializedCurrentUserCollector{}).Collect(context.Background(), CollectRequest{
		Config: HistoryMessagesConfig{Messages: []sdk.Message{sdk.UserMessage("first"), sdk.AssistantMessage("answer"), sdk.UserMessage("latest")}, CurrentUserMessageIndex: &stale},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(frags) != 1 || frags[0].ID != "message.002" {
		t.Fatalf("frags = %#v", frags)
	}
}

func TestHistoryCollectorsCarryEstimatesAndPinTheCurrentRequest(t *testing.T) {
	t.Parallel()

	current := 2
	memory := 1
	cfg := HistoryMessagesConfig{
		Messages: []sdk.Message{
			sdk.AssistantMessage("droppable history"),
			sdk.UserMessage("memory recall"),
			sdk.UserMessage("current request"),
		},
		CurrentUserMessageIndex: &current,
		MemoryMessageIndex:      &memory,
		TokenEstimates:          []int{11, 22, 33},
	}

	history, err := (&HistoryMessagesCollector{}).Collect(context.Background(), CollectRequest{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("history = %#v, want only the ordinary history message", history)
	}
	if history[0].TokenEstimate != 11 || history[0].Budget.Overflow != "" {
		t.Fatalf("history budget fields = %#v, want estimate 11 and no overflow policy", history[0])
	}

	currentFrags, err := (&materializedCurrentUserCollector{}).Collect(context.Background(), CollectRequest{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if len(currentFrags) != 1 || currentFrags[0].TokenEstimate != 33 ||
		currentFrags[0].Budget.Overflow != contextfrag.OverflowKeep {
		t.Fatalf("current fragment = %#v, want estimate 33 and OverflowKeep", currentFrags)
	}
}

func TestHistoryCollectorFallsBackForMissingEstimates(t *testing.T) {
	t.Parallel()

	frags, err := (&HistoryMessagesCollector{}).Collect(context.Background(), CollectRequest{
		Config: HistoryMessagesConfig{
			Messages:       []sdk.Message{sdk.UserMessage("old"), sdk.AssistantMessage("tail")},
			TokenEstimates: []int{7},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(frags) != 2 || frags[0].TokenEstimate != 7 || frags[1].TokenEstimate != 0 {
		t.Fatalf("token estimates = %#v, want 7 then renderer fallback", frags)
	}
	// Selection keeps all history, so no message needs an overflow policy.
	if frags[0].Budget.Overflow != "" || frags[1].Budget.Overflow != "" {
		t.Fatalf("overflow policies = %#v / %#v", frags[0].Budget, frags[1].Budget)
	}
}
