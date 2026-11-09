package converter

import (
	"testing"

	"github.com/claude-code-proxy/proxy/pkg/models"
)

// TestNonStreamingUsageExcludesCachedFromInput mirrors the streaming fix on the
// non-streaming path. OpenAI's prompt_tokens includes the cached part; Anthropic
// reports the uncached remainder plus a separate cache_read_input_tokens.
// Without this, a non-streaming caller sees the whole cached prefix billed as
// fresh input.
func TestNonStreamingUsageExcludesCachedFromInput(t *testing.T) {
	finish := "stop"
	resp := &models.OpenAIResponse{
		ID:     "chatcmpl-1",
		Object: "chat.completion",
		Model:  "ds-flash",
		Choices: []models.OpenAIChoice{{
			Index:        0,
			Message:      models.OpenAIMessage{Role: "assistant", Content: "ok"},
			FinishReason: &finish,
		}},
		Usage: models.OpenAIUsage{
			PromptTokens:     12000,
			CompletionTokens: 300,
			TotalTokens:      12300,
			PromptTokensDetails: &models.OpenAIPromptTokensDetails{
				CachedTokens: 9000,
			},
		},
	}

	out, err := ConvertResponse(resp, "claude-sonnet-4-5-20250805")
	if err != nil {
		t.Fatalf("ConvertResponse: %v", err)
	}
	if out.Usage.CacheReadInputTokens != 9000 {
		t.Errorf("CacheReadInputTokens = %d, want 9000", out.Usage.CacheReadInputTokens)
	}
	if out.Usage.InputTokens != 3000 {
		t.Errorf("InputTokens = %d, want 3000 (12000 total - 9000 cached)", out.Usage.InputTokens)
	}
	if sum := out.Usage.InputTokens + out.Usage.CacheReadInputTokens; sum != 12000 {
		t.Errorf("input + cache_read = %d, want 12000", sum)
	}
}

// TestNonStreamingUsageWithoutCacheDetail keeps the plain path intact.
func TestNonStreamingUsageWithoutCacheDetail(t *testing.T) {
	finish := "stop"
	resp := &models.OpenAIResponse{
		Model: "ds-flash",
		Choices: []models.OpenAIChoice{{
			Index:        0,
			Message:      models.OpenAIMessage{Role: "assistant", Content: "ok"},
			FinishReason: &finish,
		}},
		Usage: models.OpenAIUsage{PromptTokens: 777, CompletionTokens: 5, TotalTokens: 782},
	}
	out, err := ConvertResponse(resp, "claude-sonnet-4-5-20250805")
	if err != nil {
		t.Fatalf("ConvertResponse: %v", err)
	}
	if out.Usage.InputTokens != 777 {
		t.Errorf("InputTokens = %d, want 777", out.Usage.InputTokens)
	}
	if out.Usage.CacheReadInputTokens != 0 {
		t.Errorf("CacheReadInputTokens = %d, want 0", out.Usage.CacheReadInputTokens)
	}
}

// TestNonStreamingCachedNeverExceedsPrompt guards malformed upstream data.
func TestNonStreamingCachedNeverExceedsPrompt(t *testing.T) {
	finish := "stop"
	resp := &models.OpenAIResponse{
		Model: "ds-flash",
		Choices: []models.OpenAIChoice{{
			Index:        0,
			Message:      models.OpenAIMessage{Role: "assistant", Content: "ok"},
			FinishReason: &finish,
		}},
		Usage: models.OpenAIUsage{
			PromptTokens:        100,
			CompletionTokens:    1,
			TotalTokens:         101,
			PromptTokensDetails: &models.OpenAIPromptTokensDetails{CachedTokens: 150},
		},
	}
	out, err := ConvertResponse(resp, "claude-sonnet-4-5-20250805")
	if err != nil {
		t.Fatalf("ConvertResponse: %v", err)
	}
	if out.Usage.InputTokens < 0 {
		t.Errorf("InputTokens = %d, must never be negative", out.Usage.InputTokens)
	}
}
