package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// usageFromStream replays a streaming request whose upstream reports the given
// OpenAI usage object, and returns the usage the proxy reported to Claude Code.
func usageFromStream(t *testing.T, openAIUsage string) map[string]interface{} {
	t.Helper()
	sse := strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"}}]}`,
		``,
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":` + openAIUsage + `}`,
		``,
		`data: [DONE]`,
		``,
		``,
	}, "\n")

	upstream := newStreamingUpstream(t, sse)
	app := newTestApp(newTestConfig(upstream.URL))
	resp := postMessages(t, app, streamRequestBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body: %s", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)

	var usage map[string]interface{}
	for _, ev := range sseEvents(t, body) {
		for _, name := range []string{"message_start", "message_delta"} {
			if ev.event != name {
				continue
			}
			var payload map[string]interface{}
			if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
				continue
			}
			candidate := payload
			if msg, ok := payload["message"].(map[string]interface{}); ok {
				candidate = msg
			}
			if u, ok := candidate["usage"].(map[string]interface{}); ok && u != nil {
				usage = u
			}
		}
	}
	if usage == nil {
		t.Fatalf("no usage in stream (body: %s)", body)
	}
	return usage
}

func num(t *testing.T, m map[string]interface{}, key string) int {
	t.Helper()
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%s is not a number: %#v", key, v)
	}
	return int(f)
}

// TestCachedTokensAreNotBilledTwice pins the third cache bug.
//
// OpenAI's prompt_tokens is the TOTAL prompt size, cached tokens included:
//
//	{"prompt_tokens": 12000, "prompt_tokens_details": {"cached_tokens": 9000}}
//
// means 12000 prompt tokens of which 9000 were served from cache. Anthropic's
// two fields are DISJOINT: input_tokens is the uncached remainder, and
// cache_read_input_tokens is what was read from cache. Claude Code (and soaigen,
// which sums input + output + cache_creation) treats them that way.
//
// The proxy copied prompt_tokens straight into input_tokens, so every cached
// token was billed twice: once as input and once as cache_read (which the
// accounting deliberately excludes from cost, so the cache saved nothing and
// the total was inflated by the whole cached prefix — 70x on a real run whose
// cache hit rate was 97%).
func TestCachedTokensAreNotBilledTwice(t *testing.T) {
	usage := usageFromStream(t, `{"prompt_tokens":12000,"completion_tokens":300,"total_tokens":12300,`+
		`"prompt_tokens_details":{"cached_tokens":9000}}`)

	if got := num(t, usage, "cache_read_input_tokens"); got != 9000 {
		t.Errorf("cache_read_input_tokens = %d, want 9000", got)
	}
	if got := num(t, usage, "input_tokens"); got != 3000 {
		t.Errorf("input_tokens = %d, want 3000 (12000 total - 9000 cached)", got)
	}
	if got, want := num(t, usage, "input_tokens")+num(t, usage, "cache_read_input_tokens"), 12000; got != want {
		t.Errorf("input + cache_read = %d, want %d (must add up to the upstream total)", got, want)
	}
}

// TestUncachedStreamUsageUnchanged guards the common path: when the upstream
// reports no cached tokens, input_tokens must stay the full prompt.
func TestUncachedStreamUsageUnchanged(t *testing.T) {
	usage := usageFromStream(t, `{"prompt_tokens":777,"completion_tokens":5,"total_tokens":782}`)
	if got := num(t, usage, "input_tokens"); got != 777 {
		t.Errorf("input_tokens = %d, want 777", got)
	}
	if got := num(t, usage, "cache_read_input_tokens"); got != 0 {
		t.Errorf("cache_read_input_tokens = %d, want 0", got)
	}
}

// TestCachedTokensNeverExceedPrompt guards a malformed upstream: a provider that
// reports cached_tokens > prompt_tokens must not make input_tokens negative.
func TestCachedTokensNeverExceedPrompt(t *testing.T) {
	usage := usageFromStream(t, `{"prompt_tokens":100,"completion_tokens":1,"total_tokens":101,`+
		`"prompt_tokens_details":{"cached_tokens":150}}`)
	if got := num(t, usage, "input_tokens"); got < 0 {
		t.Errorf("input_tokens = %d, must never be negative", got)
	}
}
