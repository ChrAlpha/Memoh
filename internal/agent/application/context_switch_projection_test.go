package application

import (
	"reflect"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

func switchProjectionSource(t *testing.T, id, text string) contextfrag.ContextRef {
	t.Helper()
	record, err := historyfrag.FromDBMessage(messagepkg.Message{ID: id, Role: "assistant", Content: []byte(`"` + text + `"`)}, historyfrag.ScopeFallback{})
	if err != nil {
		t.Fatal(err)
	}
	return record.Ref
}

func TestContextSwitchProjectsOnlyCompleteCurrentCoverage(t *testing.T) {
	first := switchProjectionSource(t, "first", "one")
	second := switchProjectionSource(t, "second", "two")
	artifact := compaction.Artifact{ID: "summary", Summary: "Completed two steps", Coverage: []compaction.CoveredSource{{Ref: first}, {Ref: second}}}
	input := native.ContextSwitchInput{
		Request:   sdk.Request{Messages: []sdk.Message{sdk.UserMessage("keep task"), sdk.AssistantMessage("one"), sdk.AssistantMessage("two"), sdk.AssistantMessage("latest")}},
		Sources:   []native.ContextSwitchSource{{}, {Ref: first}, {Ref: second}, {}},
		Protected: []bool{true, false, false, true},
	}
	got := projectContextSwitch(input, []compaction.Artifact{artifact})
	if len(got.Messages) != 3 || !reflect.DeepEqual(got.SourceIndexes, []int{0, -1, 3}) {
		t.Fatalf("projection lost source positions: %+v", got)
	}
	if !reflect.DeepEqual(got.Messages[0], input.Request.Messages[0]) || !reflect.DeepEqual(got.Messages[2], input.Request.Messages[3]) {
		t.Fatal("projection changed protected input or latest step")
	}
	if !reflect.DeepEqual(got.Sources[1].Coverage, []contextfrag.ContextRef{first, second}) {
		t.Fatalf("summary lost coverage: %+v", got.Sources[1])
	}
	for _, name := range []string{"protected", "changed_source", "missing_source"} {
		t.Run(name, func(t *testing.T) {
			candidate := artifact
			candidate.Coverage = append([]compaction.CoveredSource(nil), artifact.Coverage...)
			request := input
			request.Protected = append([]bool(nil), input.Protected...)
			switch name {
			case "protected":
				request.Protected[2] = true
			case "changed_source":
				candidate.Coverage[1].Ref.ContentHash = "changed"
			case "missing_source":
				candidate.Coverage = append(candidate.Coverage, compaction.CoveredSource{Ref: switchProjectionSource(t, "unseen", "newer input")})
			}
			if got := projectContextSwitch(request, []compaction.Artifact{candidate}); got.Messages != nil {
				t.Fatalf("unsafe artifact adopted: %+v", got)
			}
		})
	}
}

func TestContextSwitchDoesNotMoveSummaryAcrossRetainedInput(t *testing.T) {
	first := switchProjectionSource(t, "first", "one")
	second := switchProjectionSource(t, "second", "two")
	artifact := compaction.Artifact{ID: "summary", Summary: "Two steps", Coverage: []compaction.CoveredSource{{Ref: first}, {Ref: second}}}
	input := native.ContextSwitchInput{
		Request: sdk.Request{Messages: []sdk.Message{sdk.AssistantMessage("one"), sdk.UserMessage("new requirement"), sdk.AssistantMessage("two")}},
		Sources: []native.ContextSwitchSource{{Ref: first}, {}, {Ref: second}}, Protected: []bool{false, true, false},
	}
	if got := projectContextSwitch(input, []compaction.Artifact{artifact}); got.Messages != nil {
		t.Fatalf("summary moved later evidence before retained input: %+v", got)
	}
}

func TestContextSwitchPreservesCoverageAcrossSummaryFusion(t *testing.T) {
	first := switchProjectionSource(t, "first", "one")
	second := switchProjectionSource(t, "second", "two")
	old := compaction.Artifact{ID: "old-summary", Summary: "One step", Coverage: []compaction.CoveredSource{{Ref: first}}}
	next := compaction.Artifact{ID: "new-summary", Summary: "Two steps", Coverage: []compaction.CoveredSource{{Ref: first}, {Ref: second}}}
	oldRecord := old.HistoryRecord(contextfrag.Scope{})
	input := native.ContextSwitchInput{
		Request: sdk.Request{Messages: []sdk.Message{modelMessageToSDKMessage(oldRecord.ModelMessage), sdk.AssistantMessage("two"), sdk.UserMessage("current")}},
		Sources: []native.ContextSwitchSource{{Ref: oldRecord.Ref, Coverage: []contextfrag.ContextRef{first}}, {Ref: second}, {}}, Protected: []bool{false, false, true},
	}
	got := projectContextSwitch(input, []compaction.Artifact{next})
	if len(got.Messages) != 2 || !reflect.DeepEqual(got.SourceIndexes, []int{-1, 2}) || !reflect.DeepEqual(got.Sources[0].Coverage, []contextfrag.ContextRef{first, second}) {
		t.Fatalf("fusion lost existing or new source: %+v", got)
	}
}
