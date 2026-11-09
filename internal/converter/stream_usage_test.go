package converter

import (
	"testing"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/pkg/models"
)

// streamOn is the pointer value Claude's JSON unmarshals "stream": true into.
func streamOn() *bool { b := true; return &b }

func convertStreaming(t *testing.T, baseURL string, withTools bool) *models.OpenAIRequest {
	t.Helper()
	req := models.ClaudeRequest{
		Model:     "claude-sonnet-4-5-20250805",
		MaxTokens: 64,
		Stream:    streamOn(),
		Messages:  []models.ClaudeMessage{{Role: "user", Content: "hi"}},
	}
	if withTools {
		req.Tools = []models.Tool{{
			Name:        "Bash",
			Description: "run a command",
			InputSchema: map[string]interface{}{"type": "object"},
		}}
	}
	cfg := &config.Config{SonnetModel: "ds-flash"}
	cfg.OpenAIBaseURL = baseURL
	out, err := ConvertRequest(req, cfg)
	if err != nil {
		t.Fatalf("ConvertRequest(%s): %v", baseURL, err)
	}
	return out
}

// TestEveryProviderAsksForStreamingUsage pins the second half of the cache-
// visibility bug: the provider switch had no default case, so any base URL that
// is neither OpenRouter, api.openai.com nor localhost (e.g. the company's own
// ds-flash gateway at 192.168.112.170:3000) got no stream_options at all.
// Streaming responses then carried usage only if the upstream volunteered it,
// which makes token accounting depend on the provider's goodwill.
//
// include_usage is the standard OpenAI field for "send a final usage chunk".
func TestEveryProviderAsksForStreamingUsage(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
	}{
		{"upstream gateway (ProviderUnknown)", "http://192.168.112.170:3000/v1"},
		{"OpenRouter", "https://openrouter.ai/api/v1"},
		{"OpenAI direct", "https://api.openai.com/v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := convertStreaming(t, tc.baseURL, false)
			if out.StreamOptions == nil {
				t.Fatalf("%s: StreamOptions is nil, so streamed usage is not requested", tc.name)
			}
			if got, ok := out.StreamOptions["include_usage"].(bool); !ok || !got {
				t.Errorf("%s: stream_options.include_usage = %v, want true", tc.name, out.StreamOptions["include_usage"])
			}
		})
	}
}

// TestOllamaStreamOptionsUnchanged records a deliberate non-change rather than
// an oversight. Ollama is the one provider we cannot verify against a live
// server here, and the proxy has no retry path for rejected parameters (only
// for max_completion_tokens), so a rejected stream_options would be a hard
// failure. Its behaviour therefore stays exactly as before until someone can
// test it against a real daemon.
func TestOllamaStreamOptionsUnchanged(t *testing.T) {
	out := convertStreaming(t, "http://localhost:11434/v1", false)
	if out.StreamOptions != nil {
		t.Errorf("Ollama behaviour changed without verification: StreamOptions = %v", out.StreamOptions)
	}
}

// TestProviderSpecificExtrasSurvive makes sure asking for usage everywhere does
// not trample the per-provider parameters each endpoint actually needs.
func TestProviderSpecificExtrasSurvive(t *testing.T) {
	t.Run("OpenRouter keeps usage.include and reasoning", func(t *testing.T) {
		out := convertStreaming(t, "https://openrouter.ai/api/v1", false)
		if out.Usage == nil || out.Usage["include"] != true {
			t.Errorf("OpenRouter lost usage.include: %v", out.Usage)
		}
		if out.Reasoning == nil || out.Reasoning["enabled"] != true {
			t.Errorf("OpenRouter lost reasoning.enabled: %v", out.Reasoning)
		}
		if out.ReasoningEffort != "" {
			t.Errorf("OpenRouter must not carry ReasoningEffort, got %q", out.ReasoningEffort)
		}
	})

	t.Run("OpenAI direct keeps reasoning_effort", func(t *testing.T) {
		out := convertStreaming(t, "https://api.openai.com/v1", false)
		if out.ReasoningEffort != "medium" {
			t.Errorf("OpenAI ReasoningEffort = %q, want medium", out.ReasoningEffort)
		}
		if out.Reasoning != nil {
			t.Errorf("OpenAI must not carry the OpenRouter reasoning object: %v", out.Reasoning)
		}
	})

	t.Run("localhost keeps tool_choice=required when tools exist", func(t *testing.T) {
		out := convertStreaming(t, "http://localhost:11434/v1", true)
		if out.ToolChoice != "required" {
			t.Errorf("localhost ToolChoice = %v, want required", out.ToolChoice)
		}
	})

	t.Run("localhost does not force tool_choice without tools", func(t *testing.T) {
		out := convertStreaming(t, "http://localhost:11434/v1", false)
		if out.ToolChoice != nil {
			t.Errorf("ToolChoice should stay unset without tools, got %v", out.ToolChoice)
		}
	})
}

// TestNonStreamingOmitsStreamOptions guards the other direction: stream_options
// is meaningless (and rejected by some servers) on a non-streaming request.
func TestNonStreamingOmitsStreamOptions(t *testing.T) {
	req := models.ClaudeRequest{
		Model:     "claude-sonnet-4-5-20250805",
		MaxTokens: 64,
		Messages:  []models.ClaudeMessage{{Role: "user", Content: "hi"}},
	}
	cfg := &config.Config{SonnetModel: "ds-flash"}
	cfg.OpenAIBaseURL = "http://192.168.112.170:3000/v1"
	out, err := ConvertRequest(req, cfg)
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	if out.StreamOptions != nil {
		t.Errorf("non-streaming request must not carry stream_options: %v", out.StreamOptions)
	}
}
