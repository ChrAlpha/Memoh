package application

import (
	"testing"

	"github.com/felinics/memoh/internal/agent/runreq"
)

func TestQueuePayloadTextNeverExposesCommandEnvelope(t *testing.T) {
	for _, tc := range []struct{ payload, want string }{
		{`{"text":"hello"}`, "hello"},
		{`{"text":"", "command":{"Token":"PRIVATE","ChatToken":"PRIVATE","Query":""}}`, ""},
		{`{"command":{"Token":"PRIVATE","UserVisibleText":"display","Query":"wrapped"}}`, "display"},
		{`{"command":{"Token":"PRIVATE","Query":"query"}}`, "query"},
		{`{"command":{"Token":"PRIVATE"`, ""},
		{`unversioned raw text`, ""},
	} {
		if got := runreq.QueuePayloadText([]byte(tc.payload)); got != tc.want {
			t.Errorf("visible text=%q, want %q", got, tc.want)
		}
	}
}
