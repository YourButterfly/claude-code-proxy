package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/claude-code-proxy/proxy/internal/config"
)

// upstreamQuotaBody is the exact rejection an OpenAI-format gateway returns when
// the account is out of quota: a 403 carrying pre_consume_token_quota_failed.
const upstreamQuotaBody = `{"error":{"message":"token quota is not enough, token remain quota: $0.07, need quota: $0.15","type":"new_api_error","param":"","code":"pre_consume_token_quota_failed"}}`

// newUpstream starts a fake OpenAI-format upstream that answers every request
// with the given status and body.
func newUpstream(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestConfig(upstreamURL string) *config.Config {
	return &config.Config{
		Host:          "127.0.0.1",
		Port:          "8082",
		OpenAIBaseURL: upstreamURL + "/v1",
		OpenAIAPIKey:  "test-key",
		OpusModel:     "test-model",
		SonnetModel:   "test-model",
		HaikuModel:    "test-model",
	}
}

func newTestApp(cfg *config.Config) *fiber.App {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	setupClaudeEndpoints(app, cfg)
	return app
}

// postMessages issues a /v1/messages request against the app under test.
func postMessages(t *testing.T, app *fiber.App, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return string(b)
}

// TestNonStreamingUpstream403IsPassedThrough pins the field bug: an upstream
// quota rejection used to be reported as 500, which made Claude Code treat it
// as a retryable server error and spin for ~3 minutes per call.
func TestNonStreamingUpstream403IsPassedThrough(t *testing.T) {
	upstream := newUpstream(t, http.StatusForbidden, upstreamQuotaBody)
	app := newTestApp(newTestConfig(upstream.URL))

	resp := postMessages(t, app, `{"model":"claude-3-5-sonnet-20241022","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 passed through, got %d", resp.StatusCode)
	}
}

// TestNonStreamingUpstreamErrorKeepsBody: the upstream body must survive so the
// platform can still match the quota vocabulary.
func TestNonStreamingUpstreamErrorKeepsBody(t *testing.T) {
	upstream := newUpstream(t, http.StatusForbidden, upstreamQuotaBody)
	app := newTestApp(newTestConfig(upstream.URL))

	resp := postMessages(t, app, `{"model":"claude-3-5-sonnet-20241022","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	raw := readBody(t, resp)

	var body map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("response is not JSON: %v (body=%q)", err, raw)
	}
	errObj, ok := body["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected an Anthropic-shaped error object, got %v", body)
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "token quota is not enough") {
		t.Errorf("upstream message lost; got %q", msg)
	}
}

// TestNonStreamingUpstreamStatusesPassThrough covers the statuses Claude Code
// treats differently: 401 is not retried, 429 is, 5xx is.
func TestNonStreamingUpstreamStatusesPassThrough(t *testing.T) {
	cases := []struct {
		upstream int
		want     int
	}{
		{http.StatusUnauthorized, http.StatusUnauthorized},
		{http.StatusForbidden, http.StatusForbidden},
		{http.StatusTooManyRequests, http.StatusTooManyRequests},
		{http.StatusBadGateway, http.StatusBadGateway},
		{http.StatusServiceUnavailable, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.upstream), func(t *testing.T) {
			upstream := newUpstream(t, tc.upstream, upstreamQuotaBody)
			app := newTestApp(newTestConfig(upstream.URL))

			resp := postMessages(t, app, `{"model":"claude-3-5-sonnet-20241022","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
			if resp.StatusCode != tc.want {
				t.Errorf("upstream %d: expected %d, got %d", tc.upstream, tc.want, resp.StatusCode)
			}
		})
	}
}

// TestStreamingUpstream403IsNotHttp200 is the streaming half of the same bug.
// The SSE headers used to be written before the upstream call, so a quota
// rejection surfaced as HTTP 200 + an error event: Claude Code saw a successful
// stream, and the status code was gone entirely.
func TestStreamingUpstream403IsNotHttp200(t *testing.T) {
	upstream := newUpstream(t, http.StatusForbidden, upstreamQuotaBody)
	app := newTestApp(newTestConfig(upstream.URL))

	resp := postMessages(t, app, `{"model":"claude-3-5-sonnet-20241022","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	raw := readBody(t, resp)

	if resp.StatusCode == http.StatusOK {
		t.Errorf("streaming quota rejection must not answer 200 (body=%q)", raw)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 passed through, got %d (body=%q)", resp.StatusCode, raw)
	}
}

// TestStreamingSuccessStillStreams guards the fix from breaking the happy path.
func TestStreamingSuccessStillStreams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}` + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte(`data: {"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)

	app := newTestApp(newTestConfig(srv.URL))
	resp := postMessages(t, app, `{"model":"claude-3-5-sonnet-20241022","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for a successful stream, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("expected SSE content type, got %q", ct)
	}
}

// TestUpstreamErrorStatusIsAvailableOnTheError: the fix keeps the status on a
// typed error so handlers can forward it instead of guessing 500.
func TestUpstreamErrorStatusIsAvailableOnTheError(t *testing.T) {
	var err error = &UpstreamError{Status: http.StatusForbidden, Body: upstreamQuotaBody}

	var target *UpstreamError
	if !errors.As(err, &target) {
		t.Fatalf("UpstreamError must be recoverable with errors.As")
	}
	if target.Status != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", target.Status)
	}
	if !strings.Contains(target.Error(), "token quota is not enough") {
		t.Errorf("message must keep the upstream body; got %q", target.Error())
	}
}
