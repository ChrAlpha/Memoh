package agent

import (
	"testing"
)

// A turn interrupted by a graceful shutdown resumes after restart unless its
// history was cleared in between: the clear takes the resume intent with the
// history it refers to. A turn started after the clear resumes as usual.
func TestPostgresHistoryResetRetiresShutdownResume(t *testing.T) {
	for _, scope := range []historyResetScope{historyResetSession, historyResetBot} {
		t.Run(string(scope), func(t *testing.T) {
			h := newWSStepHistoryHarness(t, wsStepHistorySuccess)

			cleared := h.interruptedTurn(t, h.startProcess(t), "cleared task")
			next := h.startProcess(t)
			h.manager = next.manager
			h.clearHistory(t, scope, nil)
			if hasResumeIntent(t, h, cleared) {
				t.Error("clearing history kept the interrupted turn's resume intent")
			}
			if h.resumeScan(t, next, cleared) {
				t.Fatal("resume worker continued a turn whose history was cleared")
			}

			kept := h.interruptedTurn(t, next, "task after the clear")
			if !h.resumeScan(t, h.startProcess(t), kept) {
				t.Fatal("resume worker did not continue a turn started after the clear")
			}
		})
	}
}
