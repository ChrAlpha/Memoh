package main

import (
	"fmt"
	"strings"
	"testing"
)

// An input larger than the window cannot be sent: its turn fails with an
// explicit code, and the turns after it answer. A Discuss session keeps the
// input it could not answer in its history; the next turn folds it, by its
// head and tail, into the summary it renders.
func TestPostgresAnOversizedInputFailsOnlyItsOwnTurn(t *testing.T) {
	for _, name := range []string{"web", "discuss"} {
		t.Run(name, func(t *testing.T) {
			entry := runEntryNamed(name)
			s := newEffectLoop(t, entry)
			if code, _ := s.run(entry, "EARLIER_TURN_KEEP", 0); code != "" {
				t.Fatalf("earlier turn failed with %s", code)
			}
			const payload = "HUGE_PAYLOAD "
			if code, _ := s.run(entry, payload+strings.Repeat("o", 40000), 0); code == "" {
				t.Fatal("the oversized turn answered, want it to fail with an explicit code")
			}
			for i := range 2 {
				code, requests := s.run(entry, fmt.Sprintf("NEXT_TURN_%d", i), 0)
				if code != "" || len(requests) != 1 || strings.Contains(requests[0], payload) {
					t.Fatalf("turn %d after the oversized input: code=%q provider_calls=%d, want one call without the payload", i, code, len(requests))
				}
				if name == "discuss" && len(summaryContents(t, requests[0])) != 1 {
					t.Fatalf("turn %d after the oversized input renders no summary, want the input folded", i)
				}
			}
		})
	}
}
