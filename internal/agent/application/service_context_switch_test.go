package application

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// A continuation renders everything as history: the turn before, the run's
// request row, its deferred call and the approved result. Each message names
// the row it was loaded from, a summary its artifact, and a pipeline message
// its inbound source; only the request row and the live input are kept in
// place, and a message with no source is not part of the view.
func TestContextProvenanceNamesWhatEachMessageRenders(t *testing.T) {
	t.Parallel()

	text := func(role, content string) ModelMessage {
		return ModelMessage{Role: role, Content: turn.NewTextContent(content)}
	}
	messages := []ModelMessage{
		text("user", "<summary>older</summary>"),
		text("user", "earlier question"),
		text("assistant", "earlier answer"),
		text("user", "continued request"),
		text("assistant", "deferred call"),
		text("tool", "approved result"),
		text("user", "unsourced"),
	}
	pipeline := text("user", "pipeline message")
	pipeline.ContextSource = &turn.ContextMessageSource{Kind: "external", ID: "rc-1"}
	live := text("user", "live input")
	live.ContextSource = &turn.ContextMessageSource{Kind: "external", ID: "in-1", Current: true}
	messages = append(messages, pipeline, live)
	records := []historyfrag.HistoryRecord{{ModelMessage: messages[0], SourceKind: historyfrag.SourceCompactionLog, Ref: contextfrag.ContextRef{ID: "artifact-0"}}}
	for i, id := range []string{"old-user", "old-assistant", "request", "call", "result"} {
		records = append(records, historyfrag.HistoryRecord{ModelMessage: messages[i+1], DBMessageID: id})
	}
	req := ChatRequest{PersistedUserMessageID: "request", inputInHistory: true}

	got := contextProvenance(messages, records, compactionProtectedSources(req))
	want := []struct {
		kind, id string
		anchor   bool
	}{
		{"summary", "artifact-0", false},
		{"history", "old-user", false},
		{"history", "old-assistant", false},
		{"history", "request", true},
		{"history", "call", false},
		{"history", "result", false},
		{"external", "rc-1", false},
		{"external", "in-1", true},
	}
	if len(got) != len(want) {
		t.Fatalf("provenance for %d messages, want %d", len(got), len(want))
	}
	for i, w := range want {
		source := turn.ContextMessageSource{Kind: w.kind, ID: w.id}
		if !reflect.DeepEqual(got[i].Sources, []turn.ContextMessageSource{source}) || got[i].Anchor != w.anchor {
			t.Fatalf("message %d renders %v anchor=%v, want %v anchor=%v", i, got[i].Sources, got[i].Anchor, source, w.anchor)
		}
	}
	if !reflect.DeepEqual(stepProvenance([]messagepkg.Message{
		{ID: "steer", Role: "user"}, {ID: "feedback", Role: "user", Metadata: map[string]any{messagepkg.MessageSourceMetadataKey: messagepkg.MessageSourceInternalFeedback}}, {ID: "call", Role: "assistant"},
	}), []native.MessageProvenance{
		{Sources: []turn.ContextMessageSource{{Kind: "history", ID: "steer"}}, Anchor: true},
		{Sources: []turn.ContextMessageSource{{Kind: "history", ID: "feedback"}}},
		{Sources: []turn.ContextMessageSource{{Kind: "history", ID: "call"}}},
	}) {
		t.Fatal("a step's steer is kept in place; its runtime feedback and output are not")
	}
}

type switchRunner struct {
	cfg    compaction.TriggerConfig
	passes []compaction.Pass
	err    error
}

func (r *switchRunner) Compact(ctx context.Context, cfg compaction.TriggerConfig, admit func(context.Context, compaction.Pass) (bool, error)) ([]compaction.Pass, error) {
	r.cfg = cfg
	for i, pass := range r.passes {
		if fits, err := admit(ctx, pass); err != nil || fits {
			return r.passes[:i+1], err
		}
	}
	return r.passes, r.err
}

// The event an armed run asks for reads the sources its request renders, not
// a window of the session, keeps the run's input and steers in place and its
// latest step outside the history budget, and splices every pass it commits
// into the request until it fits. A failed event names its reason.
func TestSwitchContextFoldsTheRequestView(t *testing.T) {
	t.Parallel()

	rows := func(ids ...string) []turn.ContextMessageSource {
		sources := make([]turn.ContextMessageSource, len(ids))
		for i, id := range ids {
			sources[i] = turn.ContextMessageSource{Kind: "history", ID: id}
		}
		return sources
	}
	passes := []compaction.Pass{
		{ArtifactID: "a1", Summary: "first", Covered: rows("old-user")},
		{ArtifactID: "a2", Summary: "second", Covered: rows("old-user", "call"), Absorbed: []string{"a1"}},
	}
	for _, tc := range []struct {
		name   string
		fitsAt int
		err    error
		want   error
		reason string
	}{
		{name: "second pass fits", fitsAt: 2},
		{name: "never fits", reason: "still_over", want: contextfrag.ErrBudgetUnsatisfied},
		{name: "summarizer down", err: &compaction.EventError{Reason: compaction.ReasonSummarizerFailed, Err: errors.New("503")}, reason: "summarizer_failed", want: contextfrag.ErrBudgetUnsatisfied},
		{name: "protected overflow", err: &compaction.EventError{Reason: compaction.ReasonProtectedOverflow, Err: errors.New("tail")}, reason: "protected_overflow", want: contextfrag.ErrProtectedContextOverflow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			service, _ := newControllerPolicyService(t)
			runner := &switchRunner{passes: passes, err: tc.err}
			if tc.err != nil {
				runner.passes = nil
			}
			service.compactionService = runner
			committer := &agentStepCommitter{persisted: []messagepkg.Message{{ID: "input", Role: "user"}, {ID: "call", Role: "assistant"}}}
			var folds []native.ContextFold
			sw := native.ContextSwitch{
				View: rows("old-user", "request", "call", "steer", "latest"), Anchors: rows("request", "steer"), Tail: rows("latest"),
				HistoryBudgetTokens: 900, UsageTokens: 3000, AllowanceTokens: 2000,
				Fold: func(fold native.ContextFold) (int, bool) {
					folds = append(folds, fold)
					return 3000 - 500*len(folds), len(folds) == tc.fitsAt
				},
			}
			req := ChatRequest{BotID: syncCompactBotID, ChatID: syncCompactBotID, ThreadID: syncCompactThreadID, RunID: "run"}

			detail, err := service.switchContext(req, committer, "")(t.Context(), sw)
			cfg := runner.cfg
			tail := turn.ContextMessageSource{Kind: "history", ID: "latest", Current: true}
			if !reflect.DeepEqual(cfg.View, sw.View) || !reflect.DeepEqual(cfg.Anchors, rows("request", "steer", "input")) || !cfg.CandidatesFrom.IsZero() ||
				!reflect.DeepEqual(cfg.ProtectedSources, []turn.ContextMessageSource{tail}) || cfg.HistoryBudgetTokens != 900 {
				t.Fatalf("event config view=%v anchors=%v from=%v protected=%v budget=%d", cfg.View, cfg.Anchors, cfg.CandidatesFrom, cfg.ProtectedSources, cfg.HistoryBudgetTokens)
			}
			if tc.want == nil {
				if err != nil || len(folds) != 2 || folds[1].Artifact != "a2" || !reflect.DeepEqual(folds[1].Summary, compaction.SummaryMessage("second")) ||
					!reflect.DeepEqual(folds[1].Absorbed, []string{"a1"}) || detail != "trigger=in_run passes=2 request_tokens=3000->2000 allowance=2000" {
					t.Fatalf("err=%v folds=%+v detail=%q, want both passes spliced and the event ended where it fits", err, folds, detail)
				}
				return
			}
			if !errors.Is(err, tc.want) || !strings.HasSuffix(detail, "reason="+tc.reason) {
				t.Fatalf("err=%v detail=%q, want %v with reason %s", err, detail, tc.want, tc.reason)
			}
		})
	}
}
