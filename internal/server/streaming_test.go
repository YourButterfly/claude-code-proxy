package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newStreamingUpstream starts a fake OpenAI-format upstream that answers a
// streaming request with the given SSE payload.
func newStreamingUpstream(t *testing.T, sse string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const streamRequestBody = `{"model":"claude-sonnet-4-20250514","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`

// sseEvents splits a raw SSE body into (event, data) pairs.
func sseEvents(t *testing.T, body string) []struct{ event, data string } {
	t.Helper()
	var out []struct{ event, data string }
	var curEvent, curData string
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			curEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			curData = strings.TrimPrefix(line, "data: ")
		case line == "":
			if curEvent != "" || curData != "" {
				out = append(out, struct{ event, data string }{curEvent, curData})
			}
			curEvent, curData = "", ""
		}
	}
	return out
}

// TestStreamingForwardsContentAndFinishesCleanly pins the streaming regression:
// a healthy upstream stream must produce text deltas and end with message_stop,
// with no trailing error event.
//
// The bug this catches: the upstream response body was closed (defer inside the
// handler scope) before Fiber ran the body stream writer, so every streaming
// request yielded an empty message followed by
// "stream read error: http: read on closed response body".
func TestStreamingForwardsContentAndFinishesCleanly(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`,
		``,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		``,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`,
		``,
		`data: [DONE]`,
		``,
		``,
	}, "\n")

	upstream := newStreamingUpstream(t, sse)
	app := newTestApp(newTestConfig(upstream.URL))

	resp := postMessages(t, app, streamRequestBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)

	var text strings.Builder
	var sawStop bool
	for _, ev := range sseEvents(t, body) {
		switch ev.event {
		case "error":
			t.Fatalf("stream reported an error event: %s", ev.data)
		case "content_block_delta":
			var payload struct {
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
				t.Fatalf("bad content_block_delta payload %q: %v", ev.data, err)
			}
			if payload.Delta.Type == "text_delta" {
				text.WriteString(payload.Delta.Text)
			}
		case "message_stop":
			sawStop = true
		}
	}

	if got := text.String(); got != "Hello" {
		t.Errorf("streamed text = %q, want %q (raw body: %s)", got, "Hello", body)
	}
	if !sawStop {
		t.Errorf("stream never sent message_stop (raw body: %s)", body)
	}
}
