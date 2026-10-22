package persist

import (
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/step"
)

// A run's committed rows go to memory extraction when it finishes, unless the
// request asked to skip it.
func TestFinishHonorsSkipMemoryExtraction(t *testing.T) {
	for _, skip := range []bool{false, true} {
		fx := newSteerFixture(t)
		handle := fx.handle
		rounds := &roundRecorder{}
		fx.writer.rounds = rounds
		ctx := replacementTestContext(fx)
		committer := fx.writer.NewStepCommitter(ctx, runreq.ChatRequest{
			BotID: handle.BotID, ThreadID: handle.SessionID, RunID: handle.RunID, RunHandle: handle,
			TurnID: "turn-1", TurnPosition: int64Pointer(1), Query: "hello", SkipMemoryExtraction: skip,
		}, replacementResolvedContext())
		if committer == nil {
			t.Fatal("missing step committer")
		}
		if _, err := committer.commit(ctx, 0, &step.Record{Messages: []sdk.Message{sdk.AssistantMessage("pong")}}); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if err := committer.Finish(ctx, 0); err != nil {
			t.Fatalf("finish: %v", err)
		}
		rounds.mu.Lock()
		got := len(rounds.rounds)
		rounds.mu.Unlock()
		if want := map[bool]int{false: 1, true: 0}[skip]; got != want {
			t.Fatalf("skip=%t: memory extraction ran %d times, want %d", skip, got, want)
		}
	}
}
