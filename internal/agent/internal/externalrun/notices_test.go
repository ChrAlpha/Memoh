package externalrun

import (
	"testing"

	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/apperror"
)

// Each notice kind a runtime reports leaves the application with its public
// code, and the kind itself does not.
func TestPublicRuntimeNoticeGivesEachKindItsCode(t *testing.T) {
	for kind, code := range map[event.NoticeKind]apperror.Code{
		event.NoticeNativeHistoryLost:   apperror.CodeRuntimeNativeHistoryLost,
		event.NoticeToolsUnavailable:    apperror.CodeRuntimeToolsUnavailable,
		event.NoticeElicitationDeclined: apperror.CodeRuntimeElicitationDeclined,
		event.NoticeSteerFailed:         apperror.CodeRuntimeControlSteerFailed,
	} {
		t.Run(string(kind), func(t *testing.T) {
			got := publicRuntimeNotice(event.StreamEvent{Type: event.RuntimeNotice, NoticeKind: kind, Delta: "runtime text"})
			if got.Code != string(code) || got.Delta != "runtime text" || got.NoticeKind != "" {
				t.Fatalf("notice = %+v, want code %s with the runtime's text", got, code)
			}
		})
	}
}

// A failed steer carries no text of its own; it takes the catalog detail.
func TestPublicRuntimeNoticeFillsSteerFailedDetail(t *testing.T) {
	got := publicRuntimeNotice(event.StreamEvent{Type: event.RuntimeNotice, NoticeKind: event.NoticeSteerFailed})
	want := "The additional instruction could not be delivered. Send it again after this turn finishes."
	if got.Code != string(apperror.CodeRuntimeControlSteerFailed) || got.Delta != want {
		t.Fatalf("notice = %+v, want %q", got, want)
	}
}

func TestPublicRuntimeNoticeLeavesOtherEvents(t *testing.T) {
	for _, ev := range []event.StreamEvent{
		{Type: event.TextDelta, Delta: "hello"},
		{Type: event.RuntimeNotice, Code: "tools_unavailable", Delta: "already public"},
		{Type: event.RuntimeNotice, NoticeKind: "unknown_kind", Delta: "text"},
	} {
		if got := publicRuntimeNotice(ev); got.Code != ev.Code || got.Delta != ev.Delta || got.NoticeKind != ev.NoticeKind {
			t.Fatalf("publicRuntimeNotice(%+v) = %+v, want it unchanged", ev, got)
		}
	}
}
