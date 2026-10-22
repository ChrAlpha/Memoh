package agent

import (
	"fmt"
	"testing"
)

func TestComparePromptsAppendOnlyReusesTheWholePreviousPrompt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ prev, next string }{
		"completions": {
			`{"model":"m","stream":true,"messages":[{"role":"system","content":"S"},{"role":"user","content":"u1"}],"tools":[{"type":"function","function":{"name":"t"}}]}`,
			`{"stream":true,"model":"m","tools":[ {"type":"function","function":{"name":"t"}} ],"messages":[{"content":"S","role":"system"},{"role":"user","content":"u1"},{"role":"assistant","content":"a1"}]}`,
		},
		"responses": {
			`{"model":"m","instructions":"S","input":[{"role":"user","content":"u1"}],"prompt_cache_key":"k1"}`,
			`{"model":"m","instructions":"S","input":[{"role":"user","content":"u1"},{"role":"assistant","content":"a1"}],"prompt_cache_key":"k1"}`,
		},
		"anthropic cache_control moves": {
			`{"model":"m","system":[{"type":"text","text":"S","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral"}}]}]}`,
			`{"model":"m","system":[{"type":"text","text":"S","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"u1"}]},{"role":"assistant","content":[{"type":"text","text":"a1","cache_control":{"type":"ephemeral"}}]}]}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			report, err := comparePrompts([]byte(tc.prev), []byte(tc.next))
			if err != nil {
				t.Fatal(err)
			}
			if report.Break != nil || report.Ratio() != 1 {
				t.Fatalf("report = %+v (%v), want the whole previous prompt reused", report, report.Break)
			}
		})
	}
}

func TestComparePromptsLocatesTheFirstDifference(t *testing.T) {
	t.Parallel()
	system := func(memory string) string {
		return fmt.Sprintf(`{"messages":[{"role":"system","content":%q},{"role":"user","content":"u1"}]}`,
			"intro\n\n## AGENTS.md\n\nrules\n\n## MEMORY.md\n\n"+memory)
	}
	for name, tc := range map[string]struct {
		prev, next string
		part       string
		unit       string
	}{
		"memory overview": {system("remembers A"), system("remembers B"), "system prompt: memory overview (MEMORY.md)", "messages[0]"},
		"workspace file": {
			`{"messages":[{"role":"system","content":"intro\n\n## AGENTS.md\n\nrules v1"},{"role":"user","content":"u1"}]}`,
			`{"messages":[{"role":"system","content":"intro\n\n## AGENTS.md\n\nrules v2"},{"role":"user","content":"u1"}]}`,
			"system prompt: workspace file (AGENTS.md)", "messages[0]",
		},
		"system text before any heading": {
			`{"messages":[{"role":"system","content":"time 10:00"},{"role":"user","content":"u1"}]}`,
			`{"messages":[{"role":"system","content":"time 10:01"},{"role":"user","content":"u1"}]}`,
			"system prompt: prompt text", "messages[0]",
		},
		"anthropic system blocks": {
			`{"system":[{"type":"text","text":"intro\n\n## MEMORY.md\n\nA"}],"messages":[{"role":"user","content":"u1"}]}`,
			`{"system":[{"type":"text","text":"intro\n\n## MEMORY.md\n\nB"}],"messages":[{"role":"user","content":"u1"}]}`,
			"system prompt: memory overview (MEMORY.md)", "system[0]",
		},
		"tool definitions": {
			`{"tools":[{"name":"a"}],"messages":[{"role":"user","content":"u1"}]}`,
			`{"tools":[{"name":"b"}],"messages":[{"role":"user","content":"u1"}]}`,
			"tool definitions", "tools[0]",
		},
		"history": {
			`{"messages":[{"role":"user","content":"u1"},{"role":"assistant","content":"a1"}]}`,
			`{"messages":[{"role":"user","content":"u1 edited"},{"role":"assistant","content":"a1"},{"role":"user","content":"u2"}]}`,
			"history", "messages[0]",
		},
		"current turn": {
			`{"messages":[{"role":"user","content":"u1"},{"role":"assistant","content":"a1"}]}`,
			`{"messages":[{"role":"user","content":"u1"},{"role":"assistant","content":"a1 rewritten"}]}`,
			"current turn", "messages[1]",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			report, err := comparePrompts([]byte(tc.prev), []byte(tc.next))
			if err != nil {
				t.Fatal(err)
			}
			if report.Break == nil {
				t.Fatalf("no break found, ratio %v", report.Ratio())
			}
			if report.Break.Part != tc.part || report.Break.Unit != tc.unit {
				t.Fatalf("break = %v, want %s in %s", report.Break, tc.part, tc.unit)
			}
			if report.Ratio() >= 1 {
				t.Fatalf("ratio = %v, want less than 1 past a break", report.Ratio())
			}
		})
	}
}
