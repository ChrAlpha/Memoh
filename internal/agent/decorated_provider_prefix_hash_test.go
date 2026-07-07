package agent

import (
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

func TestDecoratedProviderPrefixHashSystemPrependedShiftsSpanByOne(t *testing.T) {
	t.Parallel()

	messages := []sdk.Message{sdk.SystemMessage("sys"), sdk.UserMessage("m1"), sdk.UserMessage("m2")}

	withSystem := decoratedProviderPrefixHash("", messages, nil, 1, true)
	withoutSystem := decoratedProviderPrefixHash("", messages, nil, 1, false)

	if withSystem == withoutSystem {
		t.Fatal("systemPrepended must extend the span by one message")
	}
	wantWith, _ := contextfrag.ProviderPayloadHashAndBytes("", messages[:2], []sdk.Tool(nil))
	if withSystem != wantWith {
		t.Fatalf("systemPrepended hash = %q, want %q (messages[:2])", withSystem, wantWith)
	}
	wantWithout, _ := contextfrag.ProviderPayloadHashAndBytes("", messages[:1], []sdk.Tool(nil))
	if withoutSystem != wantWithout {
		t.Fatalf("non-prepended hash = %q, want %q (messages[:1])", withoutSystem, wantWithout)
	}
}

func TestDecoratedProviderPrefixHashClampsAtSliceBounds(t *testing.T) {
	t.Parallel()

	messages := []sdk.Message{sdk.UserMessage("m1"), sdk.UserMessage("m2")}

	got := decoratedProviderPrefixHash("sys", messages, nil, 5, false)
	want, _ := contextfrag.ProviderPayloadHashAndBytes("sys", messages[:2], []sdk.Tool(nil))
	if got != want {
		t.Fatalf("clamped hash = %q, want %q (full messages slice)", got, want)
	}

	gotPrepended := decoratedProviderPrefixHash("sys", messages, nil, 5, true)
	if gotPrepended != want {
		t.Fatalf("clamped+prepended hash = %q, want %q (still clamped to len(messages))", gotPrepended, want)
	}
}

func TestDecoratedProviderPrefixHashZeroStableCountStillCoversPrependedSystem(t *testing.T) {
	t.Parallel()

	messages := []sdk.Message{sdk.SystemMessage("sys"), sdk.UserMessage("m1")}

	got := decoratedProviderPrefixHash("", messages, nil, 0, true)
	want, _ := contextfrag.ProviderPayloadHashAndBytes("", messages[:1], []sdk.Tool(nil))
	if got != want {
		t.Fatalf("hash = %q, want %q (span covers only the prepended system message)", got, want)
	}

	gotWithoutPrepend := decoratedProviderPrefixHash("", messages, nil, 0, false)
	wantEmpty, _ := contextfrag.ProviderPayloadHashAndBytes("", messages[:0], []sdk.Tool(nil))
	if gotWithoutPrepend != wantEmpty {
		t.Fatalf("hash = %q, want %q (empty span without a prepended system message)", gotWithoutPrepend, wantEmpty)
	}
}
