package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/i18n"
	"github.com/felinics/memoh/internal/redact"
)

// surfaceLog is a Surface that records the calls a reply makes, in order.
type surfaceLog struct {
	caps channel.ChannelCapabilities

	mu       sync.Mutex
	calls    []string
	opens    []channel.StreamOptions
	previews []*previewLog
	reacted  []channel.ReactRequest
	attached []*channel.ReplyRef
	// errorTexts is the text of each error event the preview received.
	errorTexts []string
	// failSend makes Send return the error.
	failSend error
	// failReact makes React return the error.
	failReact error
}

func (s *surfaceLog) record(call string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call)
}

func (s *surfaceLog) Capabilities(string) channel.ChannelCapabilities { return s.caps }

func (s *surfaceLog) Send(_ context.Context, msg channel.OutboundMessage) error {
	s.record("send:" + msg.Message.Text)
	return s.failSend
}

func (s *surfaceLog) Attach(_ context.Context, _ string, att channel.Attachment, reply *channel.ReplyRef) error {
	s.mu.Lock()
	s.attached = append(s.attached, reply)
	s.mu.Unlock()
	s.record("attach:" + string(att.Type))
	return nil
}

func (s *surfaceLog) React(_ context.Context, req channel.ReactRequest) error {
	s.mu.Lock()
	s.reacted = append(s.reacted, req)
	s.mu.Unlock()
	s.record("react:" + req.Emoji)
	return s.failReact
}

func (s *surfaceLog) OpenPreview(_ context.Context, _ string, opts channel.StreamOptions) (channel.Preview, error) {
	s.mu.Lock()
	s.opens = append(s.opens, opts)
	preview := &previewLog{s: s}
	s.previews = append(s.previews, preview)
	s.mu.Unlock()
	s.record("open")
	return preview, nil
}

func (s *surfaceLog) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.calls...)
}

type previewLog struct{ s *surfaceLog }

func (p *previewLog) Update(_ context.Context, event channel.StreamEvent) error {
	entry := "update:" + string(event.Type)
	if event.Type == channel.StreamEventStatus {
		entry += ":" + string(event.Status)
	}
	if event.Type == channel.StreamEventDelta {
		entry += ":" + event.Delta
	}
	if event.Type == channel.StreamEventError {
		p.s.mu.Lock()
		p.s.errorTexts = append(p.s.errorTexts, event.Error)
		p.s.mu.Unlock()
	}
	p.s.record(entry)
	return nil
}

func (p *previewLog) Finish(_ context.Context, final *channel.StreamEvent) error {
	switch {
	case final == nil:
		p.s.record("finish:none")
	case final.Final.Message.IsEmpty():
		p.s.record("finish:flush")
	default:
		p.s.record("finish:" + final.Final.Message.Text)
	}
	return nil
}

func (p *previewLog) Abort(context.Context) error {
	p.s.record("abort")
	return nil
}

type observed struct {
	mu     sync.Mutex
	events []string
}

func (o *observed) observe(_ context.Context, event channel.StreamEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	entry := string(event.Type)
	switch event.Type {
	case channel.StreamEventStatus:
		entry += ":" + string(event.Status)
	case channel.StreamEventDelta:
		entry += ":" + event.Delta
	case channel.StreamEventFinal:
		entry += ":" + event.Final.Message.Text
	case channel.StreamEventError:
		entry += ":" + event.ErrorCode
	}
	o.events = append(o.events, entry)
}

func payload(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func deltaPayload(text string) string {
	return payload(map[string]any{"type": "text_delta", "delta": text})
}

func endPayload(texts ...string) string {
	messages := make([]turn.ModelMessage, 0, len(texts))
	for _, text := range texts {
		messages = append(messages, turn.ModelMessage{Role: "assistant", Content: turn.NewTextContent(text)})
	}
	return payload(map[string]any{"type": "agent_end", "messages": messages})
}

const approvalPayload = `{"type":"tool_approval_request","toolName":"exec","toolCallId":"call-1","approvalId":"ap-1","shortId":3,"input":{"command":"ls"}}`

type runResult struct {
	err error
}

type runSetup struct {
	caps      channel.ChannelCapabilities
	payloads  []string
	runErr    error
	terminal  bool
	hide      bool
	now       time.Time
	received  time.Time
	failSend  error
	failReact error
	outputs   func([]turn.ModelMessage) ([]channel.Message, bool)
	// failure renders the reply for a run error; "failed" when nil.
	failure func(error) (channel.StreamEvent, bool)
}

// run drives a reply on a recording surface with the scripted events.
func run(t *testing.T, setup runSetup) (*surfaceLog, *observed, runResult) {
	t.Helper()
	surface := &surfaceLog{caps: setup.caps, failSend: setup.failSend, failReact: setup.failReact}
	obs := &observed{}
	now := setup.now
	if now.IsZero() {
		now = t0
	}
	r, err := Open(context.Background(), Config{
		Surface: surface, Target: "chat-1", Reply: reply, SourceMessageID: "m-1",
		Metadata: map[string]any{"route_id": "route-1"}, ReceivedAt: setup.received,
		HideToolCalls: setup.hide, Observe: obs.observe, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	events := make(chan turn.Event, len(setup.payloads))
	for i, p := range setup.payloads {
		kind := ""
		var env struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(p), &env)
		kind = env.Type
		events <- turn.Event{Seq: int64(i + 1), Kind: kind, Payload: json.RawMessage(p)}
	}
	close(events)
	errs := make(chan error, 1)
	if setup.runErr != nil {
		errs <- setup.runErr
	}
	close(errs)
	outputs := setup.outputs
	if outputs == nil {
		outputs = func(messages []turn.ModelMessage) ([]channel.Message, bool) {
			var out []channel.Message
			for _, output := range turn.ExtractAssistantOutputs(messages) {
				out = append(out, channel.Message{Text: output.Content})
			}
			return out, false
		}
	}
	failure := setup.failure
	if failure == nil {
		failure = func(error) (channel.StreamEvent, bool) {
			return channel.StreamEvent{Type: channel.StreamEventError, Error: "failed", ErrorCode: "runtime_run_failed"}, true
		}
	}
	runErr := r.Run(context.Background(), RunInput{
		ReportsTerminal: setup.terminal,
		Localizer:       i18n.New("en"),
		Outputs:         outputs,
		StreamFailure:   failure,
		EventsEnded:     func() {},
	}, events, errs)
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	return surface, obs, runResult{err: runErr}
}

func equalCalls(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

var streaming = channel.ChannelCapabilities{Text: true, Markdown: true, Streaming: true, Attachments: true, Reactions: true, Preview: channel.PreviewEditInPlace}

func TestReplyEndsWithItsFirstOutputAndSendsTheRest(t *testing.T) {
	surface, obs, res := run(t, runSetup{caps: streaming, payloads: []string{deltaPayload("hi"), endPayload("first", "second")}})
	if res.err != nil {
		t.Fatal(res.err)
	}
	if got := surface.snapshot(); !equalCalls(got,
		"open", "update:status:started", "update:delta:hi", "update:agent_end", "finish:first", "send:second") {
		t.Fatalf("calls = %v", got)
	}
	// Observers see the reply as a conversation: each output is a final, and
	// the completed status comes last.
	want := []string{"status:started", "delta:hi", "agent_end", "final:first", "final:second", "status:completed"}
	if !equalCalls(obs.events, want...) {
		t.Fatalf("observed = %v, want %v", obs.events, want)
	}
}

// A request for approval the channel cannot render: the preview that shows
// text ends, the request is a message, and a new preview carries what follows.
func TestReplySplitsThePreviewAroundAPlainPrompt(t *testing.T) {
	surface, obs, res := run(t, runSetup{caps: streaming, payloads: []string{
		deltaPayload("a "), approvalPayload, deltaPayload("b"), endPayload("a b"),
	}})
	if res.err != nil {
		t.Fatal(res.err)
	}
	got := surface.snapshot()
	prompt := got[4]
	if !strings.HasPrefix(prompt, "send:") || !strings.Contains(prompt, "/approve 3") {
		t.Fatalf("calls = %v", got)
	}
	want := []string{
		"open", "update:status:started", "update:delta:a ", "finish:flush", prompt,
		"open", "update:status:started", "update:delta:b", "update:agent_end", "finish:a b",
	}
	if !equalCalls(got, want...) {
		t.Fatalf("calls = %v\nwant    %v", got, want)
	}
	if len(surface.opens) != 2 || surface.opens[0].Continuation || surface.opens[0].Reply != reply {
		t.Fatalf("first preview opened with %+v", surface.opens)
	}
	if surface.opens[0].After != nil || surface.opens[1].After != channel.Preview(surface.previews[0]) {
		t.Fatalf("continuing preview carries on from %v, want the one that ended", surface.opens[1].After)
	}
	if next := surface.opens[1]; !next.Continuation || next.Reply != nil || next.SourceMessageID != "m-1" || next.Metadata["route_id"] != "route-1" {
		t.Fatalf("continuing preview opened with %+v", next)
	}
	// Observers saw the approval request itself, not the message that stands
	// in for it, and the split is not an end of the reply.
	if !equalCalls(obs.events, "status:started", "delta:a ", "tool_call_start", "delta:b", "agent_end", "final:a b", "status:completed") {
		t.Fatalf("observed = %v", obs.events)
	}
}

func TestReplyForwardsNativePromptToThePreview(t *testing.T) {
	caps := streaming
	caps.NativeApproval = true
	surface, _, res := run(t, runSetup{caps: caps, payloads: []string{deltaPayload("a "), approvalPayload, endPayload("a")}})
	if res.err != nil {
		t.Fatal(res.err)
	}
	want := []string{"open", "update:status:started", "update:delta:a ", "update:tool_call_start", "update:agent_end", "finish:a"}
	if got := surface.snapshot(); !equalCalls(got, want...) {
		t.Fatalf("calls = %v", got)
	}
}

func TestReplyErrors(t *testing.T) {
	t.Run("an error after text ends the preview with that text, then is a message", func(t *testing.T) {
		surface, obs, res := run(t, runSetup{caps: streaming, payloads: []string{deltaPayload("partial")}, runErr: errors.New("boom")})
		if res.err == nil {
			t.Fatal("run error was dropped")
		}
		want := []string{"open", "update:status:started", "update:delta:partial", "abort", "send:failed"}
		if got := surface.snapshot(); !equalCalls(got, want...) {
			t.Fatalf("calls = %v", got)
		}
		if !equalCalls(obs.events, "status:started", "delta:partial", "error:runtime_run_failed") {
			t.Fatalf("observed = %v", obs.events)
		}
	})
	t.Run("an error before any text is the preview's terminal state", func(t *testing.T) {
		surface, _, res := run(t, runSetup{caps: streaming, payloads: []string{`{"type":"agent_start"}`}, runErr: errors.New("boom")})
		if res.err == nil {
			t.Fatal("run error was dropped")
		}
		want := []string{"open", "update:status:started", "update:agent_start", "update:error", "abort"}
		if got := surface.snapshot(); !equalCalls(got, want...) {
			t.Fatalf("calls = %v", got)
		}
	})
	t.Run("an error event mid-run does not touch the preview text", func(t *testing.T) {
		surface, _, res := run(t, runSetup{caps: streaming, payloads: []string{
			deltaPayload("partial"), `{"type":"error","code":"agent.provider_overloaded"}`, endPayload("done"),
		}})
		if res.err != nil {
			t.Fatal(res.err)
		}
		got := surface.snapshot()
		if len(got) != 6 || !strings.HasPrefix(got[3], "send:") || got[5] != "finish:done" {
			t.Fatalf("calls = %v", got)
		}
	})
	t.Run("a terminal event replaces the held errors with its own code", func(t *testing.T) {
		surface, obs, res := run(t, runSetup{caps: streaming, terminal: true, payloads: []string{
			`{"type":"error","code":"agent.provider_rate_limited"}`,
			`{"type":"error","code":"agent.provider_overloaded"}`,
			`{"type":"run_terminal","state":"failed","error_code":"agent.provider_overloaded"}`,
		}})
		if res.err != nil {
			t.Fatal(res.err)
		}
		want := []string{"open", "update:status:started", "update:error", "finish:none"}
		if got := surface.snapshot(); !equalCalls(got, want...) {
			t.Fatalf("calls = %v", got)
		}
		if !equalCalls(obs.events, "status:started", "error:agent.provider_overloaded", "status:completed") {
			t.Fatalf("observed = %v", obs.events)
		}
	})
	t.Run("a recovered error is not shown when the run completed", func(t *testing.T) {
		surface, _, res := run(t, runSetup{caps: streaming, terminal: true, payloads: []string{
			`{"type":"error","code":"agent.provider_overloaded"}`, endPayload("answer"), `{"type":"run_terminal","state":"completed"}`,
		}})
		if res.err != nil {
			t.Fatal(res.err)
		}
		want := []string{"open", "update:status:started", "update:agent_end", "finish:answer"}
		if got := surface.snapshot(); !equalCalls(got, want...) {
			t.Fatalf("calls = %v", got)
		}
	})
	t.Run("a start failure is shown on the preview", func(t *testing.T) {
		surface := &surfaceLog{caps: streaming}
		r, err := Open(context.Background(), Config{Surface: surface, Target: "chat-1", Reply: reply, Now: func() time.Time { return t0 }})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		r.Fail(context.Background(), channel.StreamEvent{Type: channel.StreamEventError, Error: "boom", ErrorCode: "internal"})
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := surface.snapshot(); !equalCalls(got, "open", "update:status:started", "update:error", "abort") {
			t.Fatalf("calls = %v", got)
		}
	})
}

func TestReplySuppressedRunFlushesText(t *testing.T) {
	outputs := func([]turn.ModelMessage) ([]channel.Message, bool) { return nil, true }
	surface, _, res := run(t, runSetup{caps: streaming, outputs: outputs, payloads: []string{deltaPayload("sending "), endPayload("x")}})
	if res.err != nil {
		t.Fatal(res.err)
	}
	if got := surface.snapshot(); !equalCalls(got, "open", "update:status:started", "update:delta:sending ", "update:agent_end", "finish:flush") {
		t.Fatalf("calls = %v", got)
	}
	surface, _, _ = run(t, runSetup{caps: streaming, outputs: outputs, payloads: []string{endPayload("x")}})
	if got := surface.snapshot(); !equalCalls(got, "open", "update:status:started", "update:agent_end", "finish:none") {
		t.Fatalf("without text: calls = %v", got)
	}
}

func TestReplyReactions(t *testing.T) {
	react := `{"type":"reaction_delta","reactions":[{"emoji":"👍"}]}`
	surface, _, res := run(t, runSetup{caps: streaming, payloads: []string{react, endPayload("ok")}})
	if res.err != nil {
		t.Fatal(res.err)
	}
	if len(surface.reacted) != 1 || surface.reacted[0].Target != "chat-1" || surface.reacted[0].MessageID != "m-1" || surface.reacted[0].Emoji != "👍" {
		t.Fatalf("reactions = %+v", surface.reacted)
	}
	// The platform refusing a reaction does not fail the reply.
	_, _, res = run(t, runSetup{caps: streaming, failReact: errors.New("no"), payloads: []string{react, endPayload("ok")}})
	if res.err != nil {
		t.Fatalf("reply failed with %v", res.err)
	}
}

func TestReplyHiddenToolEventsReachObserversOnly(t *testing.T) {
	payloads := []string{
		`{"type":"tool_call_start","toolName":"search","toolCallId":"c"}`,
		`{"type":"tool_call_end","toolName":"search","toolCallId":"c"}`,
		endPayload("found"),
	}
	surface, obs, res := run(t, runSetup{caps: streaming, hide: true, payloads: payloads})
	if res.err != nil {
		t.Fatal(res.err)
	}
	if got := surface.snapshot(); !equalCalls(got, "open", "update:status:started", "update:agent_end", "finish:found") {
		t.Fatalf("calls = %v", got)
	}
	if !equalCalls(obs.events, "status:started", "tool_call_start", "tool_call_end", "agent_end", "final:found", "status:completed") {
		t.Fatalf("observed = %v", obs.events)
	}
}

// A tool call the channel hides is still a boundary: the text streamed before
// it ends as a message of its own, and what follows opens a new preview, so
// the run's final reply does not overwrite it.
func TestReplyKeepsTheTextBeforeAHiddenToolCall(t *testing.T) {
	toolStart := `{"type":"tool_call_start","toolName":"search","toolCallId":"c"}`

	t.Run("the text before the call ends and a new preview carries the rest", func(t *testing.T) {
		surface, obs, res := run(t, runSetup{caps: streaming, hide: true, payloads: []string{
			deltaPayload("Let me check."), toolStart, deltaPayload("Here is the answer."),
			endPayload("Let me check.", "Here is the answer."),
		}})
		if res.err != nil {
			t.Fatal(res.err)
		}
		want := []string{
			"open", "update:status:started", "update:delta:Let me check.", "finish:flush",
			"open", "update:status:started", "update:delta:Here is the answer.", "update:agent_end",
			"finish:Here is the answer.",
		}
		if got := surface.snapshot(); !equalCalls(got, want...) {
			t.Fatalf("calls = %v\nwant    %v", got, want)
		}
		if len(surface.opens) != 2 || !surface.opens[1].Continuation {
			t.Fatalf("previews opened with %+v, want the second to continue the reply", surface.opens)
		}
		// The tool call reaches observers, and the repeat of the first segment
		// is observed but not delivered a second time.
		want = []string{"status:started", "delta:Let me check.", "tool_call_start", "delta:Here is the answer.", "agent_end", "final:Let me check.", "final:Here is the answer.", "status:completed"}
		if !equalCalls(obs.events, want...) {
			t.Fatalf("observed = %v\nwant     %v", obs.events, want)
		}
	})
	t.Run("a run that only repeats does not deliver the text twice", func(t *testing.T) {
		surface, _, res := run(t, runSetup{caps: streaming, hide: true, payloads: []string{
			deltaPayload("Let me check."), toolStart, endPayload("Let me check."),
		}})
		if res.err != nil {
			t.Fatal(res.err)
		}
		// The new preview shows no text and ends as a placeholder, while the
		// text that was ended before the call stands as its own message.
		want := []string{"open", "update:status:started", "update:delta:Let me check.", "finish:flush", "open", "update:status:started", "update:agent_end", "finish:none"}
		if got := surface.snapshot(); !equalCalls(got, want...) {
			t.Fatalf("calls = %v\nwant    %v", got, want)
		}
	})
	t.Run("text before the call is not ended when the channel shows no preview", func(t *testing.T) {
		caps := streaming
		caps.Preview = channel.PreviewNone
		surface, _, res := run(t, runSetup{caps: caps, hide: true, payloads: []string{
			deltaPayload("Let me check."), toolStart, endPayload("Let me check."),
		}})
		if res.err != nil {
			t.Fatal(res.err)
		}
		if got := surface.snapshot(); !equalCalls(got, "send:Let me check.") {
			t.Fatalf("calls = %v", got)
		}
	})
}

// A channel with a preview gets attachments as messages of their own, one
// each, in the order they came.
func TestReplyAttachmentsOnAPreview(t *testing.T) {
	caps := streaming
	attach := `{"type":"attachment_delta","attachments":[{"type":"image","url":"https://example.test/a.png"},{"type":"file","url":"https://example.test/b.txt"}]}`
	surface, _, res := run(t, runSetup{caps: caps, payloads: []string{deltaPayload("see "), attach, endPayload("see")}})
	if res.err != nil {
		t.Fatal(res.err)
	}
	want := []string{"open", "update:status:started", "update:delta:see ", "attach:image", "attach:file", "update:agent_end", "finish:see"}
	if got := surface.snapshot(); !equalCalls(got, want...) {
		t.Fatalf("calls = %v", got)
	}
}

// A channel with no preview opens none; the reply is its messages.
func TestReplyWithoutPreview(t *testing.T) {
	caps := streaming
	caps.Preview = channel.PreviewNone
	surface, obs, res := run(t, runSetup{caps: caps, payloads: []string{deltaPayload("hi"), approvalPayload, endPayload("first", "second")}})
	if res.err != nil {
		t.Fatal(res.err)
	}
	got := surface.snapshot()
	if len(got) != 3 || !strings.Contains(got[0], "/approve 3") || got[1] != "send:first" || got[2] != "send:second" {
		t.Fatalf("calls = %v", got)
	}
	if !equalCalls(obs.events, "status:started", "delta:hi", "tool_call_start", "agent_end", "final:first", "final:second", "status:completed") {
		t.Fatalf("observed = %v", obs.events)
	}

	failing := &surfaceLog{caps: caps}
	r, err := Open(context.Background(), Config{Surface: failing, Target: "chat-1", Reply: reply, Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	r.Fail(context.Background(), channel.StreamEvent{Type: channel.StreamEventError, Error: "boom", ErrorCode: "internal"})
	if got := failing.snapshot(); !equalCalls(got, "send:boom") {
		t.Fatalf("a start failure on a channel without preview: calls = %v", got)
	}
}

// The run fails with the window's code once the window closed, and the
// preview ends.
func TestReplyWindowExpired(t *testing.T) {
	caps := streaming
	caps.ReplyWindow = time.Hour

	// The window closes while the preview is open: the next event ends the run.
	clock := &steppedClock{times: []time.Time{t0, t0.Add(2 * time.Hour)}}
	surface := &surfaceLog{caps: caps}
	r, err := Open(context.Background(), Config{Surface: surface, Target: "chat-1", Reply: reply, ReceivedAt: t0, Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	err = r.Show(context.Background(), channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "late", Phase: channel.StreamPhaseText})
	if got := apperror.CodeOf(err); got != apperror.CodeChannelReplyWindowExpired {
		t.Fatalf("run error = %v (code %q)", err, got)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := surface.snapshot(); !equalCalls(got, "open", "abort") {
		t.Fatalf("calls = %v", got)
	}

	caps.ProactiveSend = true
	surface, _, res := run(t, runSetup{
		caps: caps, received: t0, now: t0.Add(2 * time.Hour),
		payloads: []string{deltaPayload("late"), endPayload("late")},
	})
	if res.err != nil {
		t.Fatalf("proactive send: %v", res.err)
	}
	if got := surface.snapshot(); got[len(got)-1] != "finish:late" {
		t.Fatalf("calls = %v", got)
	}
}

// steppedClock returns its times in turn, then the last one.
type steppedClock struct {
	mu    sync.Mutex
	times []time.Time
}

func (c *steppedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	at := c.times[0]
	if len(c.times) > 1 {
		c.times = c.times[1:]
	}
	return at
}

// The preview quotes the source message by the window's rule when it opens: as
// is inside the window, without a reference after it when the platform accepts
// a proactive message, and not at all when it does not.
func TestReplyOpensThePreviewByTheWindowRule(t *testing.T) {
	open := func(caps channel.ChannelCapabilities, now time.Time) (*surfaceLog, error) {
		surface := &surfaceLog{caps: caps}
		_, err := Open(context.Background(), Config{
			Surface: surface, Target: "chat-1", Reply: reply, SourceMessageID: "m-1",
			ReceivedAt: t0, Now: func() time.Time { return now },
		})
		return surface, err
	}
	caps := streaming
	caps.ReplyWindow = time.Hour

	surface, err := open(caps, t0.Add(time.Minute))
	if err != nil || len(surface.opens) != 1 || surface.opens[0].Reply != reply {
		t.Fatalf("inside the window: opens = %+v, err = %v", surface.opens, err)
	}
	proactive := caps
	proactive.ProactiveSend = true
	surface, err = open(proactive, t0.Add(2*time.Hour))
	if err != nil || len(surface.opens) != 1 || surface.opens[0].Reply != nil || surface.opens[0].SourceMessageID != "m-1" {
		t.Fatalf("expired, proactive send: opens = %+v, err = %v", surface.opens, err)
	}
	surface, err = open(caps, t0.Add(2*time.Hour))
	if got := apperror.CodeOf(err); got != apperror.CodeChannelReplyWindowExpired {
		t.Fatalf("expired, no proactive send: err = %v (code %q)", err, got)
	}
	if len(surface.opens) != 0 {
		t.Fatalf("a preview was opened after the window closed: %+v", surface.opens)
	}
}

func TestReplySendFailureStopsTheRun(t *testing.T) {
	surface, _, res := run(t, runSetup{caps: streaming, failSend: errors.New("platform said no"), payloads: []string{
		deltaPayload("a "), approvalPayload, deltaPayload("b"), endPayload("a b"),
	}})
	if res.err == nil || !strings.Contains(res.err.Error(), "platform said no") {
		t.Fatalf("run error = %v", res.err)
	}
	for _, call := range surface.snapshot() {
		if strings.HasPrefix(call, "update:delta:b") {
			t.Fatalf("the run went on after the write failed: %v", surface.snapshot())
		}
	}
}

// Closing a reply that ended leaves its preview alone.
func TestReplyCloseIsIdempotent(t *testing.T) {
	surface, _, res := run(t, runSetup{caps: streaming, payloads: []string{endPayload("done")}})
	if res.err != nil {
		t.Fatal(res.err)
	}
	for _, call := range surface.snapshot() {
		if call == "abort" {
			t.Fatalf("a finished preview was aborted: %v", surface.snapshot())
		}
	}
}

// User-input prompts carry the locale of the reply.
func TestReplyUserInputPromptCarriesLocale(t *testing.T) {
	surface := &surfaceLog{caps: streaming}
	caps := streaming
	caps.NativeUserInput = true
	surface.caps = caps
	r, err := Open(context.Background(), Config{Surface: surface, Target: "chat-1", Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan turn.Event, 1)
	events <- turn.Event{Payload: json.RawMessage(`{"type":"user_input_request","toolName":"ask_user","toolCallId":"c","userInputId":"ui","input":{}}`)}
	close(events)
	errs := make(chan error)
	close(errs)
	var got *channel.StreamToolCall
	obs := func(_ context.Context, event channel.StreamEvent) {
		if event.ToolCall != nil {
			got = event.ToolCall
		}
	}
	r.cfg.Observe = obs
	err = r.Run(context.Background(), RunInput{
		Localizer:   i18n.New("zh"),
		Outputs:     func([]turn.ModelMessage) ([]channel.Message, bool) { return nil, false },
		EventsEnded: func() {},
	}, events, errs)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Locale != "zh" {
		t.Fatalf("tool call = %+v, want locale zh", got)
	}
}

// What a channel without a preview shows of a reply is its messages, in the
// order the events came: the text before an attachment, the attachment, then
// the rest.
func TestReplyWithoutPreviewKeepsTheOrderOfAttachments(t *testing.T) {
	caps := streaming
	caps.Preview = channel.PreviewNone
	attach := `{"type":"attachment_delta","attachments":[{"type":"image","url":"https://example.test/a.png"}]}`

	t.Run("text, attachment, text", func(t *testing.T) {
		surface, obs, res := run(t, runSetup{caps: caps, payloads: []string{
			deltaPayload("see "), attach, deltaPayload("below"), endPayload("see below"),
		}})
		if res.err != nil {
			t.Fatal(res.err)
		}
		if got := surface.snapshot(); !equalCalls(got, "send:see", "attach:image", "send:below") {
			t.Fatalf("calls = %v", got)
		}
		if !equalCalls(obs.events, "status:started", "delta:see ", "attachment", "delta:below", "agent_end", "final:see below", "status:completed") {
			t.Fatalf("observed = %v", obs.events)
		}
	})
	t.Run("text before the attachment is the whole output", func(t *testing.T) {
		surface, obs, res := run(t, runSetup{caps: caps, payloads: []string{deltaPayload("here"), attach, endPayload("here")}})
		if res.err != nil {
			t.Fatal(res.err)
		}
		if got := surface.snapshot(); !equalCalls(got, "send:here", "attach:image") {
			t.Fatalf("calls = %v", got)
		}
		if !equalCalls(obs.events, "status:started", "delta:here", "attachment", "agent_end", "final:here", "status:completed") {
			t.Fatalf("observed = %v", obs.events)
		}
	})
	t.Run("text before a tool call is not a reply", func(t *testing.T) {
		surface, _, res := run(t, runSetup{caps: caps, hide: true, payloads: []string{
			deltaPayload("let me draw"),
			`{"type":"tool_call_start","toolName":"generate_image","toolCallId":"c"}`,
			`{"type":"tool_call_end","toolName":"generate_image","toolCallId":"c"}`,
			attach, deltaPayload("done"), endPayload("done"),
		}})
		if res.err != nil {
			t.Fatal(res.err)
		}
		if got := surface.snapshot(); !equalCalls(got, "attach:image", "send:done") {
			t.Fatalf("calls = %v", got)
		}
	})
	t.Run("a retry discards what the first attempt held", func(t *testing.T) {
		surface, _, res := run(t, runSetup{caps: caps, payloads: []string{
			deltaPayload("first try"), `{"type":"retry"}`, attach, endPayload("second try"),
		}})
		if res.err != nil {
			t.Fatal(res.err)
		}
		if got := surface.snapshot(); !equalCalls(got, "attach:image", "send:second try") {
			t.Fatalf("calls = %v", got)
		}
	})
}

// A channel without a preview gets the state of each finished tool call as a
// message when the bot shows tool calls, and nothing when it hides them.
func TestReplyWithoutPreviewSendsToolResults(t *testing.T) {
	caps := streaming
	caps.Preview = channel.PreviewNone
	payloads := []string{
		`{"type":"tool_call_start","toolName":"web_search","toolCallId":"c","input":{"query":"memoh"}}`,
		`{"type":"tool_call_end","toolName":"web_search","toolCallId":"c","input":{"query":"memoh"},"result":{"ok":true}}`,
		endPayload("found"),
	}

	surface, obs, res := run(t, runSetup{caps: caps, payloads: payloads})
	if res.err != nil {
		t.Fatal(res.err)
	}
	got := surface.snapshot()
	if len(got) != 2 || !strings.HasPrefix(got[0], "send:") || !strings.Contains(got[0], "web_search · completed") || got[1] != "send:found" {
		t.Fatalf("calls = %v", got)
	}
	if !equalCalls(obs.events, "status:started", "tool_call_start", "tool_call_end", "agent_end", "final:found", "status:completed") {
		t.Fatalf("observed = %v", obs.events)
	}

	surface, _, res = run(t, runSetup{caps: caps, hide: true, payloads: payloads})
	if res.err != nil {
		t.Fatal(res.err)
	}
	if got := surface.snapshot(); !equalCalls(got, "send:found") {
		t.Fatalf("hidden: calls = %v", got)
	}
}

// Attachments are messages of their own that quote the source message by the
// same rule as every other message of the reply.
func TestReplyAttachmentsQuoteByTheWindowRule(t *testing.T) {
	caps := streaming
	caps.Preview = channel.PreviewEditInPlace
	caps.ReplyWindow = time.Hour
	attach := `{"type":"attachment_delta","attachments":[{"type":"image","url":"https://example.test/a.png"}]}`

	surface, _, res := run(t, runSetup{caps: caps, received: t0, now: t0.Add(time.Minute), payloads: []string{attach, endPayload("ok")}})
	if res.err != nil {
		t.Fatal(res.err)
	}
	if len(surface.attached) != 1 || surface.attached[0] != reply {
		t.Fatalf("inside the window: attachments quote %+v, want the source message", surface.attached)
	}

	caps.ProactiveSend = true
	surface, _, res = run(t, runSetup{caps: caps, received: t0, now: t0.Add(2 * time.Hour), payloads: []string{attach, endPayload("ok")}})
	if res.err != nil {
		t.Fatal(res.err)
	}
	if len(surface.attached) != 1 || surface.attached[0] != nil {
		t.Fatalf("after the window: attachments quote %+v, want no reference", surface.attached)
	}
}

// Error text is masked before it leaves the reply, whichever way it goes out:
// through the preview, as a message after preview text, or as a message on a
// channel without a preview.
func TestReplyMasksSecretsInErrorText(t *testing.T) {
	const secret = "sk-delivery-0123456789abcdef"
	redact.SetSecrets("delivery-reply-test", secret)
	t.Cleanup(func() { redact.SetSecrets("delivery-reply-test") })
	failure := channel.StreamEvent{Type: channel.StreamEventError, Error: "upstream rejected key " + secret + " today"}
	delta := channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "partial", Phase: channel.StreamPhaseText}
	masked := "upstream rejected key " + strings.Repeat("*", len(secret)) + " today"

	show := func(t *testing.T, caps channel.ChannelCapabilities, events ...channel.StreamEvent) (*surfaceLog, *observed) {
		t.Helper()
		surface := &surfaceLog{caps: caps}
		obs := &observed{}
		r, err := Open(context.Background(), Config{Surface: surface, Target: "chat-1", Reply: reply, Observe: obs.observe, Now: func() time.Time { return t0 }})
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range events {
			if err := r.Show(context.Background(), ev); err != nil {
				t.Fatal(err)
			}
		}
		return surface, obs
	}

	t.Run("through the preview", func(t *testing.T) {
		surface, _ := show(t, streaming, failure)
		if len(surface.errorTexts) != 1 || surface.errorTexts[0] != masked {
			t.Fatalf("the preview received %q, want %q", surface.errorTexts, masked)
		}
	})
	t.Run("as a message after preview text", func(t *testing.T) {
		surface, _ := show(t, streaming, delta, failure)
		if got := surface.snapshot(); !equalCalls(got, "open", "update:delta:partial", "send:Error: "+masked) {
			t.Fatalf("calls = %v", got)
		}
	})
	t.Run("as a message on a channel without a preview", func(t *testing.T) {
		caps := streaming
		caps.Preview = channel.PreviewNone
		surface, _ := show(t, caps, failure)
		if got := surface.snapshot(); !equalCalls(got, "send:Error: "+masked) {
			t.Fatalf("calls = %v", got)
		}
	})
	t.Run("when the run fails", func(t *testing.T) {
		surface, _, res := run(t, runSetup{
			caps: streaming, runErr: errors.New("boom"), payloads: []string{deltaPayload("partial")},
			failure: func(error) (channel.StreamEvent, bool) { return failure, true },
		})
		if res.err == nil {
			t.Fatal("run error was dropped")
		}
		if got := surface.snapshot(); got[len(got)-1] != "send:Error: "+masked {
			t.Fatalf("calls = %v", got)
		}
	})
}
