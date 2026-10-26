package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextlimit "github.com/felinics/memoh/internal/agent/context/limit"
	"github.com/felinics/memoh/internal/agent/toolexec"
)

func TestLimitToolOutputPrunesLargeStringLeaves(t *testing.T) {
	t.Parallel()

	large := "HEAD\n" + strings.Repeat("0123456789", 200) + "\nTAIL"
	result := LimitToolOutput(map[string]any{
		"content": large,
		"ok":      true,
	}, "test_tool", ToolOutputLimit{MaxBytes: 512, MaxLines: 80})

	structured, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result = %#v, want map", result)
	}
	content, ok := structured["content"].(string)
	if !ok {
		t.Fatalf("content = %#v, want string", structured["content"])
	}
	if len(content) >= len(large) {
		t.Fatalf("content was not pruned: got %d bytes, original %d", len(content), len(large))
	}
	if !strings.Contains(content, "[memoh pruned]") {
		t.Fatalf("content missing prune marker:\n%s", content)
	}
	if !strings.Contains(content, "HEAD") || !strings.Contains(content, "TAIL") {
		t.Fatalf("content did not preserve head and tail:\n%s", content)
	}
	if structured["ok"] != true {
		t.Fatalf("non-string field changed: %#v", structured)
	}
}

func TestLimitToolOutputFallsBackWhenStructuredJSONStillExceedsLimit(t *testing.T) {
	t.Parallel()

	large := map[string]any{}
	for i := 0; i < 24; i++ {
		large[fmt.Sprintf("key_%02d", i)] = strings.Repeat("x", 128)
	}

	result := LimitToolOutput(large, "huge_tool", ToolOutputLimit{MaxBytes: 512, MaxLines: 120})
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if len(raw) > 512 {
		t.Fatalf("result JSON bytes = %d, want <= 512\n%s", len(raw), raw)
	}
	structured, ok := result.(map[string]any)
	if !ok || structured["_memoh_truncated"] != true {
		t.Fatalf("result = %#v, want fallback truncation marker", result)
	}
}

func TestLimitToolOutputNormalizesTinyPositiveByteLimit(t *testing.T) {
	t.Parallel()

	result := LimitToolOutput(map[string]any{
		"content": strings.Repeat("x", 1024),
	}, "tiny_tool", ToolOutputLimit{MaxBytes: 1, MaxLines: 80})

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if len(raw) > 256 {
		t.Fatalf("result JSON bytes = %d, want <= normalized minimum 256\n%s", len(raw), raw)
	}
}

func TestLimitToolOutputPreservesErrorSignalOnFallback(t *testing.T) {
	t.Parallel()

	result := LimitToolOutput(map[string]any{
		"isError": true,
		"content": "HEAD\n" + strings.Repeat("error detail ", 300) + "\nTAIL",
	}, "error_tool", ToolOutputLimit{MaxBytes: 512, MaxLines: 80})

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if len(raw) > 512 {
		t.Fatalf("result JSON bytes = %d, want <= 512\n%s", len(raw), raw)
	}
	structured, ok := result.(map[string]any)
	if !ok || structured["isError"] != true {
		t.Fatalf("result = %#v, want isError preserved", result)
	}
}

// A call's share of its step's bound cuts its result, error or not, below the
// tool's own bound; a share above that bound, or none, leaves the tool's own.
func TestWrapToolOutputLimitsKeepsTheCallsShare(t *testing.T) {
	t.Parallel()

	large := "HEAD\n" + strings.Repeat("0123456789", 1000) + "\nTAIL"
	wrapped := WrapToolOutputLimits([]toolexec.Tool{{
		Name: "big_tool",
		Execute: func(_ *toolexec.ToolExecContext, input sdk.ToolArguments) (sdk.ToolOutput, error) {
			if strings.Contains(string(input.JSON), "fail") {
				return sdk.ToolOutput{}, errors.New(large)
			}
			return sdk.TextOutput(large), nil
		},
	}}, ToolOutputLimit{MaxBytes: 4096, MaxLines: 2000})
	result := func(share int, fail bool) string {
		t.Helper()
		input := map[string]any{}
		if fail {
			input["fail"] = true
		}
		out, err := wrapped[0].Execute(&toolexec.ToolExecContext{Context: context.Background(), ToolName: "big_tool", OutputBytes: share}, toolexec.ArgumentsFromValue(input))
		if (err != nil) != fail {
			t.Fatalf("share %d: err = %v, want failure %v", share, err, fail)
		}
		if err != nil {
			return err.Error()
		}
		return out.Text
	}

	for _, fail := range []bool{false, true} {
		own := result(0, fail)
		if size := len(contextlimit.MarshalString(own)); size > 4096 || !strings.Contains(own, "[memoh pruned]") {
			t.Fatalf("failure %v: unshared result = %d bytes, want it pruned to the tool's own 4096", fail, size)
		}
		if above := result(8192, fail); above != own {
			t.Fatalf("failure %v: a share above the tool's bound changed the result", fail)
		}
		if size := len(contextlimit.MarshalString(result(1024, fail))); size > 1024 || size >= len(own) {
			t.Fatalf("failure %v: shared result = %d bytes, want at most the share of 1024", fail, size)
		}
	}
}

func TestWrapToolOutputLimitsPrunesErrors(t *testing.T) {
	t.Parallel()

	wrapped := WrapToolOutputLimits([]toolexec.Tool{{
		Name: "broken_tool",
		Execute: func(*toolexec.ToolExecContext, sdk.ToolArguments) (sdk.ToolOutput, error) {
			return sdk.ToolOutput{}, errors.New("HEAD\n" + strings.Repeat("error detail ", 300) + "\nTAIL")
		},
	}}, ToolOutputLimit{MaxBytes: 512, MaxLines: 80})

	_, err := wrapped[0].Execute(&toolexec.ToolExecContext{Context: context.Background(), ToolName: "broken_tool"}, toolexec.ArgumentsFromValue(nil))
	if err == nil {
		t.Fatal("Execute() error = nil, want limited error")
	}
	if len(err.Error()) >= len("HEAD\n"+strings.Repeat("error detail ", 300)+"\nTAIL") {
		t.Fatalf("error was not pruned: %d bytes", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "[memoh pruned]") {
		t.Fatalf("limited error missing prune marker:\n%s", err.Error())
	}
	if !strings.Contains(err.Error(), "HEAD") || !strings.Contains(err.Error(), "TAIL") {
		t.Fatalf("limited error did not preserve head and tail:\n%s", err.Error())
	}
}
