package agent

import (
	"context"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

func TestBuildGenerateOptionsRecordsFinalInputHash(t *testing.T) {
	t.Parallel()

	ledger := contextfrag.NewMutationLedger()
	cfg := RunConfig{
		System:           "system prompt",
		Messages:         []sdk.Message{sdk.UserMessage("hello")},
		ContextMutations: ledger,
	}

	_ = (*Agent)(nil).buildGenerateOptions(context.Background(), cfg, nil, nil, nil)

	if ledger.FinalInputHash() == "" {
		t.Fatal("buildGenerateOptions should record the final provider input hash")
	}
	want := contextfrag.ProviderInputHash("system prompt", []sdk.Message{sdk.UserMessage("hello")})
	if ledger.FinalInputHash() != want {
		t.Fatalf("final hash = %q, want %q", ledger.FinalInputHash(), want)
	}
}

func TestBuildGenerateOptionsNilLedgerSafe(t *testing.T) {
	t.Parallel()

	cfg := RunConfig{
		System:   "system prompt",
		Messages: []sdk.Message{sdk.UserMessage("hello")},
	}

	opts := (*Agent)(nil).buildGenerateOptions(context.Background(), cfg, nil, nil, nil)
	if len(opts) == 0 {
		t.Fatal("options should still be produced without a ledger")
	}
}
