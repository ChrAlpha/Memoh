package agent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/turn"
)

// benchScenario runs once per client type on a fresh session.
type benchScenario struct {
	name string
	opts benchOptions
	run  func(t *testing.T, e *benchEnv)
}

// A conversation of several turns, each calling a tool before it answers,
// never changes what an earlier request already sent: the next request of the
// turn and the first request of the next turn both begin with the whole
// previous prompt.
func chatToolTurns(t *testing.T, e *benchEnv) {
	e.chatTurn("t1", "What is the capital of France?", nil, toolThenAnswer("france", "Paris.")...)
	e.chatTurn("t2", "And of Italy?", nil, toolThenAnswer("italy", "Rome.")...)
	e.chatTurn("t3", "And of Spain?", nil, toolThenAnswer("spain", "Madrid.")...)
	requireReuse(t, e.pairs("chat, tools each turn"), noBreak)
}

// Memory written after a turn rewrites MEMORY.md, which sits in the system
// prompt ahead of the whole history. While the session's cache is warm the
// rewrite waits, so the next turn keeps its prefix; once the session has been
// idle past the cache lifetime the deferred rewrite happens at the start of the
// next turn, and only that turn's first request may differ.
func memoryWriteWaitsForColdCache(t *testing.T, e *benchEnv) {
	e.chatTurn("t1", "Remember that I like tea.", nil, toolThenAnswer("tea", "Noted.")...)
	e.chatTurn("t2", "And that I dislike coffee.", nil, toolThenAnswer("coffee", "Noted.")...)
	if memory := e.readWorkspaceFile("MEMORY.md"); strings.Contains(memory, "fact number 1") {
		t.Fatalf("MEMORY.md was rewritten while the session was warm:\n%s", memory)
	}
	e.idleFor(6 * time.Minute)
	e.chatTurn("t3", "What do I like?", nil, toolThenAnswer("likes", "Tea.")...)
	if memory := e.readWorkspaceFile("MEMORY.md"); !strings.Contains(memory, "fact number 2") {
		t.Fatalf("MEMORY.md was not rewritten at the first turn after the cache went cold:\n%s", memory)
	}
	e.chatTurn("t4", "And what don't I like?", nil, toolThenAnswer("dislikes", "Coffee.")...)
	requireReuse(t, e.pairs("memory write, then cold cache"), breakInto(map[string]string{"t3": "system prompt: memory overview (MEMORY.md)"}))
}

// A change to an instruction file in the workspace is an expected break: the
// request that first carries it differs at that file, and the turns after it
// reuse the new prompt in full.
func workspaceFileChange(t *testing.T, e *benchEnv) {
	e.chatTurn("t1", "Hello.", nil, toolThenAnswer("hello", "Hi.")...)
	e.chatTurn("t2", "Still there?", nil, toolThenAnswer("there", "Yes.")...)
	e.writeWorkspaceFile("AGENTS.md", "Answer in French from now on.\n")
	e.chatTurn("t3", "Ça va?", nil, toolThenAnswer("va", "Oui.")...)
	e.chatTurn("t4", "Encore?", nil, toolThenAnswer("encore", "Oui.")...)
	requireReuse(t, e.pairs("workspace file change"), breakInto(map[string]string{"t3": "system prompt: workspace file (AGENTS.md)"}))
}

// Discuss batches carry several external messages each; the history a batch's
// run reads is what the earlier batches wrote.
func discussBatches(t *testing.T, e *benchEnv) {
	alice := e.discuss.participant(t, false)
	e.discussBatch("b1", alice, []string{"Release is on Friday.", "Who owns the notes?"}, toolThenAnswer("release", "Alice does.")...)
	e.discussBatch("b2", alice, []string{"Add the migration list."}, toolThenAnswer("migrations", "Added.")...)
	e.discussBatch("b3", alice, []string{"Freeze starts Thursday.", "Please confirm."}, toolThenAnswer("freeze", "Confirmed.")...)
	requireReuse(t, e.pairs("discuss, several batches"), noBreak)
}

// A picture rides only the turn it arrived in: the stored history row keeps
// the text, so the first request of the next turn differs from the previous one
// at that message, and nowhere before it. Requests inside a turn, and turns that
// follow, reuse the full prompt.
func pictureTurns(t *testing.T, e *benchEnv) {
	picture := []turn.Attachment{{Type: "image", ContentHash: "bench-picture", Mime: "image/png", Name: "photo.png"}}
	e.chatTurn("t1", "What is in this picture?", picture, toolThenAnswer("picture", "Noise.")...)
	e.chatTurn("t2", "Is it a photo?", nil, toolThenAnswer("photo", "No.")...)
	e.chatTurn("t3", "And this one?", picture, toolThenAnswer("again", "Same noise.")...)
	e.chatTurn("t4", "Thanks.", nil, toolThenAnswer("thanks", "Welcome.")...)
	requireReuse(t, e.pairs("picture in a turn"), breakInto(map[string]string{"t2": "history", "t4": "history"}))
}

// Summarizing old history is an expected break: the first request after a
// summary differs from the previous one inside the history, never in the tool
// definitions or the system prompt. Every other request reuses the full prompt,
// including the ones after the summary. The blocking compaction runs at the
// start of a turn, so the turns continue until it has run and two turns have
// followed it.
func compactionTurns(t *testing.T, e *benchEnv) {
	query := strings.Repeat("padding ", 3000)
	compacted := func(p benchPair) bool { return p.to.Summaries > p.from.Summaries }
	settled := func() bool {
		clean, seen := 0, false
		for _, p := range e.measure() {
			switch {
			case compacted(p):
				clean, seen = 0, true
			case seen && p.kind == crossTurn:
				clean++
			}
		}
		return clean >= 2
	}
	for i := 1; i <= 24 && !settled(); i++ {
		label := fmt.Sprintf("t%d", i)
		e.chatTurn(label, label+" "+query, nil, toolThenAnswer("pad", "Done.")...)
	}
	if !settled() {
		t.Fatal("the history was never summarized")
	}
	for _, p := range e.pairs("history compaction") {
		switch {
		case p.report.Break == nil:
		case compacted(p) && p.kind == crossTurn && p.report.Break.Part == "history":
		default:
			t.Errorf("%s request %s reuses %.1f%% of the previous prompt; first difference: %v",
				p.kind, p.name(), 100*p.report.Ratio(), p.report.Break)
		}
	}
}

func TestPromptCacheReuse(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	scenarios := []benchScenario{
		{name: "chat, tools each turn", run: chatToolTurns},
		{name: "memory write, then cold cache", run: memoryWriteWaitsForColdCache},
		{name: "workspace file change", run: workspaceFileChange},
		{name: "discuss, several batches", opts: benchOptions{discuss: true}, run: discussBatches},
		{name: "picture in a turn", opts: benchOptions{vision: true}, run: pictureTurns},
		{name: "history compaction", opts: benchOptions{compaction: true, contextWindow: 60000}, run: compactionTurns},
	}
	for _, scenario := range scenarios {
		for _, clientType := range promptCacheClientTypes {
			t.Run(scenario.name+"/"+string(clientType), func(t *testing.T) {
				e := newBenchEnv(t, clientType, scenario.opts)
				scenario.run(t, e)
			})
		}
	}
	printBenchTable(t)
}
