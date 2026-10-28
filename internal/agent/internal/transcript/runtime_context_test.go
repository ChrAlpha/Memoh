package transcript

import (
	"reflect"
	"testing"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// A request row replays the runtime context its request carried right after
// itself, as the request sent it; the run's own request stays bare, since the
// run supplies its context anew.
func TestInjectRuntimeContextRecordsReplaysWhatTheRequestCarried(t *testing.T) {
	t.Parallel()

	recall := runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent("<memory-context>\n- the user likes tea\n</memory-context>")}
	skills := runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent("<requested_skill name=\"brew\">steep five minutes</requested_skill>")}
	metadata, err := historyfrag.RuntimeContextMetadata([]historyfrag.RuntimeContext{
		{Kind: messagepkg.RuntimeContextMemoryRecall, Message: recall},
		{Kind: messagepkg.RuntimeContextRequestedSkills, Message: skills},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := func(id, text string) historyfrag.HistoryRecord {
		return historyfrag.HistoryRecord{
			DBMessageID:  id,
			ModelMessage: runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent(text)},
			Metadata:     metadata,
		}
	}
	answer := historyfrag.HistoryRecord{DBMessageID: "answer", ModelMessage: runreq.ModelMessage{Role: "assistant", Content: runreq.NewTextContent("tea")}}
	records := []historyfrag.HistoryRecord{request("earlier", "what do I like?"), answer, request("current", "and to eat?")}

	got := injectRuntimeContextRecords(records, "current")
	texts := make([]string, len(got))
	for i, record := range got {
		texts[i] = record.ModelMessage.TextContent()
	}
	want := []string{"what do I like?", recall.TextContent(), skills.TextContent(), "tea", "and to eat?"}
	if !reflect.DeepEqual(texts, want) {
		t.Fatalf("history = %q, want %q", texts, want)
	}
	if !got[1].Synthetic || got[1].Metadata[messagepkg.RuntimeContextMetadataKey] != messagepkg.RuntimeContextMemoryRecall {
		t.Fatalf("replayed recall = %+v, want a synthetic memory recall record", got[1])
	}
	// The replay sends the bytes the request sent.
	sent := runreq.ModelMessagesToSDKMessages([]runreq.ModelMessage{recall})
	replayed := runreq.ModelMessagesToSDKMessages([]runreq.ModelMessage{got[1].ModelMessage})
	if !reflect.DeepEqual(replayed, sent) {
		t.Fatalf("replayed %#v, the request sent %#v", replayed, sent)
	}
	if bare := injectRuntimeContextRecords([]historyfrag.HistoryRecord{answer}, ""); len(bare) != 1 {
		t.Fatalf("a history without runtime context changed: %+v", bare)
	}
}
