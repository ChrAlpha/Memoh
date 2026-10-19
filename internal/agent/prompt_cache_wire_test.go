package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/felinics/memoh/internal/models"
)

// wireStep is one scripted model reply: a tool call, or plain text.
type wireStep struct {
	// Tool and Args make the reply a call of that tool; Args is its JSON.
	Tool string
	Args string
	// Text is the reply when Tool is empty.
	Text string
}

// wireRequest is one request body the model endpoint received, as the bytes
// the real client put on the wire.
type wireRequest struct {
	// Turn is the label the test had set when the request arrived.
	Turn string
	// Step counts the requests of that turn, from 1.
	Step int
	// Summaries is how many compaction summaries the endpoint had served when
	// the request arrived.
	Summaries int
	Body      []byte
}

// wireProvider is a model endpoint speaking the three streaming protocols the
// agent's clients use. It keeps every request body untouched and answers each
// with the next scripted step, or a short text once the script is spent.
type wireProvider struct {
	server *httptest.Server

	mu       sync.Mutex
	steps    []wireStep
	turn     string
	turnStep int
	requests []wireRequest
	calls    int
	// summaries counts the non-streamed requests, which are the compaction
	// service's summaries and not part of the session's request sequence.
	summaries int
}

func newWireProvider(t *testing.T) *wireProvider {
	t.Helper()
	w := &wireProvider{}
	w.server = httptest.NewServer(http.HandlerFunc(w.serve))
	t.Cleanup(w.server.Close)
	return w
}

// URL is the base URL of a provider row pointing at this endpoint.
func (w *wireProvider) URL() string { return w.server.URL }

// script queues the replies of the requests that follow.
func (w *wireProvider) script(steps ...wireStep) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.steps = append(w.steps, steps...)
}

// beginTurn labels the requests that arrive from now on.
func (w *wireProvider) beginTurn(label string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.turn, w.turnStep = label, 0
}

// recorded returns the request bodies received so far.
func (w *wireProvider) recorded() []wireRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]wireRequest(nil), w.requests...)
}

func (w *wireProvider) serve(rw http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var probe struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		http.Error(rw, "unexpected request", http.StatusBadRequest)
		return
	}
	if !probe.Stream {
		w.serveSummary(rw, r)
		return
	}
	w.mu.Lock()
	w.turnStep++
	w.calls++
	n := w.calls
	w.requests = append(w.requests, wireRequest{Turn: w.turn, Step: w.turnStep, Summaries: w.summaries, Body: body})
	step := wireStep{Text: "ok"}
	if len(w.steps) > 0 {
		step, w.steps = w.steps[0], w.steps[1:]
	}
	w.mu.Unlock()

	rw.Header().Set("Content-Type", "text/event-stream")
	rw.WriteHeader(http.StatusOK)
	callID := fmt.Sprintf("call_%d", n)
	switch {
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		writeCompletionsStream(rw, callID, step)
	case strings.HasSuffix(r.URL.Path, "/responses"):
		writeResponsesStream(rw, callID, step)
	case strings.HasSuffix(r.URL.Path, "/messages"):
		writeAnthropicStream(rw, callID, step)
	default:
		http.Error(rw, "unknown path "+r.URL.Path, http.StatusNotFound)
		return
	}
	if flusher, ok := rw.(http.Flusher); ok {
		flusher.Flush()
	}
}

// serveSummary answers the compaction service's summary request. A run's own
// model calls are streamed, so a request that is not belongs to no turn and
// stays out of the recorded sequence.
func (w *wireProvider) serveSummary(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	w.summaries++
	text := fmt.Sprintf("SUMMARY-%d %s", w.summaries, strings.Repeat("recap ", 200))
	w.mu.Unlock()

	rw.Header().Set("Content-Type", "application/json")
	var reply map[string]any
	switch {
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		reply = map[string]any{
			"id": "c", "object": "chat.completion", "model": "m",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": text}}},
		}
	case strings.HasSuffix(r.URL.Path, "/responses"):
		reply = map[string]any{
			"id": "resp_1", "created_at": 1700000000, "model": "m",
			"output": []map[string]any{{"type": "message", "id": "msg_1", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}}}},
			"usage":  map[string]any{"input_tokens": 10, "output_tokens": 5},
		}
	case strings.HasSuffix(r.URL.Path, "/messages"):
		reply = map[string]any{
			"id": "msg_1", "type": "message", "model": "m", "role": "assistant",
			"content":     []map[string]any{{"type": "text", "text": text}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
		}
	default:
		http.Error(rw, "unknown path "+r.URL.Path, http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(rw).Encode(reply)
}

func sseData(rw io.Writer, event, data string) {
	if event != "" {
		_, _ = fmt.Fprintf(rw, "event: %s\n", event)
	}
	_, _ = fmt.Fprintf(rw, "data: %s\n\n", data)
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func writeCompletionsStream(rw io.Writer, callID string, step wireStep) {
	if step.Tool != "" {
		sseData(rw, "", fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":null}]}`, callID, step.Tool, jsonString(step.Args)))
		sseData(rw, "", `{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	} else {
		sseData(rw, "", fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":%s},"finish_reason":null}]}`, jsonString(step.Text)))
		sseData(rw, "", `{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}
	sseData(rw, "", "[DONE]")
}

func writeResponsesStream(rw io.Writer, callID string, step wireStep) {
	sseData(rw, "response.created", `{"type":"response.created","response":{"id":"resp_1","created_at":1700000000,"model":"m"}}`)
	if step.Tool != "" {
		item := fmt.Sprintf(`{"type":"function_call","id":"fc_%s","call_id":%q,"name":%q`, callID, callID, step.Tool)
		sseData(rw, "response.output_item.added", fmt.Sprintf(`{"type":"response.output_item.added","output_index":0,"item":%s}}`, item))
		sseData(rw, "response.function_call_arguments.delta", fmt.Sprintf(`{"type":"response.function_call_arguments.delta","item_id":"fc_%s","output_index":0,"delta":%s}`, callID, jsonString(step.Args)))
		sseData(rw, "response.output_item.done", fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":%s,"arguments":%s}}`, item, jsonString(step.Args)))
	} else {
		sseData(rw, "response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`)
		sseData(rw, "response.output_text.delta", fmt.Sprintf(`{"type":"response.output_text.delta","item_id":"msg_1","delta":%s}`, jsonString(step.Text)))
		sseData(rw, "response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1"}}`)
	}
	sseData(rw, "response.completed", `{"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5}}}`)
}

func writeAnthropicStream(rw io.Writer, callID string, step wireStep) {
	sseData(rw, "message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","model":"m","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`)
	if step.Tool != "" {
		sseData(rw, "content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":%q,"name":%q}}`, callID, step.Tool))
		sseData(rw, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%s}}`, jsonString(step.Args)))
		sseData(rw, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		sseData(rw, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`)
	} else {
		sseData(rw, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		sseData(rw, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, jsonString(step.Text)))
		sseData(rw, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		sseData(rw, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`)
	}
	sseData(rw, "message_stop", `{"type":"message_stop"}`)
}

// promptCacheClientTypes are the client types the benchmark measures. The
// OpenAI Responses one is what dev models use.
var promptCacheClientTypes = []models.ClientType{
	models.ClientTypeOpenAIResponses,
	models.ClientTypeOpenAICompletions,
	models.ClientTypeAnthropicMessages,
}
