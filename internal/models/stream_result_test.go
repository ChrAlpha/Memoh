package models

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
)

const testSilence = 200 * time.Millisecond

// anthropicServer answers with an Anthropic message stream: what before
// writes, then the summary text.
func anthropicServer(t *testing.T, before func(w http.ResponseWriter, r *http.Request, emit func(event, data string)) bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server notices the client leave only once the body is read.
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		emit := func(event, data string) {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
			flusher.Flush()
		}
		if !before(w, r, emit) {
			return
		}
		emit("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
		emit("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"summary"}}`)
		emit("content_block_stop", `{"type":"content_block_stop","index":1}`)
		emit("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}`)
		emit("message_stop", `{"type":"message_stop"}`)
	}))
	t.Cleanup(server.Close)
	return server
}

func messageStart(emit func(event, data string)) {
	emit("message_start", `{"type":"message_start","message":{"id":"msg","type":"message","model":"m","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`)
}

func streamSummary(t *testing.T, server *httptest.Server) (sdk.ModelResult, error) {
	t.Helper()
	model := NewSDKChatModel(SDKModelConfig{ClientType: string(ClientTypeAnthropicMessages), ModelID: "m", BaseURL: server.URL, HTTPClient: server.Client()})
	return StreamResult(t.Context(), model, sdk.Request{Messages: []sdk.Message{sdk.UserMessage("summarize")}}, testSilence)
}

// Anything the provider sends renews the silence window, including the
// keepalive frames the SDK drops before they become stream parts.
func TestStreamResultOutlivesItsSilenceWhileTheProviderSends(t *testing.T) {
	t.Parallel()

	frames := map[string]func(w http.ResponseWriter, emit func(event, data string)){
		"text": func(_ http.ResponseWriter, emit func(event, data string)) {
			emit("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"draft "}}`)
		},
		"reasoning": func(_ http.ResponseWriter, emit func(event, data string)) {
			emit("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"thinking "}}`)
		},
		"ping": func(_ http.ResponseWriter, emit func(event, data string)) {
			emit("ping", `{"type":"ping"}`)
		},
		"sse_comment": func(w http.ResponseWriter, _ func(event, data string)) {
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			w.(http.Flusher).Flush()
		},
	}
	for name, frame := range frames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := anthropicServer(t, func(w http.ResponseWriter, r *http.Request, emit func(event, data string)) bool {
				messageStart(emit)
				switch name {
				case "text":
					emit("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
				case "reasoning":
					emit("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
				}
				// Three silence windows of frames, five to a window.
				for range 15 {
					select {
					case <-r.Context().Done():
						return false
					case <-time.After(testSilence / 5):
					}
					frame(w, emit)
				}
				if name == "text" || name == "reasoning" {
					emit("content_block_stop", `{"type":"content_block_stop","index":0}`)
				}
				return true
			})
			result, err := streamSummary(t, server)
			if err != nil {
				t.Fatalf("a provider sending %s frames was cut off: %v", name, err)
			}
			if result.FinishReason != sdk.FinishReasonStop || result.Text == "" {
				t.Fatalf("result = %q finish=%s, want the completed summary", result.Text, result.FinishReason)
			}
		})
	}
}

// A provider that stops sending is cut off one silence window after it last
// sent, as an incomplete stream a retry may ask again; one that never answers
// is cut off the same way, without a deadline on its headers.
func TestStreamResultCutsOffASilentProvider(t *testing.T) {
	t.Parallel()

	for name, before := range map[string]func(w http.ResponseWriter, r *http.Request, emit func(event, data string)) bool{
		"after_headers": func(_ http.ResponseWriter, r *http.Request, emit func(event, data string)) bool {
			messageStart(emit)
			<-r.Context().Done()
			return false
		},
		"before_headers": func(_ http.ResponseWriter, r *http.Request, _ func(event, data string)) bool {
			// The handler has written nothing, so no header has been sent.
			<-r.Context().Done()
			return false
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := anthropicServer(t, before)
			started := time.Now()
			_, err := streamSummary(t, server)
			if !errors.Is(err, ErrStreamSilent) || !errors.Is(err, sdk.ErrStreamIncomplete) {
				t.Fatalf("err = %v, want a silent stream cut off as incomplete", err)
			}
			if elapsed := time.Since(started); elapsed > 5*testSilence {
				t.Fatalf("silent provider held the call for %s, want about %s", elapsed, testSilence)
			}
		})
	}
}

// errorThenSilence reports a provider error and then keeps its stream open
// without another part.
type errorThenSilence struct{ sdk.Provider }

func (errorThenSilence) DoStream(ctx context.Context, _ sdk.Request) (<-chan sdk.StreamPart, error) {
	parts := make(chan sdk.StreamPart, 1)
	parts <- &sdk.ErrorPart{Error: &sdk.APIError{Provider: "test", StatusCode: http.StatusBadRequest, Kind: sdk.KindUnknown}}
	go func() {
		<-ctx.Done()
		close(parts)
	}()
	return parts, nil
}

// An answer the provider gave keeps its own error when silence follows it,
// so a rejected request is not retried as a cut-off one.
func TestStreamResultKeepsTheProvidersAnswerOverTheSilenceAfterIt(t *testing.T) {
	t.Parallel()

	model := &sdk.Model{ID: "m", Provider: errorThenSilence{}}
	_, err := StreamResult(t.Context(), model, sdk.Request{Messages: []sdk.Message{sdk.UserMessage("summarize")}}, testSilence)
	var answer *sdk.APIError
	if !errors.As(err, &answer) || answer.StatusCode != http.StatusBadRequest {
		t.Fatalf("err = %v, want the provider's 400", err)
	}
	if errors.Is(err, sdk.ErrStreamIncomplete) || errors.Is(err, ErrStreamSilent) {
		t.Fatalf("err = %v, want it not reported as a cut-off stream", err)
	}
}
