package compaction

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/chat/checkpoint"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/models/modelretry"
)

// Source is what one message of a folded request stands for.
type Source struct {
	// Sources are the history rows, inbound messages or summary the message
	// renders. A message with none is runtime context the caller rebuilds
	// for every request: it stays where it is and no summary stands for it.
	Sources []turn.ContextMessageSource
	// Anchor keeps the message verbatim: the run's input or a steer. A fold
	// keeps anchors newest first up to the anchor cap and folds the rest.
	Anchor bool
}

// Summarizer answers a fork request: the run's own model, or the compaction
// model for a request built from rows.
type Summarizer func(context.Context, sdk.Request) (sdk.ModelResult, error)

// Event is one compaction event over a request its caller holds: the run's
// next request, or one built from rows the run cannot show.
type Event struct {
	BotID     string
	SessionID string
	// ModelRecordID names the summarizer's model row in the compaction log.
	ModelRecordID string
	// Request is what the caller sends next and Sources what each of its
	// messages renders. Messages from Cut on are the tail the caller keeps
	// verbatim after the summary: the latest step, or the input a run starts
	// from. A step folds too when the request cannot keep it (tailFolds).
	Request sdk.Request
	Sources []Source
	Cut     int
	// Current is the input a run starts from when the tail holds it: the
	// summarizer reads it to judge what the work needs.
	Current []sdk.Message
	// Anchors is how much of the request's anchors the fold keeps verbatim.
	Anchors AnchorPolicy
	// Window is the summarizer's context window and MaxOutputTokens its
	// output cap; Allowance is what the caller's next request may take.
	Window          int
	MaxOutputTokens int
	Allowance       int
	// Pressure is the request size that called for the event, for hooks.
	Pressure int
	// ReplacedTurnID is the turn an edit or retry replaces: the event first
	// moves the session to the checkpoint its replacement keeps.
	ReplacedTurnID string
	Summarizer     Summarizer
	// Apply splices every committed pass into the caller's request as it
	// commits, and returns the request, sources and cut a further pass reads.
	Apply func(Pass) (sdk.Request, []Source, int)
}

// Fold runs one compaction event over a request its caller holds: it forks
// the request before its tail and asks for the summary that replaces
// everything foldable there, the summaries of the checkpoint the request
// renders included. A pass reads as much of the request as the summarizer's
// window holds; the event passes again only for what that left. It returns
// the passes it committed; a failed pass ends the event with an *EventError
// and leaves the passes before it.
func (s *Service) Fold(ctx context.Context, ev Event) ([]Pass, error) {
	scope := hookScope{BotID: ev.BotID, SessionID: ev.SessionID, ModelID: ev.Request.Model, InputTokens: ev.Pressure, BudgetTokens: ev.Allowance}
	return s.event(ctx, scope, ev.ReplacedTurnID, func(ctx context.Context) ([]Pass, error) {
		return s.passes(ctx, ev)
	})
}

// passes runs ev's passes, splicing each into the caller's request through
// ev.Apply, until one reads up to the tail or nothing is left to fold. A tail
// that leaves the request over its allowance once nothing before it is left
// to fold, or that leaves a summary no room, folds too (tailFolds).
func (s *Service) passes(ctx context.Context, ev Event) ([]Pass, error) {
	var passes []Pass
	for {
		pass, folded, err := s.foldPass(ctx, ev)
		if (err == nil && !folded || ReasonOf(err) == ReasonProtectedOverflow) && tailFolds(ev) {
			ev.Cut, ev.Current = len(ev.Request.Messages), nil
			continue
		}
		if err != nil {
			return passes, err
		}
		if !folded {
			return passes, nil
		}
		passes = append(passes, pass)
		ev.Request, ev.Sources, ev.Cut = ev.Apply(pass)
		if !pass.More {
			return passes, nil
		}
	}
}

// foldPass forks the request once and commits the summary it gets as the
// session's next checkpoint. It reports false when nothing before the tail
// can be folded.
func (s *Service) foldPass(ctx context.Context, ev Event) (Pass, bool, error) {
	if ev.Cut < 0 || ev.Cut > len(ev.Request.Messages) || len(ev.Sources) != len(ev.Request.Messages) {
		return Pass{}, false, fmt.Errorf("compaction: request of %d messages has %d sources and cut %d", len(ev.Request.Messages), len(ev.Sources), ev.Cut)
	}
	parent, err := s.checkpoints.Active(ctx, ev.SessionID)
	if err != nil {
		return Pass{}, false, err
	}
	if err := rendersCheckpoint(ev, parent); err != nil {
		return Pass{}, false, err
	}
	rows, err := s.requestRows(ctx, ev)
	if err != nil {
		return Pass{}, false, err
	}
	plan, err := planFold(ev, rows.unfoldable(ev))
	if err != nil || !plan.folds() {
		return Pass{}, false, err
	}
	fork := ev.Request
	fork.Messages = append(slices.Clip(plan.fork), sdk.UserMessage(plan.instruction))
	fork.MaxTokens = &plan.maxTokens

	persistCtx := context.WithoutCancel(ctx)
	logID, err := s.openLog(persistCtx, ev.BotID, ev.SessionID)
	if err != nil {
		return Pass{}, false, err
	}
	fail := func(err error) (Pass, bool, error) {
		_ = s.completeLog(persistCtx, s.queries, logID, "error", "", err.Error(), 0, nil, pgtype.UUID{})
		return Pass{}, false, err
	}
	result, err := s.summarize(ctx, ev, fork, plan)
	if err != nil {
		return fail(err)
	}
	summary := strings.TrimSpace(result.Text)
	replay := summaryReplayTokens(summary)
	switch {
	case replay >= plan.replaced:
		return fail(failed(ReasonIneffective, fmt.Errorf("%w: summary_tokens=%d replaced_tokens=%d", errIneffectiveSummary, replay, plan.replaced)))
	case plan.bounded() && replay > plan.room:
		return fail(failed(ReasonStillOver, fmt.Errorf("%w: replay=%d room=%d", errNoSummaryRoom, replay, plan.room)))
	}

	folds := rows.of(plan)
	commit := checkpoint.Fold{
		SessionID: ev.SessionID,
		Parent:    parent,
		SummaryID: formatUUID(logID),
		Summary:   summary,
		Folded:    folds.folded,
		Read:      folds.read,
	}
	usage, _ := json.Marshal(result.Usage)
	if err := s.inSession(ctx, ev.BotID, ev.SessionID, func(q dbstore.Queries) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := s.checkpoints.Commit(ctx, q, commit); err != nil {
			return err
		}
		return s.completeLog(ctx, q, logID, "ok", summary, "", len(folds.folded), usage, db.ParseUUIDOrEmpty(ev.ModelRecordID))
	}); err != nil {
		return fail(staleSource(err))
	}
	order, cut := plan.order(ev)
	return Pass{
		SummaryID: commit.SummaryID,
		Summary:   summary,
		Order:     order,
		Cut:       cut,
		More:      !plan.final(),
	}, true, nil
}

// tailFolds reports whether ev's tail folds with what is before it: the
// request is over its allowance and the tail holds a completed step, whose
// calls and results the summary can stand for and get_messages reads back.
// A tail that holds only the input a run starts from stays, so the event
// fails as protected rather than summarize the input it answers.
func tailFolds(ev Event) bool {
	if ev.Cut >= len(ev.Request.Messages) || forkTokens(ev.Request) <= ev.Allowance {
		return false
	}
	return slices.ContainsFunc(ev.Sources[ev.Cut:], func(source Source) bool {
		return len(source.Sources) > 0 && !source.Anchor && !isSummarySource(source)
	})
}

// rendersCheckpoint refuses a request composed on a checkpoint the session
// no longer has: the summaries it renders must be the ones the active
// checkpoint holds, or no fold onto that checkpoint matches it.
func rendersCheckpoint(ev Event, parent checkpoint.Checkpoint) error {
	rendered := map[string]bool{}
	for _, source := range ev.Sources {
		for _, s := range source.Sources {
			if s.Kind == "summary" {
				rendered[s.ID] = true
			}
		}
	}
	held := parent.SummaryIDs()
	matches := len(rendered) == len(held)
	for _, id := range held {
		matches = matches && rendered[id]
	}
	if !matches {
		return failed(ReasonStaleSource, fmt.Errorf("%w: the request renders summaries the active checkpoint %q does not hold", errStaleSource, parent.ID))
	}
	return nil
}

// summarize asks for the summary. A transient failure gets one immediate
// retry, or the wait the provider asks for; a rejected request is not made
// again. A model that answers with a tool call is asked once more, with the
// same prefix and a firmer instruction; the answer must finish naturally and
// say something.
func (s *Service) summarize(ctx context.Context, ev Event, fork sdk.Request, plan foldPlan) (sdk.ModelResult, error) {
	ask := func(fork sdk.Request) (sdk.ModelResult, error) {
		return modelretry.Do(models.WithModelSession(ctx, ev.SessionID), s.logger, "agent.compaction",
			modelretry.Config{MaxAttempts: 1, FastAttempts: 1}, modelretry.Retryable,
			func(ctx context.Context) (sdk.ModelResult, error) { return ev.Summarizer(ctx, fork) })
	}
	result, err := ask(fork)
	if err == nil && len(result.ToolCalls) > 0 {
		s.logger.WarnContext(ctx, "compaction: the summarizer called a tool, asking again",
			slog.String("session_id", ev.SessionID), slog.Int("tool_calls", len(result.ToolCalls)))
		// The reminder takes from what the fork may answer, so the request
		// stays within the window it was sized for.
		reminder := "\n\n" + noToolsReminder
		fork.Messages = append(slices.Clip(fork.Messages[:len(fork.Messages)-1]), sdk.UserMessage(plan.instruction+reminder))
		maxTokens := plan.maxTokens - instructionTokens(reminder)
		fork.MaxTokens = &maxTokens
		result, err = ask(fork)
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return sdk.ModelResult{}, ctxErr
		}
		return sdk.ModelResult{}, failed(ReasonSummarizerFailed, err)
	}
	switch {
	case len(result.ToolCalls) > 0:
		return sdk.ModelResult{}, failed(ReasonSummarizerFailed, fmt.Errorf("compaction: the summarizer called %d tools", len(result.ToolCalls)))
	case strings.TrimSpace(result.Text) == "":
		return sdk.ModelResult{}, failed(ReasonEmpty, errEmptySummary)
	case result.FinishReason != sdk.FinishReasonStop:
		return sdk.ModelResult{}, failed(ReasonTruncated, fmt.Errorf("%w: finish_reason=%s max_tokens=%d", errIncompleteSummary, result.FinishReason, plan.maxTokens))
	}
	return result, nil
}

// requestRows is where the sources a request renders before its cut stand
// in the history: the context rows that hold each one, by the source as the
// request names it. A history source names its row; an inbound or a
// self-sent message names the rows it was written to.
type requestRows map[turn.ContextMessageSource][]checkpoint.Row

func (s *Service) requestRows(ctx context.Context, ev Event) (requestRows, error) {
	sessionUUID, err := db.ParseUUID(ev.SessionID)
	if err != nil {
		return nil, err
	}
	var ids []pgtype.UUID
	var external []string
	for _, source := range ev.Sources[:ev.Cut] {
		for _, s := range source.Sources {
			switch s.Kind {
			case "history":
				if id, err := db.ParseUUID(s.ID); err == nil {
					ids = append(ids, id)
				}
			case "external", "self":
				if s.ID != "" {
					external = append(external, s.ID)
				}
			}
		}
	}
	if len(ids) == 0 && len(external) == 0 {
		return nil, nil
	}
	found, err := s.queries.ListHistoryRowsBySources(ctx, sqlc.ListHistoryRowsBySourcesParams{SessionID: sessionUUID, Ids: ids, ExternalMessageIds: external})
	if err != nil {
		return nil, fmt.Errorf("load the rows the request renders: %w", err)
	}
	rows := requestRows{}
	for _, row := range found {
		id := formatUUID(row.ID)
		held := checkpoint.Row{ID: id, Position: messagepkg.HistoryPosition{Turn: row.TurnPosition.Int64, Seq: row.TurnMessageSeq.Int64}}
		key := turn.ContextMessageSource{Kind: "history", ID: id}
		rows[key] = append(rows[key], held)
		if message := strings.TrimSpace(row.SourceMessageID.String); message != "" {
			for _, kind := range []string{"external", "self"} {
				key := turn.ContextMessageSource{Kind: kind, ID: message}
				rows[key] = append(rows[key], held)
			}
		}
	}
	return rows, nil
}

// unfoldable marks the messages before ev's cut a fold cannot stand for: one
// that renders a source no context row holds stays where it is, as the
// checkpoint has nothing to fold for it.
func (r requestRows) unfoldable(ev Event) map[int]bool {
	out := map[int]bool{}
	for i, source := range ev.Sources[:ev.Cut] {
		if isSummarySource(source) {
			continue
		}
		for _, s := range source.Sources {
			if len(r[turn.ContextMessageSource{Kind: s.Kind, ID: s.ID}]) == 0 {
				out[i] = true
				break
			}
		}
	}
	return out
}

// foldRows is what a pass read and folds as its checkpoint names them: the
// rows it folds and the rows it read.
type foldRows struct {
	folded []checkpoint.Row
	read   []string
}

func (r requestRows) of(plan foldPlan) foldRows {
	var out foldRows
	read, folded := map[string]bool{}, map[string]bool{}
	for _, source := range plan.read {
		for _, row := range r[turn.ContextMessageSource{Kind: source.Kind, ID: source.ID}] {
			if !read[row.ID] {
				read[row.ID] = true
				out.read = append(out.read, row.ID)
			}
		}
	}
	for _, source := range plan.folded {
		for _, row := range r[turn.ContextMessageSource{Kind: source.Kind, ID: source.ID}] {
			if !folded[row.ID] {
				folded[row.ID] = true
				out.folded = append(out.folded, row)
			}
		}
	}
	return out
}

// noToolsReminder is what a second fork request adds after a tool call.
const noToolsReminder = "Your previous answer called a tool. Do not call any tool: answer with the summary text alone."

// hookScope is what the compaction hooks are told about an event.
type hookScope struct {
	BotID        string
	SessionID    string
	ModelID      string
	InputTokens  int
	BudgetTokens int
}

// forkTokens prices a fork request as its provider receives it.
func forkTokens(req sdk.Request) int {
	return contextfrag.ProviderEnvelopeTokens(req.System, req.Messages, req.Tools)
}
