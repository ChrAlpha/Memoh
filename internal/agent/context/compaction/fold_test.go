package compaction

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

func (f *fakeQueries) ListHistoryRowsBySources(_ context.Context, arg sqlc.ListHistoryRowsBySourcesParams) ([]sqlc.ListHistoryRowsBySourcesRow, error) {
	var rows []sqlc.ListHistoryRowsBySourcesRow
	for _, row := range f.ordered() {
		if slices.Contains(arg.Ids, row.ID) || row.ExternalMessageID.Valid && slices.Contains(arg.ExternalMessageIds, row.ExternalMessageID.String) {
			rows = append(rows, sqlc.ListHistoryRowsBySourcesRow{ID: row.ID, SourceMessageID: row.ExternalMessageID, TurnPosition: row.TurnPosition, TurnMessageSeq: row.TurnMessageSeq, Role: row.Role, Content: row.Content, EventID: row.EventID})
		}
	}
	return rows, nil
}

// scriptedSummarizer answers fork requests in turn, repeating its last
// answer, and records each request.
type scriptedSummarizer struct {
	answers  []sdk.ModelResult
	requests []sdk.Request
}

func (s *scriptedSummarizer) summarize(_ context.Context, req sdk.Request) (sdk.ModelResult, error) {
	s.requests = append(s.requests, req)
	answer := s.answers[min(len(s.requests), len(s.answers))-1]
	return answer, nil
}

func summaryAnswer(text string) sdk.ModelResult {
	return sdk.ModelResult{Text: text, FinishReason: sdk.FinishReasonStop}
}

// foldSession is a session whose history rows the requests below render.
type foldSession struct {
	t       *testing.T
	q       *fakeQueries
	svc     *Service
	botID   string
	session string
}

func newFoldSession(t *testing.T) *foldSession {
	t.Helper()
	q := &fakeQueries{}
	return &foldSession{t: t, q: q, svc: newMachineryService(q), botID: uuid.NewString(), session: uuid.NewString()}
}

// row adds a context row to the session and returns the source naming it.
func (s *foldSession) row() turn.ContextMessageSource {
	id := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	s.q.history = append(s.q.history, candidateRow{ID: id, Role: "user", Content: []byte(`"row"`)})
	return turn.ContextMessageSource{Kind: "history", ID: id.String()}
}

func callMessage(id string) sdk.Message {
	return sdk.Message{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{sdk.ToolCallPart{ToolCallID: id, ToolName: "read"}}}
}

func resultMessage(id, output string) sdk.Message {
	return sdk.ToolMessage(sdk.ToolResultPart{ToolCallID: id, ToolName: "read", Result: toolexec.OutputFromValue(output)})
}

func historySource(sources ...turn.ContextMessageSource) Source {
	return Source{Sources: sources}
}

// request is one run's next request: the checkpoint's summary, three rows
// of earlier work, the run's input, and the latest step as the tail.
type request struct {
	messages []sdk.Message
	sources  []Source
	cut      int
	rows     []turn.ContextMessageSource
}

func (s *foldSession) request() request {
	held := s.q.hold("Earlier work summarized.")
	summary := turn.ContextMessageSource{Kind: "summary", ID: summaryIDs(held)[0]}
	rows := []turn.ContextMessageSource{s.row(), s.row(), s.row(), s.row(), s.row(), s.row()}
	return request{
		messages: []sdk.Message{
			SummaryMessage("Earlier work summarized."),
			sdk.UserMessage("Please audit the parser module and list every unsafe cast you find in it."),
			callMessage("call_1"),
			resultMessage("call_1", strings.Repeat("parser.go:12 unsafe cast of int64 to int32\n", 40)),
			sdk.UserMessage("RUN_INPUT: fix the casts the audit found"),
			callMessage("call_2"),
			resultMessage("call_2", "patched parser.go"),
		},
		sources: []Source{
			historySource(summary),
			historySource(rows[0]),
			historySource(rows[1]),
			historySource(rows[2]),
			{Sources: []turn.ContextMessageSource{rows[3]}, Anchor: true},
			historySource(rows[4]),
			historySource(rows[5]),
		},
		cut:  5,
		rows: rows,
	}
}

func (s *foldSession) event(r request, summarizer *scriptedSummarizer) Event {
	req := sdk.Request{Model: "run-model", System: "You are the bot.", Messages: r.messages, Tools: []sdk.ToolDefinition{{Name: "read", Description: "read a file"}}}
	return Event{
		BotID:           s.botID,
		SessionID:       s.session,
		Request:         req,
		Sources:         r.sources,
		Cut:             r.cut,
		Window:          32000,
		Allowance:       24000,
		MaxOutputTokens: 8000,
		Summarizer:      summarizer.summarize,
		Apply: func(pass Pass) (sdk.Request, []Source, int) {
			messages, sources, cut := splice(req.Messages, r.sources, r.cut, pass)
			req.Messages, r.sources, r.cut = messages, sources, cut
			return req, sources, cut
		},
	}
}

// A fold forks the request the run would send: the prefix before the tail
// is the run's own, byte for byte, and one instruction follows it. The
// summary replaces the checkpoint's summary and the work before the run's
// input; the input stays, and the checkpoint moves past the folded rows.
func TestFoldForksTheRequestBeforeItsTail(t *testing.T) {
	t.Parallel()
	s := newFoldSession(t)
	r := s.request()
	summarizer := &scriptedSummarizer{answers: []sdk.ModelResult{summaryAnswer("The parser audit found one unsafe cast at parser.go:12.")}}

	passes, err := s.svc.Fold(context.Background(), s.event(r, summarizer))
	if err != nil || len(passes) != 1 {
		t.Fatalf("passes=%+v err=%v, want one pass", passes, err)
	}
	if len(summarizer.requests) != 1 {
		t.Fatalf("summarizer calls = %d, want 1", len(summarizer.requests))
	}
	fork := summarizer.requests[0]
	if fork.System != "You are the bot." || len(fork.Tools) != 1 || fork.Model != "run-model" || fork.MaxTokens == nil || *fork.MaxTokens > 8000 {
		t.Fatalf("fork request changed what the run sends: system=%q tools=%d model=%q max_tokens=%v", fork.System, len(fork.Tools), fork.Model, fork.MaxTokens)
	}
	if len(fork.Messages) != r.cut+1 || !slices.EqualFunc(fork.Messages[:r.cut], r.messages[:r.cut], messagesEqual) {
		t.Fatalf("fork messages are not the run's prefix plus an instruction: %d messages", len(fork.Messages))
	}
	instruction := messageText(fork.Messages[r.cut])
	for _, want := range []string{instructionMarker, "<kept>", "RUN_INPUT: fix the casts", "ref=" + r.rows[2].ID + ":0"} {
		if !strings.Contains(instruction, want) {
			t.Fatalf("instruction lacks %q:\n%s", want, instruction)
		}
	}
	pass := passes[0]
	if pass.More || !slices.Equal(pass.Absorbed, []string{r.sources[0].Sources[0].ID}) {
		t.Fatalf("pass = %+v, want the earlier summary absorbed and nothing left", pass)
	}
	for _, row := range r.rows[:3] {
		if !slices.Contains(pass.Covered, row) {
			t.Fatalf("covered = %v, want the earlier work %s covered", pass.Covered, row.ID)
		}
	}
	for _, row := range r.rows[3:] {
		if slices.Contains(pass.Covered, row) {
			t.Fatalf("covered = %v, the input and the tail must stay", pass.Covered)
		}
	}
	held := s.q.held()
	if len(held) != 1 || held[0].SummaryID != pass.ArtifactID {
		t.Fatalf("active checkpoint holds %+v, want only the new summary", held)
	}
	if position := s.q.checkpoints[s.q.active]; position.TurnPosition != s.q.ordered()[2].TurnPosition.Int64 {
		t.Fatalf("checkpoint position %d, want the last folded row %d", position.TurnPosition, s.q.ordered()[2].TurnPosition.Int64)
	}
}

// Runtime context has no rows: it stays where it is, no summary stands for
// it, and the instruction tells the summarizer to leave it out. A closure
// the read path wrote for a call goes with the call.
func TestFoldLeavesRuntimeContextAndTakesClosuresWithTheirCalls(t *testing.T) {
	t.Parallel()
	s := newFoldSession(t)
	r := s.request()
	memory := sdk.UserMessage("MEMORY_RECALL: the user prefers small patches")
	abandoned := callMessage("call_0")
	closure := resultMessage("call_0", "tool execution interrupted before a response was recorded")
	callRow := s.row()
	r.messages = slices.Insert(r.messages, 1, abandoned, closure)
	r.sources = slices.Insert(r.sources, 1, historySource(callRow), historySource(callRow))
	r.messages = slices.Insert(r.messages, 6, memory)
	r.sources = slices.Insert(r.sources, 6, Source{})
	r.cut += 3
	summarizer := &scriptedSummarizer{answers: []sdk.ModelResult{summaryAnswer("Audit done; one cast found.")}}

	passes, err := s.svc.Fold(context.Background(), s.event(r, summarizer))
	if err != nil || len(passes) != 1 {
		t.Fatalf("passes=%+v err=%v", passes, err)
	}
	instruction := messageText(summarizer.requests[0].Messages[r.cut])
	if !strings.Contains(instruction, "<runtime_context>") || !strings.Contains(instruction, "MEMORY_RECALL") {
		t.Fatalf("instruction does not name the runtime context:\n%s", instruction)
	}
	if !slices.Contains(passes[0].Covered, callRow) {
		t.Fatalf("covered = %v, want the abandoned call's row", passes[0].Covered)
	}
	messages, _, _ := splice(r.messages, r.sources, r.cut, passes[0])
	for _, message := range messages {
		text := messageText(message)
		if strings.Contains(text, "call_0") || message.Role == sdk.MessageRoleTool && strings.Contains(text, "interrupted") {
			t.Fatalf("the spliced request keeps the abandoned call or its closure: %q", text)
		}
	}
	if !slices.ContainsFunc(messages, func(m sdk.Message) bool { return strings.Contains(messageText(m), "MEMORY_RECALL") }) {
		t.Fatal("the spliced request lost the runtime context")
	}
}

// When the request before the tail does not leave the summarizer room to
// answer in its window, a pass reads less, ending at a tool-group boundary,
// and the next pass forks the spliced request again.
func TestFoldReadsLessWhenThePrefixFillsTheWindow(t *testing.T) {
	t.Parallel()
	s := newFoldSession(t)
	r := s.request()
	second := s.row()
	secondResult := s.row()
	r.messages = slices.Insert(r.messages, 4, callMessage("call_3"), resultMessage("call_3", strings.Repeat("lexer.go:40 unsafe cast of uint to int\n", 40)))
	r.sources = slices.Insert(r.sources, 4, historySource(second), historySource(secondResult))
	r.cut += 2
	summarizer := &scriptedSummarizer{answers: []sdk.ModelResult{summaryAnswer("The parser part of the audit is summarized."), summaryAnswer("The parser and lexer audits are summarized.")}}
	ev := s.event(r, summarizer)
	// The window holds one long result and an answer, not both results.
	ev.Window = forkTokens(sdk.Request{System: ev.Request.System, Messages: r.messages[:4], Tools: ev.Request.Tools}) + 1900
	ev.Allowance = 20000
	ev.MaxOutputTokens = 4000
	apply, applied := ev.Apply, 0
	ev.Apply = func(pass Pass) (sdk.Request, []Source, int) {
		applied++
		return apply(pass)
	}

	passes, err := s.svc.Fold(context.Background(), ev)
	if err != nil || len(passes) != 2 || applied != 2 {
		t.Fatalf("passes=%d applied=%d err=%v, want two passes, each spliced into the caller's request", len(passes), applied, err)
	}
	if !passes[0].More || passes[1].More {
		t.Fatalf("more = %v,%v, want the first pass to leave work for the second", passes[0].More, passes[1].More)
	}
	first, next := summarizer.requests[0], summarizer.requests[1]
	if read := len(first.Messages) - 1; read != 4 {
		t.Fatalf("the first pass read %d messages, want the 4 up to the second call", read)
	}
	if !strings.Contains(messageText(next.Messages[0]), "The parser part of the audit is summarized.") {
		t.Fatalf("the second pass did not read the first pass's summary: %q", messageText(next.Messages[0]))
	}
	if next.System != first.System || !slices.EqualFunc(next.Tools, first.Tools, func(a, b sdk.ToolDefinition) bool { return a.Name == b.Name }) || len(next.Tools) != 1 {
		t.Fatalf("the second fork sends system %q and %d tools, want the run's", next.System, len(next.Tools))
	}
	if !slices.Equal(passes[1].Absorbed, []string{passes[0].ArtifactID}) || !slices.Contains(passes[1].Covered, secondResult) {
		t.Fatalf("second pass absorbed %v and covered %v, want the first summary and the lexer audit", passes[1].Absorbed, passes[1].Covered)
	}
}

// A message larger than the window the summarizer reads in is no wall: the
// fork reads its head and tail, and the summary stands for all of it.
func TestFoldReadsAMessageLargerThanItsWindowByItsHeadAndTail(t *testing.T) {
	t.Parallel()
	s := newFoldSession(t)
	r := s.request()
	huge := "HUGE_HEAD " + strings.Repeat("payload ", 12000) + " HUGE_TAIL"
	r.messages[1] = sdk.UserMessage(huge)
	summarizer := &scriptedSummarizer{answers: []sdk.ModelResult{summaryAnswer("The audit request and its findings are summarized.")}}
	ev := s.event(r, summarizer)
	ev.Window, ev.Allowance, ev.MaxOutputTokens = 8000, 6000, 2000
	if tokens := forkTokens(sdk.Request{Messages: r.messages[1:2]}); tokens <= ev.Window {
		t.Fatalf("the message takes %d tokens, want more than the %d-token window", tokens, ev.Window)
	}

	passes, err := s.svc.Fold(context.Background(), ev)
	if err != nil || len(passes) != 1 || !slices.Contains(passes[0].Covered, r.rows[0]) {
		t.Fatalf("passes=%+v err=%v, want one pass that stands for the large message", passes, err)
	}
	fork := summarizer.requests[0]
	read := messageText(fork.Messages[1])
	if forkTokens(fork) > ev.Window || !strings.Contains(read, "HUGE_HEAD") || !strings.Contains(read, "HUGE_TAIL") || !strings.Contains(read, "[memoh pruned]") {
		t.Fatalf("fork = %d tokens, read %d bytes of the message, want its head and tail within the window", forkTokens(fork), len(read))
	}
	if messageText(fork.Messages[0]) != messageText(r.messages[0]) || len(fork.Messages) != r.cut+1 {
		t.Fatalf("the fork changed a message it can read whole")
	}
}

// A call whose arguments are larger than the window is folded the same way:
// the fork sends the head and tail of its arguments under the same call, so
// its result still pairs with it.
func TestFoldReadsACallWithArgumentsLargerThanItsWindow(t *testing.T) {
	t.Parallel()
	s := newFoldSession(t)
	r := s.request()
	content := "CONTENT_HEAD " + strings.Repeat("line of the file ", 8000) + " CONTENT_TAIL"
	r.messages[2] = sdk.Message{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{sdk.ToolCallPart{
		ToolCallID: "call_1", ToolName: "write", Input: sdk.ParseToolArguments(`{"path":"big.txt","content":` + strconv.Quote(content) + `}`),
	}}}
	summarizer := &scriptedSummarizer{answers: []sdk.ModelResult{summaryAnswer("A large file was written.")}}
	ev := s.event(r, summarizer)
	ev.Window, ev.Allowance, ev.MaxOutputTokens = 8000, 6000, 2000

	passes, err := s.svc.Fold(context.Background(), ev)
	if err != nil || len(passes) != 1 || !slices.Contains(passes[0].Covered, r.rows[1]) {
		t.Fatalf("passes=%+v err=%v, want one pass that stands for the large call", passes, err)
	}
	fork := summarizer.requests[0]
	call, ok := fork.Messages[2].Content[0].(sdk.ToolCallPart)
	if !ok || call.ToolCallID != "call_1" || !call.Input.Valid() || forkTokens(fork) > ev.Window ||
		!strings.Contains(call.Input.String(), "CONTENT_HEAD") || !strings.Contains(call.Input.String(), "CONTENT_TAIL") {
		t.Fatalf("fork = %d tokens, call = %+v, want the call kept with the head and tail of its arguments", forkTokens(fork), fork.Messages[2])
	}
}

// A summarizer that calls a tool is asked once more with the same prefix; a
// second tool call fails the event.
func TestFoldAsksAgainOnceWhenTheSummarizerCallsATool(t *testing.T) {
	t.Parallel()
	toolCall := sdk.ModelResult{FinishReason: sdk.FinishReasonToolCalls, ToolCalls: []sdk.ToolCall{{ToolCallID: "x", ToolName: "read"}}}
	for _, tc := range []struct {
		name    string
		answers []sdk.ModelResult
		wantErr Reason
	}{
		{name: "answers the second time", answers: []sdk.ModelResult{toolCall, summaryAnswer("Audit summarized.")}},
		{name: "calls a tool twice", answers: []sdk.ModelResult{toolCall, toolCall}, wantErr: ReasonSummarizerFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newFoldSession(t)
			r := s.request()
			summarizer := &scriptedSummarizer{answers: tc.answers}
			passes, err := s.svc.Fold(context.Background(), s.event(r, summarizer))
			if ReasonOf(err) != tc.wantErr || (tc.wantErr == "") != (len(passes) == 1) {
				t.Fatalf("passes=%d err=%v, want reason %q", len(passes), err, tc.wantErr)
			}
			if len(summarizer.requests) != 2 {
				t.Fatalf("summarizer calls = %d, want 2", len(summarizer.requests))
			}
			first, second := summarizer.requests[0], summarizer.requests[1]
			if !slices.EqualFunc(first.Messages[:r.cut], second.Messages[:r.cut], messagesEqual) || !strings.Contains(messageText(second.Messages[r.cut]), noToolsReminder) {
				t.Fatal("the second request changed the prefix or lacks the reminder")
			}
		})
	}
}

// What the request keeps verbatim may leave no room for any summary; the
// event fails before it asks the summarizer.
func TestFoldFailsBeforeAskingWhenNoSummaryFits(t *testing.T) {
	t.Parallel()
	s := newFoldSession(t)
	r := s.request()
	summarizer := &scriptedSummarizer{answers: []sdk.ModelResult{summaryAnswer("unused")}}
	ev := s.event(r, summarizer)
	ev.Allowance = forkTokens(sdk.Request{System: ev.Request.System, Messages: r.messages[r.cut-1:], Tools: ev.Request.Tools}) + 10

	_, err := s.svc.Fold(context.Background(), ev)
	if ReasonOf(err) != ReasonProtectedOverflow || len(summarizer.requests) != 0 {
		t.Fatalf("err=%v summarizer calls=%d, want protected_overflow before any call", err, len(summarizer.requests))
	}
}

// A request composed on a checkpoint the session no longer has matches no
// fold onto the active one.
func TestFoldRefusesARequestOfAnotherCheckpoint(t *testing.T) {
	t.Parallel()
	s := newFoldSession(t)
	r := s.request()
	r.sources[0] = historySource(turn.ContextMessageSource{Kind: "summary", ID: uuid.NewString()})
	summarizer := &scriptedSummarizer{answers: []sdk.ModelResult{summaryAnswer("unused")}}

	_, err := s.svc.Fold(context.Background(), s.event(r, summarizer))
	if ReasonOf(err) != ReasonStaleSource || !errors.Is(err, errStaleSource) || len(summarizer.requests) != 0 {
		t.Fatalf("err=%v summarizer calls=%d, want stale_source before any call", err, len(summarizer.requests))
	}
}

// An edit or retry folds onto the checkpoint its replacement keeps: the
// event rewinds before it reads the active checkpoint.
func TestFoldRewindsBeforeTheTurnAReplacementReplaces(t *testing.T) {
	t.Parallel()
	s := newFoldSession(t)
	r := s.request()
	summarizer := &scriptedSummarizer{answers: []sdk.ModelResult{summaryAnswer("Audit summarized.")}}
	ev := s.event(r, summarizer)
	ev.ReplacedTurnID = uuid.NewString()

	if _, err := s.svc.Fold(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(s.q.rewinds) != 1 || s.q.rewinds[0].TurnID.String() != ev.ReplacedTurnID {
		t.Fatalf("rewinds = %+v, want one before the replaced turn", s.q.rewinds)
	}
}

func messagesEqual(a, b sdk.Message) bool {
	return a.Role == b.Role && messageText(a) == messageText(b) && len(a.Content) == len(b.Content)
}

// However much room the next request has, a summary may take at most a
// share of the allowance, so a fold leaves room for the turns after it; the
// fork's output cap stays above the length it asks for.
func TestFoldBoundsTheSummaryBelowTheRoom(t *testing.T) {
	t.Parallel()
	s := newFoldSession(t)
	r := s.request()
	summarizer := &scriptedSummarizer{answers: []sdk.ModelResult{summaryAnswer("Audit summarized.")}}
	ev := s.event(r, summarizer)
	ev.Window, ev.Allowance, ev.MaxOutputTokens = 200000, 168000, 64000

	if _, err := s.svc.Fold(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	fork := summarizer.requests[0]
	instruction := messageText(fork.Messages[len(fork.Messages)-1])
	_, after, ok := strings.Cut(instruction, "the summary must stay under ")
	if !ok {
		t.Fatalf("instruction states no limit:\n%s", instruction)
	}
	stated, _ := strconv.Atoi(strings.Fields(after)[0])
	if stated <= 0 || stated > ev.Allowance/summaryShareOfAllowance || *fork.MaxTokens <= stated {
		t.Fatalf("stated limit %d, max_tokens %d: want at most %d and below the output cap", stated, *fork.MaxTokens, ev.Allowance/summaryShareOfAllowance)
	}
}

// inbound adds a context row written from an inbound or self-sent message
// and returns the message's external ID.
func (s *foldSession) inbound(external string) string {
	id := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	s.q.history = append(s.q.history, candidateRow{ID: id, Role: "user", Content: []byte(`"row"`), ExternalMessageID: pgtype.Text{String: external, Valid: true}})
	return external
}

// A message that renders a source no context row holds cannot be folded:
// the checkpoint has nothing to stand for, so it stays where it is and the
// summary leaves it alone. A self-sent message folds by the row it was
// written to, and the pass names it as the request did, so the request's own
// splice removes it.
func TestFoldStandsOnlyForWhatRowsHold(t *testing.T) {
	t.Parallel()
	s := newFoldSession(t)
	r := s.request()
	echo := turn.ContextMessageSource{Kind: "self", ID: s.inbound("echo-1")}
	ghost := turn.ContextMessageSource{Kind: "external", ID: "never-written"}
	r.messages = slices.Insert(r.messages, 1, sdk.UserMessage("the bot said so"+strings.Repeat(" e", 400)), sdk.UserMessage("a message no row holds"))
	r.sources = slices.Insert(r.sources, 1, historySource(echo), historySource(ghost))
	r.cut += 2
	summarizer := &scriptedSummarizer{answers: []sdk.ModelResult{summaryAnswer("The parser audit and the bot's note are summarized.")}}

	passes, err := s.svc.Fold(context.Background(), s.event(r, summarizer))
	if err != nil || len(passes) != 1 {
		t.Fatalf("passes=%+v err=%v, want one pass", passes, err)
	}
	if !slices.Contains(passes[0].Covered, echo) || slices.Contains(passes[0].Covered, ghost) {
		t.Fatalf("covered = %v, want the self-sent message folded by its row and the rowless one left", passes[0].Covered)
	}
	instruction := messageText(summarizer.requests[0].Messages[len(summarizer.requests[0].Messages)-1])
	if !strings.Contains(instruction, "a message no row holds") {
		t.Fatalf("the instruction does not name the message that stays:\n%s", instruction)
	}
	messages, _, _ := splice(r.messages, r.sources, r.cut, passes[0])
	if slices.ContainsFunc(messages, func(m sdk.Message) bool { return strings.HasPrefix(messageText(m), "the bot said so") }) ||
		!slices.ContainsFunc(messages, func(m sdk.Message) bool { return messageText(m) == "a message no row holds" }) {
		t.Fatal("the splice did not remove the folded self-sent message and keep the rowless one")
	}
}

// When nothing but summaries is left to fold, an event rewrites them only
// when they do not fit the room the next request has, and the rewrite must
// fit it; summaries that fit are left alone.
func TestFoldRewritesSummariesOnlyToFitTheRoom(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		allowance int
		passes    int
	}{
		{name: "fits", allowance: 24000},
		{name: "over the room", allowance: 1000, passes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newFoldSession(t)
			earlier := strings.Repeat("The earlier work is long. ", 200)
			held := s.q.hold(earlier)
			input := s.row()
			messages := []sdk.Message{SummaryMessage(earlier), sdk.UserMessage("RUN_INPUT"), callMessage("call_1"), resultMessage("call_1", "done")}
			sources := []Source{historySource(turn.ContextMessageSource{Kind: "summary", ID: summaryIDs(held)[0]}), {Sources: []turn.ContextMessageSource{input}, Anchor: true}, historySource(s.row()), historySource(s.row())}
			summarizer := &scriptedSummarizer{answers: []sdk.ModelResult{summaryAnswer("Earlier work, shortened.")}}
			ev := s.event(request{messages: messages, sources: sources, cut: 2}, summarizer)
			ev.Allowance = tc.allowance

			passes, err := s.svc.Fold(context.Background(), ev)
			if err != nil || len(passes) != tc.passes {
				t.Fatalf("passes=%d err=%v, want %d", len(passes), err, tc.passes)
			}
			if tc.passes == 0 {
				return
			}
			if len(passes[0].Covered) != 0 || !slices.Equal(passes[0].Absorbed, summaryIDs(held)) || passes[0].More {
				t.Fatalf("pass = %+v, want the summary rewritten and nothing else folded", passes[0])
			}
			if limit := summaryLimitIn(t, summarizer.requests[0]); limit > tc.allowance {
				t.Fatalf("the rewrite may take %d tokens, over the %d the request has", limit, tc.allowance)
			}
		})
	}
}

// A residue too small for any summary to replace ends the event once the
// request fits, rather than failing it as ineffective.
func TestFoldLeavesAResidueTooSmallToSummarize(t *testing.T) {
	t.Parallel()
	s := newFoldSession(t)
	held := s.q.hold("Earlier work summarized.")
	input := s.row()
	messages := []sdk.Message{SummaryMessage("Earlier work summarized."), sdk.AssistantMessage("ok"), sdk.UserMessage("RUN_INPUT"), callMessage("call_1"), resultMessage("call_1", "done")}
	sources := []Source{historySource(turn.ContextMessageSource{Kind: "summary", ID: summaryIDs(held)[0]}), historySource(s.row()), {Sources: []turn.ContextMessageSource{input}, Anchor: true}, historySource(s.row()), historySource(s.row())}
	summarizer := &scriptedSummarizer{answers: []sdk.ModelResult{summaryAnswer("unused")}}

	passes, err := s.svc.Fold(context.Background(), s.event(request{messages: messages, sources: sources, cut: 3}, summarizer))
	if err != nil || len(passes) != 0 || len(summarizer.requests) != 0 {
		t.Fatalf("passes=%d err=%v summarizer calls=%d, want the fitting request left alone", len(passes), err, len(summarizer.requests))
	}
}

// A Discuss run keeps the inputs it answers while they leave a tenth of the
// room for the summary; the default cap keeps a run's input and steers
// within a quarter of the allowance. Older anchors fold either way.
func TestFoldKeepsAnchorsByPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		policy AnchorPolicy
		folded int
	}{
		{name: "capped", policy: AnchorsCapped, folded: 3},
		{name: "inputs first", policy: AnchorsFirst, folded: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newFoldSession(t)
			held := s.q.hold("Earlier talk summarized.")
			messages := []sdk.Message{SummaryMessage("Earlier talk summarized.")}
			sources := []Source{historySource(turn.ContextMessageSource{Kind: "summary", ID: summaryIDs(held)[0]})}
			var inputs []turn.ContextMessageSource
			for i := range 4 {
				input := turn.ContextMessageSource{Kind: "external", ID: s.inbound("input-" + strconv.Itoa(i))}
				inputs = append(inputs, input)
				messages = append(messages, sdk.UserMessage("input "+strconv.Itoa(i)+strings.Repeat(" i", 1200)))
				sources = append(sources, Source{Sources: []turn.ContextMessageSource{input}, Anchor: true})
			}
			summarizer := &scriptedSummarizer{answers: []sdk.ModelResult{summaryAnswer("Earlier talk and the oldest inputs summarized.")}}
			ev := s.event(request{messages: messages, sources: sources, cut: len(messages)}, summarizer)
			ev.Request.Tools, ev.Allowance, ev.Window, ev.Anchors = nil, 3200, 12000, tc.policy

			passes, err := s.svc.Fold(context.Background(), ev)
			if err != nil || len(passes) != 1 {
				t.Fatalf("passes=%d err=%v, want one pass", len(passes), err)
			}
			for i, input := range inputs {
				if folded := slices.Contains(passes[0].Covered, input); folded != (i < tc.folded) {
					t.Fatalf("input %d folded=%v, want the %d oldest folded", i, folded, tc.folded)
				}
			}
		})
	}
}

// summaryLimitIn is the summary length a fork request's instruction states.
func summaryLimitIn(t *testing.T, fork sdk.Request) int {
	t.Helper()
	instruction := messageText(fork.Messages[len(fork.Messages)-1])
	_, after, ok := strings.Cut(instruction, "must stay under ")
	if !ok {
		t.Fatalf("the instruction states no summary limit:\n%s", instruction)
	}
	limit, err := strconv.Atoi(strings.Fields(after)[0])
	if err != nil {
		t.Fatal(err)
	}
	return limit
}

// A pass refuses a request that rendered a source at a version its row no
// longer holds, before it asks for a summary: a stored message whose content
// changed since, or an inbound message that took another event. A source
// whose version the request did not record is taken as it is.
func TestFoldRefusesASourceThatChangedSinceTheRequestRenderedIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		version func(*turn.ContextMessageSource)
		stale   bool
	}{
		{name: "unrecorded"},
		{name: "edited content", version: func(s *turn.ContextMessageSource) { s.ContentHash = "not-the-row" }, stale: true},
		{name: "later event", version: func(s *turn.ContextMessageSource) { s.Kind, s.EventCursor = "external", 7 }, stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newFoldSession(t)
			r := s.request()
			source := r.sources[1].Sources[0]
			if tc.version != nil {
				tc.version(&source)
				if source.Kind == "external" {
					source.ID = s.inbound("inbound-1")
				}
			}
			r.sources[1] = historySource(source)
			summarizer := &scriptedSummarizer{answers: []sdk.ModelResult{summaryAnswer("The parser audit found one unsafe cast at parser.go:12.")}}

			_, err := s.svc.Fold(context.Background(), s.event(r, summarizer))
			if stale := ReasonOf(err) == ReasonStaleSource; stale != tc.stale || tc.stale && len(summarizer.requests) != 0 {
				t.Fatalf("err=%v summarizer calls=%d, want stale=%v before any summary", err, len(summarizer.requests), tc.stale)
			}
		})
	}
}
