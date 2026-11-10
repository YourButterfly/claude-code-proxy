package server

import (
	"bufio"
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/pkg/models"
)

// The regression these tests pin:
//
// The streaming upstream client used to be `&http.Client{Timeout: 300s}`.
// http.Client.Timeout is a deadline for the entire exchange INCLUDING body
// reads, so it silently capped generation length rather than detecting faults.
// Reproduced locally against an upstream emitting one SSE chunk every 2s: the
// stream died at exactly 300.0s with
// "context deadline exceeded (Client.Timeout ... while reading body)" and
// quiet_for=0.8s — connection healthy, generation still in progress. Production
// runs showed the same signature: three calls ended at 300.4-300.5s with zero
// tokens and no error text on the client beyond "stream read error", while
// successful generation calls took 345s, 395s, 552s and 564s.
//
// The fix splits the two failure modes: headers must arrive within
// streamingResponseHeaderTimeout, and the body must not go silent for longer
// than streamIdleTimeout. Neither bounds total elapsed time.

// sseChunk is one well-formed OpenAI streaming chunk carrying a text delta.
const sseChunk = `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"x"}}]}` + "\n\n"

// streamingRequest builds a minimal OpenAI-format streaming request against the
// configured model.
func streamingRequest(cfg *config.Config) *models.OpenAIRequest {
	stream := true
	return &models.OpenAIRequest{
		Model:    cfg.OpusModel,
		Stream:   &stream,
		Messages: []models.OpenAIMessage{{Role: "user", Content: "hi"}},
	}
}

// TestSlowButHealthyStreamIsNotCutOff is the core regression: a stream that
// keeps producing data far past the old 300s wall must run to completion.
//
// The old client would have severed this at 300s. The test uses a short idle
// window instead of waiting minutes: the point is that reads keep arriving, so
// the idle timer never fires no matter how long the whole stream takes.
func TestSlowButHealthyStreamIsNotCutOff(t *testing.T) {
	const chunks = 6
	const interval = 40 * time.Millisecond

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("upstream response writer is not a Flusher")
			return
		}
		for i := 0; i < chunks; i++ {
			_, _ = w.Write([]byte(sseChunk))
			flusher.Flush()
			time.Sleep(interval)
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig(upstream.URL)
	resp, err := callOpenAIStream(streamingRequest(cfg), cfg)
	if err != nil {
		t.Fatalf("callOpenAIStream: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	// An idle window shorter than the inter-chunk interval would fail this, so
	// pick one comfortably longer: reads keep resetting the timer.
	idle := interval * 8
	var out bytes.Buffer
	w := bufio.NewWriter(&out)
	streamOpenAIToClaudeWithIdle(w, resp.Body, "test-model", cfg, time.Now(), idle)
	_ = w.Flush()

	body := out.String()
	if strings.Contains(body, "stream read error") {
		t.Fatalf("healthy slow stream was cut off: %s", body)
	}
	if got := strings.Count(body, `"text_delta"`); got != chunks {
		t.Fatalf("forwarded %d of %d text deltas; body: %s", got, chunks, body)
	}
	if !strings.Contains(body, "message_stop") {
		t.Errorf("stream never finished: %s", body)
	}
}

// TestStalledStreamFailsFastAndSaysWhy covers the failure the idle timer exists
// for: the upstream sends headers and one chunk, then goes silent forever. The
// client must get an error promptly that names the real cause.
//
// Before the fix this case was indistinguishable from a long generation — both
// produced "context deadline exceeded" after a fixed 300s.
func TestStalledStreamFailsFastAndSaysWhy(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			_, _ = w.Write([]byte(sseChunk))
			flusher.Flush()
		}
		<-release // never sends another byte
	}))
	t.Cleanup(func() { close(release); upstream.Close() })

	cfg := newTestConfig(upstream.URL)
	resp, err := callOpenAIStream(streamingRequest(cfg), cfg)
	if err != nil {
		t.Fatalf("callOpenAIStream: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	idle := 80 * time.Millisecond
	var out bytes.Buffer
	w := bufio.NewWriter(&out)

	start := time.Now()
	streamOpenAIToClaudeWithIdle(w, resp.Body, "test-model", cfg, start, idle)
	_ = w.Flush()
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("stall was not detected promptly (took %s)", elapsed)
	}
	body := out.String()
	if !strings.Contains(body, "stream read error") {
		t.Fatalf("stalled stream produced no error event; body: %s", body)
	}
	// The message must name the idle timeout, not a generic deadline: the whole
	// point is telling "upstream went quiet" apart from "proxy gave up".
	if !strings.Contains(body, "idle timeout") {
		t.Errorf("error does not explain the idle timeout; body: %s", body)
	}
}

// TestStreamingClientDoesNotCapTotalDuration guards the fix at the source: the
// client must not carry an overall Timeout, because that field is what capped
// body reads. A future "let's add a timeout for safety" edit breaks this test
// instead of production.
func TestStreamingClientDoesNotCapTotalDuration(t *testing.T) {
	client := newStreamingClient()
	if client.Timeout != 0 {
		t.Fatalf(
			"streaming client Timeout = %s; it must be 0 (it would cap generation length). "+
				"Bound headers and idle separately instead.",
			client.Timeout,
		)
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("streaming client Transport = %T, want *http.Transport", client.Transport)
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("ResponseHeaderTimeout must be set, or a dead upstream hangs forever")
	}
}
