package timeline

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
)

// Leaving out what CoveredTurnResponseIDs names must not change the composed
// context, whatever the coverage shape: non-prefix coverage, several
// artifacts, unusable artifacts, and responses sharing a millisecond with
// the anchor. Artifacts hold the projection's invariant that AnchorEndMs is
// the creation time of the last covered source, sources sorted by time.
func TestCoveredTurnResponsesComposeIdentically(t *testing.T) {
	rng := rand.New(rand.NewPCG(247, 8)) //nolint:gosec // a fixed seed keeps the explored shapes reproducible
	for iteration := range 5000 {
		rc, trs, artifacts := randomCompactedTimeline(rng)
		covered := CoveredTurnResponseIDs(artifacts)
		loaded := slices.DeleteFunc(slices.Clone(trs), func(tr TurnResponseEntry) bool {
			return slices.Contains(covered, tr.SourceMessageID)
		})
		want := ComposeContextWithArtifacts(rc, trs, artifacts)
		got := ComposeContextWithArtifacts(rc, loaded, artifacts)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d: leaving out %v changed the context\nwant %q\ngot  %q",
				iteration, covered, composedTexts(want), composedTexts(got))
		}
	}
}

func TestCoveredTurnResponseIDsKeepsTheAnchorSlot(t *testing.T) {
	artifacts := []CompactionArtifact{
		{ID: "blank", Summary: " ", AnchorEndMs: 900, Sources: []CompactionSource{{HistoryMessageID: "h0", CreatedAtMs: 100}}},
		{ID: "a1", Summary: "s1", AnchorEndMs: 300, Sources: []CompactionSource{
			{HistoryMessageID: "h1", CreatedAtMs: 100},
			{ExternalMessageID: "m1", CreatedAtMs: 200},
			{HistoryMessageID: "h2", CreatedAtMs: 300},
		}},
		{ID: "a2", Summary: "s2", AnchorEndMs: 500, Sources: []CompactionSource{
			{HistoryMessageID: "h1", CreatedAtMs: 100},
			{HistoryMessageID: "h3", CreatedAtMs: 400},
			{HistoryMessageID: "h4", CreatedAtMs: 500},
		}},
	}
	got := CoveredTurnResponseIDs(artifacts)
	if want := []string{"h1", "h3"}; !slices.Equal(got, want) {
		t.Fatalf("covered = %v, want %v: an unusable artifact replaces nothing and anchor-time sources stay loaded", got, want)
	}
}

func randomCompactedTimeline(rng *rand.Rand) (RenderedContext, []TurnResponseEntry, []CompactionArtifact) {
	// A narrow time range makes millisecond ties common.
	at := func() int64 { return 1 + rng.Int64N(12) }
	var rc RenderedContext
	for i := range rng.IntN(6) {
		rc = append(rc, textSegment(fmt.Sprintf("m%d", i), at(), fmt.Sprintf("rc-%d", i)))
	}
	var trs []TurnResponseEntry
	for i := range rng.IntN(10) {
		tr := assistantTR(at(), fmt.Sprintf("tr-%d", i))
		tr.SourceMessageID = fmt.Sprintf("h%d", i)
		trs = append(trs, tr)
	}
	slices.SortStableFunc(trs, func(a, b TurnResponseEntry) int { return int(a.RequestedAtMs - b.RequestedAtMs) })

	artifacts := make([]CompactionArtifact, rng.IntN(4))
	for i := range artifacts {
		var sources []CompactionSource
		for _, segment := range rc {
			if rng.IntN(3) == 0 {
				sources = append(sources, CompactionSource{ExternalMessageID: segment.MessageID, CreatedAtMs: segment.ReceivedAtMs})
			}
		}
		for _, tr := range trs {
			if rng.IntN(2) == 0 {
				sources = append(sources, CompactionSource{HistoryMessageID: tr.SourceMessageID, CreatedAtMs: tr.RequestedAtMs})
			}
		}
		// A covered response may also lie outside the loaded window.
		if rng.IntN(3) == 0 {
			sources = append(sources, CompactionSource{HistoryMessageID: fmt.Sprintf("unloaded-%d", i), CreatedAtMs: at()})
		}
		slices.SortStableFunc(sources, func(a, b CompactionSource) int { return int(a.CreatedAtMs - b.CreatedAtMs) })
		artifact := CompactionArtifact{ID: fmt.Sprintf("a%d", i), Summary: fmt.Sprintf("summary-%d", i), Sources: sources}
		if len(sources) > 0 {
			artifact.AnchorEndMs = sources[len(sources)-1].CreatedAtMs
		}
		if rng.IntN(5) == 0 {
			artifact.Summary = ""
		}
		artifacts[i] = artifact
	}
	return rc, trs, artifacts
}

func composedTexts(result *ComposeContextResult) []string {
	if result == nil {
		return nil
	}
	return messageTexts(result.Messages)
}
