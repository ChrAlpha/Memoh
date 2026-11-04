# Recall dedup: store what each turn selected, render each request without repeats

Date: 2026-10-29. Status: approved design (v3), not yet implemented. No product code changed.
Code read: `feat/persist-turn-context` @ `ac29ce30b` (on `agent-unify-20261023` @ `6c0c339b4`) and #1700 @
`3de5ef0e4`. Target: the combined head, persist-turn-context rebased onto main + #1700 + the integrated agent-unify
(landing plan `/workspaces/Memoh-evidence/unify-onto-1700-20261029/CONFLICTS.md`). Names are #1700's where they differ.

Two earlier drafts persisted each turn's *emitted* subset and repaired it after folds; review found that every repair
needed another durable protocol (selection recovery, restoration events with positions, commit cursors, reservations,
fold-cursor fixes). This design stores the source and derives the rest. Evidence for every claim below, including the
earlier counterexamples used as test vectors, is in `/workspaces/Memoh-evidence/recall-dedup-design-20261029/`
(`REVIEW-r1.md`, `REVIEW-r2.md`, `v3/REVIEW-r1.md` and their probes).

## 1. Problem

`feat/persist-turn-context` records each turn's runtime context (memory recall, requested skills) on the turn's
request row and replays it right after that row in later requests. The cross-turn prefix became byte-stable, but every
recall stays in the context until its row is folded, and a topical conversation recalls largely the same memories
every turn.

20-turn, 40-request fixture, identical 11,133-byte recall per turn, pre-persistence `a53613ae1` vs persistence
(canonicalized prompt bytes, not billed tokens; `/workspaces/Memoh-evidence/review-24c50-20261029/REPORT.md`):

| | Small tool output | Large tool output (65 KB/result) |
| --- | ---: | ---: |
| Cumulative prompt bytes | 882,812 → 5,143,372 (5.83x) | 26,882,812 → 31,143,372 (+15.8%) |
| Cumulative non-reusable bytes | 246,640 → 239,676 (−2.8%) | 2,781,640 → 1,539,676 (−44.6%) |
| Final request bytes | 26,648 → 239,676 | 1,326,648 → 1,539,676 |

The builtin packer selects up to 6 items with a 1,800-character body budget, plus ellipses, labels and envelope (a
real pack emits 1,815 snippet characters before labels); bodies over 360 characters are always truncated. A typical
recall is several partial items, roughly 2-5 KB depending on script, replayed once per visible turn. On six identical
long-body turns the coverage rule below cuts newly emitted snippet bytes over 20 turns from 32,860 to 1,643.

## 2. Model and guarantee

A **recall unit** is a request row together with what its turn's provider recall selected: the ordered selected
fragments and the exact hook bytes. That is what is stored. What the model sees is derived by one renderer over the
request's **view** (§4.4): walking the view in order, each unit emits the selected fragments not covered (§4.2) by an
earlier eligible showing, followed by its hook bytes.

**Guarantee (ordinary requests).** At every ordinary model dispatch of a native run, for every valid unit in the
final view, each selected fragment is emitted in that unit or covered by an earlier showing that is eligible for that
unit (after the last summary or barrier before the unit, with no different later assertion for its ref).

**Fold behaviour (not a guarantee of summarizer exposure).** When a fold removes a unit, its recall leaves with it,
as without dedup. Each selected fragment of a folded unit has a showing in the uncapped read of that pass or an
earlier one: the covering showing precedes the unit, a pass reads a prefix through the units it folds, and summary
placement keeps survivor order. The summarizer input cap (`cutGroup`/`cutParts`) can still clip that showing, and
because dedup removed the duplicate, a fragment can reach no summarizer at all where the full selection would have
shown it once. This is accepted: the memory store, not the transcript, is the source of truth for memory; a fragment
lost from a summary is recalled again by the next turn whose query selects it, and that turn shows it because the
summary cleared coverage. Cap losses are counted (§4.6).

Properties:

- **P1, prefix-causal rendering.** Given the typed view (§4.4), the rendering of a unit depends only on the entries
  before it and on the unit itself. Equal view prefixes render to equal recall bytes. Other sources of request change
  (system prompt, prompt hooks, provider normalization, cache metadata) are outside this property and are measured
  as today.
- **P2, a fold preserves what precedes its first change.** For a fold event, the rendered request is unchanged
  before the first position where the event changed or removed an entry of the old view. Units after that position
  may gain fragments, possibly before the new summary. Cache cost beyond that position, including extra events from
  re-render overflow, is measured, not assumed free.
- **P3, selection.** Rendering only omits from a selection, in its order. It never shows a fragment the unit's turn
  did not select, nor a longer prefix than it selected.

Out of scope: dedup of hook text; summaries as coverage; external runtimes (Codex, Claude Code, ACP), which render the
full selection into their separate sections; the inherited subagent fork snapshot, which carries no units (a direct
subagent-thread turn may repeat a memory the snapshot shows); units whose stored selection is invalid (§4.3).

## 3. Facts the design rests on

- **Provider.** Production is `builtin.BuiltinProvider` (graph runtime, optional pgvector index; file runtime still
  implemented). `OnBeforeChat` renders `<memory-context>` with one `- [label] snippet` entry per packed item (label
  optional, snippet may span lines). Other implementations are tests: `agenttest.MemoryProvider`,
  `promptcache_fixture_test.go`, `memory/recall_test.go`, `handlers/memory_capability_test.go`,
  `memory/adapters/service_test.go`.
- **IDs identify a memory, not its content or the order of assertions.** `Update` keeps the ID and replaces the body;
  legacy bare IDs are canonicalized on `Update`/`Compact`; `Compact` keeps a representative ID and may change body and
  capture time (the label date). `runtimeHash` hashes the trimmed body, so A→B→A returns to A's hash. Imported `Hash`
  values are not always recomputed.
- **Bodies are unbounded.** Memory tools cap a body at 2,000 runes, but the management update, graph/file `Update`
  and file ingest do not; a 61,439-byte body was accepted in a probe.
- **Snippets are trimmed prefixes of variable length.** The packer shares the budget, shrinks to 80 and regrows to at
  most 360 characters, then reorders. `TruncateSnippet(s, n)` returns the trimmed body when it fits, otherwise
  `TrimSpace(first n runes) + "..."`, so the retained prefix can be shorter than n.
- **Call sites.** `runconfig.Resolve` (Web, IM, schedule, direct subagent-thread turns) and
  `runconfig.BuildRuntimeContextSections` (external runtimes; separate memory and hook sections with their own trust
  and overflow policy). Native Discuss reaches `Resolve` with no query and recalls nothing; spawned subagents bypass
  `Resolve`. The after-memory hook runs once per recall (`memory/recall.go:119-156`).
- **Materialization.** #1700 renders a checkpoint's ordered items (kept rows and summaries interleaved), then the
  byte-budgeted tail (`application/service_history.go:111-175`); retry and edit render `KeptBeforeTurn`. After the
  load, preparation prunes failures and may merge a required-window reread, which loads a whole visible suffix
  without a byte bound (`service_history.go:227-245`). The expander suppresses the stored runtime projections of the
  run's own reused request (every `ReusePersistedUserMessage`, including Discuss head reuse) but keeps the row
  (`transcript/history.go:41-49`); duplicate-input row removal is a separate rule (`UserMessagePersisted &&
  !RequestStaysInContext`, `history.go:243-250`), which Discuss does not trigger. Tail admission counts content only
  and keeps its crossing row.
- **Runtime context today** is expanded after its request row as source-less synthetic records, memory before
  requested skills. #1700's fold treats a source-less message as context the caller rebuilds: it stays and no summary
  stands for it (`compaction/fold.go:25-33`).
- **In-run folds.** `prepareProviderAttempt` calls the context switch; the application runs one `compaction.Fold`
  event; each pass commits, is spliced by `sw.Fold` and measured (`native/context_switch.go:147-152`); after the event
  the application checks the final size, the runtime installs the splice and measures the envelope again
  (`native/agent.go:320-326`), and Generate/Stream rebuild and dispatch. `Fold` keeps a numeric cut between passes
  (`fold.go:125`). The splice state (`requestSplice` messages, provenance, `from`) is owned by the runtime.
- **Summaries are lossy and clipped**; a probe reduced 6,024 bytes including a recall to 392 bytes without it.

## 4. Design

### 4.1 Provider contract and the stored fragment

```go
// RecalledItem is one item a provider selected, in its order.
type RecalledItem struct {
    Ref        string // memory ID
    Label      string // exact label text without brackets; empty when none
    Assertion  string // digest of the full item, computed by NewRecalledItem
    Text       string // the selected prefix of the trimmed body, without any ellipsis
    ShownRunes int    // rune count of Text
    Complete   bool   // Text is the whole trimmed body
}

type BeforeChatResult struct {
    Items          []RecalledItem
    RetrievalMode  string
    FallbackReason string
}
```

Items are built only by `adapters.NewRecalledItem(ref, label, body string, shownRunes int) (RecalledItem, error)`, in
the `internal/memory/adapters` leaf package that providers and the keeper already share. The provider packs as today
and calls it per packed item while the full body is in hand, with the effective prefix length after `TrimSpace`. The
constructor validates the prefix against the body, sets `Text`, `ShownRunes` and `Complete`, computes
`Assertion = sha256(canonical JSON ["memoh.recall.v1", ref, label, trimmed full body])` from that body (never from an
imported `Hash`), and keeps no reference to the body: nothing downstream (cache, history, trace) holds more than the
selected fragment. Two items with the same selected prefix and different unshown suffixes get different assertions.
The rendered snippet is `Text`,
plus `...` when `!Complete`; a literal `...` inside a complete body stays text. A full-selection render reproduces
today's bytes exactly.

`ContextText`, `ResultCount` and `ResultRefs` go; counts and refs derive from `Items`. The context cache stores the
selection, copies the slice on `Set`/`Get`, and is bounded by bytes as well as entries. `before_memory_search` and
`after_memory_search` run once per recall, as today, with selection-based metrics; their resulting hook bytes are
frozen into the unit.

### 4.2 Coverage

`memory.Render` walks the typed view in order with a coverage state `ref → (assertion, shown_runes)`:

- a **summary** or **barrier** entry clears the whole state;
- for each unit, for each selected item `s` in order: if `state[s.Ref]` has `assertion == s.Assertion` and
  `shown_runes >= s.ShownRunes`, `s` is covered and omitted; otherwise `s` is emitted. Every emitted item sets
  `state[s.Ref] = (s.Assertion, s.ShownRunes)`, also when it replaces a different assertion for that ref;
- the unit renders the `<memory-context>` block with its emitted items (if any) followed by its hook bytes (if any);
  a unit with neither renders no message. Memory precedes requested skills, as today.

A complete showing covers any later fragment of the same item; equal fragments cover each other; a longer fragment is
shown; A→B→A shows A again because the state for the ref holds B; a changed label or canonicalized ID is a new
assertion; a fold that summarizes a newer assertion clears the state, so the current one is shown.

### 4.3 Storage and validation

On the request row, `runtime_context` holds, per kind:

```json
{"kind":"memory_recall","selection":{"items":[{"ref":"bot:42","label":"Ana, 2026-10-01","assertion":"…","text":"…","shown_runes":256,"complete":false}],"hook":"…"}}
{"kind":"requested_skills","message":{…}}
```

The selection is written with the first step commit, where persist-turn-context writes runtime context today, and is
stored even when the turn's own rendering emits nothing. It is immutable afterwards; a fork copying the row copies
exactly what the turn selected. Retry and edit replace or clear the copied value with the replacement's first write.
Its size is that of today's rendered recall plus about 100 bytes per item.

Validation has two levels. **Construction** (`NewRecalledItem`, body available) checks that `Text` is the effective
prefix of the body and that `Complete` matches, and computes the assertion; items are trusted and immutable from then
on. **Reload** (the codec, body gone) checks what the stored item can show, with rune-aware counts: nonempty ref;
valid UTF-8; `ShownRunes == runes(Text) > 0`; assertion well-formed, and equal to the recomputed digest when
`Complete`; hook valid UTF-8. It cannot verify an incomplete prefix against its unavailable body. A memory selection
that fails reload validation, wholly or in any item, becomes a **barrier**: it clears coverage and renders nothing. Such a unit is outside the guarantee; the
barrier only guarantees that it never lets an older showing cover a later one. Barriers are logged and counted.

### 4.4 The typed view and the one renderer

The **view** is built in this order, then rendered:

1. load checkpoint items and the tail (or `KeptBeforeTurn` + cutoff for a replacement), within the materialization
   budget (§4.6);
2. apply the existing preparation: admission, failure pruning, cutoffs, required-window recovery;
3. attach each request row's stored unit; an invalid one becomes a barrier at its position;
4. resolve the run's own unit: when the run has a fresh selection (new turn, retry, edit), it is the unit of the run's
   request, replacing any stored one; when it has none (Discuss head reuse, decision resume, recovery), the stored
   unit of its request is used. The own request is never dropped from the unit sequence just because it is reused.

`memory.Render(view)` is the only producer of native recall bytes. It runs:

1. **When a request is assembled** (`Resolve`, continuation, decision resume, steer, Discuss). The pre-turn estimate
   uses the unfiltered rendering of the own unit, an upper bound on the recall component only.
2. **After every in-run fold event**, once `compaction.Fold` has returned and before the runtime installs, measures
   and dispatches. This is a new runtime-owned splice operation that replaces recall projections and updates messages,
   provenance, the `from` mapping, `known`, dynamic references, cache breakpoints, initial and stable message counts
   and token estimates together. Nothing changes inside an event, so its numeric cut stays valid. If the re-rendered
   request exceeds the allowance, no ordinary request is dispatched; another fold event runs over the re-rendered
   request. An event that folds nothing ends with today's errors (`protected overflow`, `still over`).

**Termination.** Under a frozen view, every successful event strictly decreases `(raw durable sources, summaries,
summary tokens)` lexicographically: a raw fold removes raw sources; absorbing summaries reduces their count; a
single-summary rewrite keeps its position and must shrink (`fold.go:173-176`). Rendering only re-creates projections
for surviving units and never resurrects a removed source. An event that makes no progress ends with today's errors.
Rendered size is not monotone (a probe went from 150 to 1,287 tokens after re-render, then fit after a second event),
so tests assert the lexicographic measure per branch, including a successful summary-only rewrite.

**Failed events.** Passes committed before a failure stay committed. No ordinary request is sent from the local
splice; the run fails as today, and any later request reloads the committed checkpoint and renders anew. Live/reload
equality is asserted for successful events only.

Decision resume and recovery need nothing extra: the stored selection is the input and rendering is a function of
the view.

### 4.5 Sources and fold units (persist-turn-context rebase contract)

The rendered recall projection of a unit has the unit's request row as its source and forms one fold unit with the
request and its other runtime context:

- retention, read and summary boundaries never fall between a request and its projections: `retain`, `planFold`,
  `forkRead`, size caps and checkpoint commit all use the unit (#1700 today allows a boundary between consecutive user
  messages, `fold_plan.go:361-410`);
- a folded unit contributes its rendered projection to the summarizer read, as it stands in the request;
- the row-built summarizer reads projections with their rows (the candidate query returns metadata);
- the run's own projection is source-less until its first step commit, then is rebound to the request row (also when
  a steer is claimed at that commit);
- a unit that renders nothing still exists through its row, so a later render can produce its projection right after
  the row;
- fork copies carry the metadata and owners are remapped to the copies.

### 4.6 Accounting

- **Durable bytes:** the serialized selection (fragments, not bodies) per unit.
- **Materialization budget:** content plus serialized `runtime_context` over the whole materialization: checkpoint
  kept rows, tail, required-window rereads and row-built candidates. #1700's tail admission and aggregate count
  content only; the rebase ports persist-turn-context's `ContextBytes` rule to all of them. Kept units and the
  required window cannot be silently dropped: when they exceed the budget before decoding, the load returns
  incomplete and the request folds, or fails explicitly; truncation is carried through rereads. Units stay atomic.
- **Model estimate:** rendered messages only.
- **Provider bytes:** measured (§7).
- **Trace:** `SetMemoryRecall` is published after each final render (assembly and every event); per dispatch it
  reports selected, covered (ref and covering unit) and emitted, and barriers. Each fold pass reports the folded
  units' fragments that its capped summarizer request no longer contains. The context view reads the rendered
  request.

### 4.7 Entries and lifecycle

| Path | Behaviour |
| --- | --- |
| Web, IM, schedule | Recall; own unit is the fresh selection. |
| Retry, edit | Re-recall; the fresh selection replaces the copied one (first write replaces or clears it); view is `KeptBeforeTurn` + cutoff. |
| Native Discuss | No provider recall; head reuse renders the head's stored unit, if any; earlier batch rows render normally. |
| Decision resume, recovery | No new recall; the stored unit is rendered against the current view. |
| Steer | The unit stays on the original request row; steers carry none. If the original unit is folded, its recall leaves with it (§2, fold behaviour). |
| In-run fold | §4.4.2. |
| Chat fork (#1700) | Rows copied with metadata, owners remapped; refs unchanged (same bot). |
| Direct subagent-thread turn | Recall and render against its own history; the parent snapshot covers nothing. |
| Spawned subagent | No recall. |
| External runtimes | No dedup; full selection rendered into separate memory and hook sections as today. |

## 5. Alternatives rejected

- **Persisting the emitted subset (drafts 1 and 2).** A derived decision goes stale when the view changes; keeping it
  correct needed a separate selection store, restoration events with positions and cutoffs, commit cursors,
  reservations, opaque-emission barriers and fold-cursor fixes.
- **Persisting full bodies.** Adds nothing to rendering, amplifies storage up to four orders of magnitude for large
  bodies, and can make a tiny recall too large to admit for folding.
- **Re-rendering inside each fold pass, a selection-aware summarizer read, or a fragment-preserving cap.** Would
  restore summarizer exposure under clipping, at the cost of remapping the fold cursor, re-pricing reads inside the
  planner, or exempting memory text from the summarizer cap. The memory store re-supplies a lost fragment when a later
  retrieval selects it again (§2; not guaranteed), so transcript-level exposure does not justify coupling the fold engine to memory.
- **Provider-side exclusion with backfill.** Changes which items a turn shows depending on history and spends the
  bytes dedup saves (P3).
- **ID-only key; rendered-text digest; unordered shown set.** Fail on updates, on complete→partial and identical
  partials (120 items / 32,860 snippet bytes over 20 identical long-body turns), and on A→B→A.
- **Summaries as coverage; pinning covering rows in the fold engine.** Summaries are lossy; pinning couples the fold
  to memory and fails when the budget is tight.

## 6. Code touched (combined head)

- Memory: `internal/memory/adapters/{types.go,context_cache.go}`, `builtin/builtin.go`, the test implementations in
  §3; `internal/memory/adapters/recalled.go` (`NewRecalledItem`: construction validation and assertion);
  `internal/agent/internal/memory/{recall.go,render.go,selection.go}` (codec, reload validation, coverage).
- Storage and materialization: `context/history/runtime_context.go`, `internal/persist/step_commit.go` and
  `internal/chat/message` (selection with the first step, owner rebind), the transcript materializer and #1700's
  `service_history.go` / `service_history_prepare.go` (typed view, own-unit resolution, materialization budget),
  `history_checkpoints.sql` and the required-window query (metadata, bytes, incompleteness).
- Fold and runtime: `compaction/{fold_plan.go,fold_read.go}` (units), `application/service_context_switch.go`,
  `runtime/native/{context_switch.go,agent.go,types.go}` (event-end render operation, re-measure, refold), the
  continuation loader (`plan/continue.go`, `plan/assemble.go`).
- Trace and view: `context/fragment/lifecycle.go`, `runconfig/{resolve.go,prompt.go,runtime_context.go}`,
  `contextview`, `docs/agent-runtime.md`.

## 7. Acceptance

**Oracles.** Captured independently of production `Render` (a reference implementation in the test package):

- *Ordinary requests:* at every dispatch, record the frozen fresh selection and the ordered view/source manifest;
  the request's recall bytes equal the reference rendering; the ordinary guarantee holds per unit with eligibility
  relative to that unit's position; after commit, stored selections equal the recorded ones; provider request,
  reload (successful events) and context view agree.
- *Summarizer reads:* for every pass, record the uncapped read (source coverage) and the capped summarizer request
  bytes separately. Assert that each selected fragment of a folded unit has a showing in this or an earlier pass's
  uncapped read; measure and report which fragments survive the cap; assert that cap losses are counted in the trace.
  Availability in the surviving request is reported separately and is not evidence of summarizer exposure.

| Family | Cases |
| --- | --- |
| Identity | Graph and file A→B→A; partial B after full A; no-op update; same body under two IDs; legacy ID canonicalized on Update and Compact; Compact changing body or date; explicit and id-less file ingest; delete and recreate; stale or missing pgvector entries. |
| Packing and render | Same selected prefix with different unshown suffixes (different assertions, no second retrieval); all short, all long (>360 runes), mixed; complete→partial, partial→longer partial, identical partials; effective prefix shorter than the cut; labels on/off; multiline, Unicode, literal `...`; large unshown bodies (storage stays fragment-sized). Full-selection render byte-identical to today. |
| Validation | Missing fields, negative or oversized `shown_runes`, text/count mismatch, invalid UTF-8, mixed valid/invalid items: no panic, no invented snippet, barrier applied, A→invalid→A shows A. |
| Cache and hooks | Fresh/stale/fallback hits against different views; slice isolation; byte bound; hook denied; empty result with hook; all covered with and without hook. Hook runs once per recall; metrics unchanged. |
| View construction | Own-request override for new turn, retry, edit; Discuss head with a stored nonempty unit; decision resume; required-window recovery bringing an owner and its unit back; memory before skills; current input kept with zero recall projections; ownership rebind at first commit, with a steer claimed there. |
| Folds | Summarizer cap removing the only showing: covering unit folded (AnchorsCapped) and covering unit kept (AnchorsFirst), with the matched full-selection control; cap losses traced; pre-turn fold; step-0 fold from real envelope growth; later and repeated in-run folds re-rendering units after the first changed position (including before the new summary); two-pass event where pass 1 folds the covering unit and pass 2 the covered one (summarizer-read oracle); re-render overflow → second event → fit; no-op, error and successful one-summary-rewrite termination with the lexicographic progress assertion; pass committed then later failure or cancel (no ordinary dispatch; resume reloads); Generate and Stream; provenance, index and cache-breakpoint mappings after the event-end operation. |
| Persistence faults (real PostgreSQL) | First write fails or is interrupted; steer at first write; history written but queue commit fails; selection encode/decode failure; all-covered selection still stored; retry cancel and rollback; crossed with none/some/all covered. |
| Resources (real PostgreSQL) | Selection-heavy kept rows, tail and required window under the materialization budget; aggregate kept + tail + reread; an atomic unit too large to admit (explicit fold or failure, never a silent skip); storage, cache bytes and decode/hash allocations. |
| Entries and lifecycle | Web, IM, schedule; Discuss batches; decision resume of all-covered and partly covered turns whose covering showing was folded meanwhile; first and later steer; original request folded under the latest steer; direct and spawned subagent; external runtimes (memory-only, hook-only, both; trust and overflow unchanged). |
| Checkpoints and forks (real PostgreSQL) | `[kept A, summary, kept B] + tail C`, each with a unit; units never split; live request equals reload after every successful event; retry and edit on `KeptBeforeTurn`; fork before and after a fold; retry of a fork copy. |

**Cache gates.** The 8 prefix scenarios × 3 clients keep their expected breaks (memory-file update, workspace
instructions, pictures, compaction) and add none; unchanged transitions stay at 100%. For a fold with kept rows
before its summary, the serialized longest common prefix extends to the first changed position. Extra events are
counted and their cost reported.

**Measurement.** 20-turn fixture, small and large tool outputs; recall profiles identical / 50% rotating / no overlap,
crossed with complete / partial / mixed packing; a rollback workload and a forced-fold workload. Compare the rebased
persistence head with the dedup head on the same base. Report cumulative serialized prompt bytes, non-reusable bytes,
recall bytes and items shown (complete vs partial), final request size, durable and materialized bytes, barriers,
re-rendered units and extra events per fold. Pass: no overlap is byte-equal on equal views; identical recall shows
each item once per summary window in every packing profile; no new prefix breaks.

**Runtime and UI.** On the combined head, `mise run dev` with the builtin provider and a stub chat provider capturing
request bodies. In the Web UI: three related questions (later turns carry only new or changed memory lines, in
captured bodies and the context view); a long task that folds after earlier recalls (fragments whose showing was
folded reappear in their units; reload equals the live request); a decision and a steer run. Captured requests,
stored selections and the context view are checked together, with current screenshots. Agent verification is not
Human QA.

## 8. Landing

Dedup lands as commits on `feat/persist-turn-context` after it is rebased onto the integrated head, in the same PR,
so persistence never ships without dedup and the stored format ships once. It depends on §4.5 and §4.6, which the
persistence rebase must deliver (`CONFLICTS.md` §6 states the same contract), and on the event-end render operation
in the native runtime (§4.4). Acceptance runs on the combined head only.

## 9. Open questions

1. Whether the label date belongs to the assertion (proposed: yes; a date change shows the item again).
2. Whether to store a coverage-algorithm version with selections. Changing the algorithm re-renders every unit once,
   a one-time prefix break on deploy; proposed: no version, accept the one-time break.
