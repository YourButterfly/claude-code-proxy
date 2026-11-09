package converter

import (
	"strings"
	"testing"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/pkg/models"
)

// TestBillingHeaderCCHIsStableAcrossRequests pins the prompt-cache regression.
//
// Claude Code stamps every request's system prompt with
//
//	x-anthropic-billing-header: cc_version=...; cc_entrypoint=sdk-cli; cch=<hex>
//
// where cch changes on every request. The upstream (ds-flash) applies automatic
// prefix caching, so a prefix that differs in even one byte — at offset ~92,
// right at the front — can never be reused. The effect was total: cache_read
// stayed 0 for an entire 250-request run, and the same prompt cost full input
// every turn.
//
// The proxy is the only place that sees every request, so it must normalize the
// field before forwarding. This test asserts that two requests whose cch values
// differ produce byte-identical OpenAI bodies.
func TestBillingHeaderCCHIsStableAcrossRequests(t *testing.T) {
	const sysA = "x-anthropic-billing-header: cc_version=2.1.153.44c; " +
		"cc_entrypoint=sdk-cli; cch=62b6d;\nYou are a coding agent."
	const sysB = "x-anthropic-billing-header: cc_version=2.1.153.44c; " +
		"cc_entrypoint=sdk-cli; cch=a57c9;\nYou are a coding agent."

	build := func(sys string) *models.OpenAIRequest {
		claudeReq := models.ClaudeRequest{
			Model:     "claude-sonnet-4-5-20250805",
			MaxTokens: 100,
			System:    []interface{}{map[string]interface{}{"type": "text", "text": sys}},
			Messages:  []models.ClaudeMessage{{Role: "user", Content: "hi"}},
		}
		cfg := &config.Config{SonnetModel: "ds-flash"}
		cfg.OpenAIBaseURL = "http://192.168.112.170:3000/v1"
		out, err := ConvertRequest(claudeReq, cfg)
		if err != nil {
			t.Fatalf("ConvertRequest: %v", err)
		}
		return out
	}

	gotA := systemContent(build(sysA))
	gotB := systemContent(build(sysB))

	if gotA != gotB {
		t.Fatalf("system prompt still varies per request, so prefix caching is dead:\n A=%q\n B=%q",
			gotA, gotB)
	}
	if strings.Contains(gotA, "cch=62b6d") || strings.Contains(gotA, "cch=a57c9") {
		t.Errorf("per-request cch value survived normalization: %q", gotA)
	}
	if !strings.Contains(gotA, "x-anthropic-billing-header") {
		t.Errorf("billing header was dropped entirely; only the volatile value may change: %q", gotA)
	}
	if !strings.Contains(gotA, "cc_entrypoint=sdk-cli") {
		t.Errorf("stable parts of the billing header must be preserved: %q", gotA)
	}
}

// TestBillingHeaderWithoutCCHIsUntouched guards against the normalizer eating
// unrelated system prompts (or a prompt that merely mentions "cch=").
func TestBillingHeaderWithoutCCHIsUntouched(t *testing.T) {
	cases := []string{
		"You are a coding agent.",
		"Explain cch=abc semantics in some other protocol.",
		"x-anthropic-billing-header: cc_version=2.1.153.44c; cc_entrypoint=sdk-cli;",
	}
	for _, sys := range cases {
		claudeReq := models.ClaudeRequest{
			Model:     "claude-sonnet-4-5-20250805",
			MaxTokens: 100,
			System:    []interface{}{map[string]interface{}{"type": "text", "text": sys}},
			Messages:  []models.ClaudeMessage{{Role: "user", Content: "hi"}},
		}
		cfg := &config.Config{SonnetModel: "ds-flash"}
		cfg.OpenAIBaseURL = "http://192.168.112.170:3000/v1"
		out, err := ConvertRequest(claudeReq, cfg)
		if err != nil {
			t.Fatalf("ConvertRequest: %v", err)
		}
		if got := systemContent(out); got != sys {
			t.Errorf("system prompt was altered but should not have been:\n in=%q\nout=%q", sys, got)
		}
	}
}

// systemContent returns the single leading system message's text.
func systemContent(req *models.OpenAIRequest) string {
	for _, m := range req.Messages {
		if m.Role == "system" {
			if s, ok := m.Content.(string); ok {
				return s
			}
		}
	}
	return ""
}
