package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/converter"
	"github.com/claude-code-proxy/proxy/pkg/models"
	"github.com/gofiber/fiber/v2"
)

const (
	// streamingResponseHeaderTimeout bounds how long an upstream may take to
	// produce response HEADERS. It is the "dead upstream" guard, and it must not
	// bound the body: see newStreamingClient.
	streamingResponseHeaderTimeout = 120 * time.Second

	// streamIdleTimeout bounds how long the body may go silent. A generation
	// that stalls (upstream wedged, connection black-holed by a middlebox) is
	// caught here; a generation that is merely long is not.
	//
	// 180s because an upstream reasoning over a large prefix legitimately shows
	// no output for a minute or more before the first token; anything below that
	// trades one failure mode for another. Overridable via STREAM_IDLE_TIMEOUT
	// (Go duration, e.g. "300s") so a deployment behind an unusually slow
	// gateway does not need a rebuild.
	streamIdleTimeoutDefault = 180 * time.Second
)

// streamIdleTimeout is the effective idle window: STREAM_IDLE_TIMEOUT when set
// and parseable, the default otherwise. A malformed value falls back rather than
// failing startup — losing a tunable must not take the proxy down.
var streamIdleTimeout = func() time.Duration {
	raw := os.Getenv("STREAM_IDLE_TIMEOUT")
	if raw == "" {
		return streamIdleTimeoutDefault
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		fmt.Printf(
			"⚠️  STREAM_IDLE_TIMEOUT=%q is not a positive Go duration; using %s\n",
			raw, streamIdleTimeoutDefault,
		)
		return streamIdleTimeoutDefault
	}
	return d
}()

// newStreamingClient builds the HTTP client used for streaming upstream calls.
//
// It has NO http.Client.Timeout on purpose. That field is a deadline for the
// whole exchange including response-body reads, so on a streaming endpoint it
// silently caps generation length: a healthy stream emitting a chunk every 2s
// was killed at exactly 300.0s (quiet_for=0.8s) in local reproduction, and
// production runs died at 300.4-300.5s with zero tokens. Generation calls that
// work take 345s, 395s, 552s, 564s, so a whole-request cap cannot be set high
// enough to be safe without also being useless as a fault detector.
//
// The two distinct failure modes are therefore bounded separately:
//   - upstream never answers      -> streamingResponseHeaderTimeout (Transport)
//   - upstream answers then stalls -> streamIdleTimeout (per read, in the
//     stream reader)
func newStreamingClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			// Headers only. Body reads are governed by streamIdleTimeout.
			ResponseHeaderTimeout: streamingResponseHeaderTimeout,
		},
	}
}

// addOpenRouterHeaders adds OpenRouter-specific HTTP headers for better rate limits.
// Sets HTTP-Referer and X-Title headers when configured, which helps with OpenRouter's
// rate limiting and usage tracking.
func addOpenRouterHeaders(req *http.Request, cfg *config.Config) {
	if cfg.OpenRouterAppURL != "" {
		req.Header.Set("HTTP-Referer", cfg.OpenRouterAppURL)
	}
	if cfg.OpenRouterAppName != "" {
		req.Header.Set("X-Title", cfg.OpenRouterAppName)
	}
}

// writeUpstreamFailure answers an upstream failure with the upstream's own
// status code and body, wrapped in Anthropic's error envelope.
//
// Status pass-through is the whole point: Claude Code retries 5xx and 429 with
// exponential backoff but gives up immediately on 401/403. Answering 500 for a
// quota rejection turned a one-second failure into ~3 minutes per call.
func writeUpstreamFailure(c *fiber.Ctx, err error) error {
	var upstream *UpstreamError
	if errors.As(err, &upstream) {
		return c.Status(downstreamStatus(upstream.Status)).JSON(fiber.Map{
			"type": "error",
			"error": fiber.Map{
				"type":    "api_error",
				"message": upstream.Error(),
			},
		})
	}
	// Not an upstream HTTP failure (marshal error, connection refused, ...):
	// that is a genuine proxy-side fault, so 502 keeps the retry semantics
	// without claiming the upstream rejected the request.
	return c.Status(http.StatusBadGateway).JSON(fiber.Map{
		"type": "error",
		"error": fiber.Map{
			"type":    "api_error",
			"message": fmt.Sprintf("OpenAI API error: %v", err),
		},
	})
}

// handleMessages is the main handler for /v1/messages endpoint.
// It parses Claude requests, converts them to OpenAI format, and routes to either
// streaming or non-streaming handlers based on the request's stream parameter.
func handleMessages(c *fiber.Ctx, cfg *config.Config) error {
	// Debug: Log raw request
	if cfg.Debug {
		fmt.Printf("\n=== CLAUDE REQUEST ===\n%s\n===================\n", string(c.Body()))
	}

	// Parse Claude request
	var claudeReq models.ClaudeRequest
	if err := c.BodyParser(&claudeReq); err != nil {
		// Log the error and raw body for debugging
		fmt.Printf("[ERROR] Failed to parse request body: %v\n", err)
		fmt.Printf("[ERROR] Raw body: %s\n", string(c.Body()))
		return c.Status(400).JSON(fiber.Map{
			"type": "error",
			"error": fiber.Map{
				"type":    "invalid_request_error",
				"message": fmt.Sprintf("Invalid request body: %v", err),
			},
		})
	}

	// Validate API key (if configured)
	if cfg.AnthropicAPIKey != "" {
		apiKey := c.Get("x-api-key")
		if apiKey != cfg.AnthropicAPIKey {
			return c.Status(401).JSON(fiber.Map{
				"type": "error",
				"error": fiber.Map{
					"type":    "authentication_error",
					"message": "Invalid API key",
				},
			})
		}
	}

	// Convert Claude request to OpenAI format
	openaiReq, err := converter.ConvertRequest(claudeReq, cfg)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{
			"type": "error",
			"error": fiber.Map{
				"type":    "invalid_request_error",
				"message": err.Error(),
			},
		})
	}

	// Debug: Log converted OpenAI request
	if cfg.Debug {
		openaiReqJSON, _ := json.MarshalIndent(openaiReq, "", "  ")
		fmt.Printf("\n=== OPENAI REQUEST ===\n%s\n===================\n", string(openaiReqJSON))
		if len(claudeReq.Tools) > 0 {
			fmt.Printf("[DEBUG] Request has %d tools\n", len(claudeReq.Tools))
			for i, tool := range openaiReq.Tools {
				fmt.Printf("[DEBUG] Tool %d: %s\n", i, tool.Function.Name)
			}
		}
	}

	// Debug: Check Stream field
	if cfg.Debug {
		if openaiReq.Stream == nil {
			fmt.Printf("[DEBUG] Stream field is nil\n")
		} else {
			fmt.Printf("[DEBUG] Stream field = %v\n", *openaiReq.Stream)
		}
	}

	// Handle streaming vs non-streaming
	if openaiReq.Stream != nil && *openaiReq.Stream {
		return handleStreamingMessages(c, openaiReq, cfg)
	}

	// Track timing for simple log
	startTime := time.Now()

	// Non-streaming response
	openaiResp, err := callOpenAI(openaiReq, cfg)
	if err != nil {
		return writeUpstreamFailure(c, err)
	}

	// Debug: Log OpenAI response
	if cfg.Debug {
		openaiRespJSON, _ := json.MarshalIndent(openaiResp, "", "  ")
		fmt.Printf("\n=== OPENAI RESPONSE ===\n%s\n====================\n", string(openaiRespJSON))
		if len(openaiResp.Choices) > 0 {
			choice := openaiResp.Choices[0]
			fmt.Printf("[DEBUG] OpenAI response has %d tool_calls\n", len(choice.Message.ToolCalls))
			for i, tc := range choice.Message.ToolCalls {
				fmt.Printf("[DEBUG] ToolCall %d: ID=%s, Name=%s\n", i, tc.ID, tc.Function.Name)
			}
		}
	}

	// Convert OpenAI response to Claude format
	claudeResp, err := converter.ConvertResponse(openaiResp, claudeReq.Model)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{
			"type": "error",
			"error": fiber.Map{
				"type":    "api_error",
				"message": fmt.Sprintf("Response conversion error: %v", err),
			},
		})
	}

	// Debug: Log Claude response
	if cfg.Debug {
		claudeRespJSON, _ := json.MarshalIndent(claudeResp, "", "  ")
		fmt.Printf("\n=== CLAUDE RESPONSE ===\n%s\n====================\n\n", string(claudeRespJSON))
		fmt.Printf("[DEBUG] Claude response has %d content blocks\n", len(claudeResp.Content))
		for i, block := range claudeResp.Content {
			fmt.Printf("[DEBUG] Block %d: type=%s", i, block.Type)
			if block.Type == "tool_use" {
				fmt.Printf(", name=%s, id=%s", block.Name, block.ID)
			}
			fmt.Printf("\n")
		}
	}

	// Simple log: one-line summary
	if cfg.SimpleLog {
		duration := time.Since(startTime).Seconds()
		tokensPerSec := 0.0
		if duration > 0 && claudeResp.Usage.OutputTokens > 0 {
			tokensPerSec = float64(claudeResp.Usage.OutputTokens) / duration
		}
		timestamp := time.Now().Format("15:04:05")
		fmt.Printf("[%s] [REQ] %s model=%s in=%d out=%d tok/s=%.1f\n",
			timestamp,
			cfg.OpenAIBaseURL,
			openaiReq.Model,
			claudeResp.Usage.InputTokens,
			claudeResp.Usage.OutputTokens,
			tokensPerSec)
	}

	return c.JSON(claudeResp)
}

// handleStreamingMessages handles streaming SSE responses from the provider.
// It forwards the OpenAI request, receives streaming chunks, and converts them to
// Claude's SSE event format in real-time using streamOpenAIToClaude.
func handleStreamingMessages(c *fiber.Ctx, openaiReq *models.OpenAIRequest, cfg *config.Config) error {
	// Track timing for simple log
	startTime := time.Now()

	if cfg.Debug {
		fmt.Printf("[DEBUG] handleStreamingMessages: Making streaming request to %s\n", cfg.OpenAIBaseURL+"/chat/completions")
	}

	// Open the upstream stream BEFORE committing any response headers.
	//
	// This ordering is load-bearing. Previously the SSE headers (and with them
	// the 200 status) were written first and the upstream call happened inside
	// the stream writer, so an upstream rejection could only be reported as an
	// SSE error event on an HTTP 200 — the status code was gone. Claude Code
	// reads a 200 as a healthy stream, so a quota rejection became a retryable
	// 5xx-shaped failure with no recoverable status.
	resp, err := callOpenAIStream(openaiReq, cfg)
	if err != nil {
		if cfg.Debug {
			fmt.Printf("[DEBUG] handleStreamingMessages: Request failed: %v\n", err)
		}
		return writeUpstreamFailure(c, err)
	}

	// Set SSE headers only now that the upstream agreed to serve.
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	// Always log that the upstream accepted the request, even when nothing
	// follows.  Diagnosing a mid-stream disconnect ("stream read error: context
	// deadline exceeded") needs to know WHICH side died: an upstream that never
	// returned headers fails in callOpenAIStream above, whereas this line proves
	// headers arrived and the failure is in the body.  Without it both cases look
	// identical from the client (one 115-byte error line, zero tokens).
	fmt.Printf("[%s] [STREAM-OPEN] tag=%s %s model=%s status=%d duration_to_headers=%.1fs\n",
		time.Now().Format("15:04:05"), streamTag(startTime), cfg.OpenAIBaseURL,
		openaiReq.Model, resp.StatusCode, time.Since(startTime).Seconds())

	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		// The upstream body is closed HERE, not by a defer in this function.
		//
		// Fiber runs the body stream writer after the handler returns, so a
		// `defer resp.Body.Close()` in handler scope closes the body before a
		// single chunk is read. Every streaming request then produced an empty
		// message (0 output tokens) followed by
		// "stream read error: http: read on closed response body" — the
		// c.Gateway error that made all streaming calls fail while
		// non-streaming calls succeeded.
		defer func() { _ = resp.Body.Close() }()

		if cfg.Debug {
			fmt.Printf("[DEBUG] StreamWriter: Starting\n")
		}

		// Stream conversion
		streamOpenAIToClaude(w, resp.Body, openaiReq.Model, cfg, startTime)

		if cfg.Debug {
			fmt.Printf("[DEBUG] StreamWriter: Completed\n")
		}
	})

	return nil
}

// ToolCallState tracks the state of a tool call during streaming
type ToolCallState struct {
	ID          string // Tool call ID from OpenAI
	Name        string // Function name
	ArgsBuffer  string // Accumulated JSON arguments
	JSONSent    bool   // Flag if we sent the JSON delta
	ClaudeIndex int    // The content block index for Claude
	Started     bool   // Flag if content_block_start was sent
}

// streamOpenAIToClaude converts OpenAI streaming responses to Claude's SSE event format.
//
// It processes the OpenAI SSE stream chunk-by-chunk, generating the proper sequence of
// Claude events: message_start, content_block_start, content_block_delta, content_block_stop,
// message_delta, and message_stop.
//
// Handles:
//   - Thinking blocks from reasoning models (OpenRouter's reasoning_details, OpenAI's reasoning_content)
//   - Text content deltas
//   - Tool call deltas (accumulates JSON arguments across chunks)
//   - Token usage tracking and throughput calculation for simple log mode
//
// The function maintains state to track content block indices, tool call accumulation,
// and ensures proper event ordering for Claude Code compatibility.
func streamOpenAIToClaude(w *bufio.Writer, reader io.Reader, providerModel string, cfg *config.Config, startTime time.Time) {
	streamOpenAIToClaudeWithIdle(w, reader, providerModel, cfg, startTime, streamIdleTimeout)
}

// streamOpenAIToClaudeWithIdle is streamOpenAIToClaude with an injectable idle
// timeout so tests can exercise the stall path without waiting minutes.
func streamOpenAIToClaudeWithIdle(w *bufio.Writer, reader io.Reader, providerModel string, cfg *config.Config, startTime time.Time, idle time.Duration) {
	if cfg.Debug {
		fmt.Printf("[DEBUG] streamOpenAIToClaude: Starting conversion\n")
	}
	// Instrumentation for mid-stream disconnects.  A "stream read error" on a
	// 0-token call is the signature of the upstream or an intermediary going
	// silent, and the error string alone cannot say whether ANY bytes ever
	// arrived, how long the stream was quiet before it broke, or whether chunks
	// were already forwarded.  Those three facts decide whether the culprit is
	// the upstream, the link, or this proxy.
	//
	// The counters live in the reader (not in locals) because the idle-timeout
	// path can leave a read goroutine running after this function has moved on;
	// reading them through its mutex is what keeps this log line race-free.
	tag := streamTag(startTime)
	var eventsIn int
	var downstreamErr error
	counting := &countingReader{r: reader, idle: idle}
	streamReport := func() (int64, time.Time, time.Time) { return counting.snapshot() }
	defer func() {
		bytesIn, firstChunkAt, lastChunkAt := streamReport()
		extra := ""
		if downstreamErr != nil {
			extra = fmt.Sprintf(" downstream_gone=%v", downstreamErr)
		}
		fmt.Printf(
			"[%s] [STREAM-CLOSE] tag=%s %s model=%s elapsed=%.1fs bytes_from_upstream=%d "+
				"events=%d first_chunk_after=%s quiet_for=%s%s\n",
			time.Now().Format("15:04:05"), tag, cfg.OpenAIBaseURL, providerModel,
			time.Since(startTime).Seconds(), bytesIn, eventsIn, firstChunkLatency(firstChunkAt, startTime),
			quietFor(lastChunkAt), extra,
		)
	}()
	scanner := bufio.NewScanner(counting)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // Increase buffer size

	// State variables
	messageID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	textBlockIndex := 1   // Text block is index 1 (thinking is 0)
	toolBlockCounter := 2 // Tool calls start at index 2
	currentToolCalls := make(map[int]*ToolCallState)
	finalStopReason := "end_turn"
	usageData := map[string]interface{}{
		"input_tokens":                0,
		"output_tokens":               0,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     0,
		"cache_creation": map[string]interface{}{
			"ephemeral_5m_input_tokens": 0,
			"ephemeral_1h_input_tokens": 0,
		},
	}

	// Thinking block tracking (to show thinking indicator in Claude Code)
	thinkingBlockIndex := 0 // Thinking block is always index 0
	thinkingBlockStarted := false
	thinkingBlockHasContent := false
	textBlockStarted := false // Track if we've sent text block_start

	// Send initial SSE events
	writeSSEEvent(w, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         providerModel,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":                0,
				"output_tokens":               0,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
				"cache_creation": map[string]interface{}{
					"ephemeral_5m_input_tokens": 0,
					"ephemeral_1h_input_tokens": 0,
				},
			},
		},
	})

	writeSSEEvent(w, "ping", map[string]interface{}{
		"type": "ping",
	})

	_ = w.Flush()

	// Process streaming chunks
	for scanner.Scan() {
		line := scanner.Text()
		eventsIn++

		// Stop pulling from the upstream once the downstream is gone.
		//
		// Every flush below reports whether the client socket is still writable.
		// When it is not, continuing to read the upstream means holding an
		// AbortController-less connection open for a generation nobody will see,
		// and buffering every chunk of it. This used to be masked by
		// http.Client.Timeout (the whole request died at 300s regardless); with
		// that cap correctly removed, nothing else bounds it.
		if werr := w.Flush(); werr != nil {
			downstreamErr = werr
			return
		}

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}

		// Check for [DONE] marker
		if strings.Contains(line, "[DONE]") {
			break
		}

		// Parse data line
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		dataJSON := strings.TrimPrefix(line, "data: ")

		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(dataJSON), &chunk); err != nil {
			continue
		}

		// Log every chunk to see what OpenRouter is sending
		if cfg.Debug {
			fmt.Printf("[DEBUG] Raw chunk from OpenRouter: %s\n", dataJSON)
		}

		// Handle usage data
		if usage, ok := chunk["usage"].(map[string]interface{}); ok {
			if cfg.Debug {
				usageJSON, _ := json.Marshal(usage)
				fmt.Printf("[DEBUG] Received usage from OpenAI: %s\n", string(usageJSON))
			}

			// Convert float64 to int for token counts (JSON unmarshals numbers as float64)
			inputTokens := 0
			outputTokens := 0
			if val, ok := usage["prompt_tokens"].(float64); ok {
				inputTokens = int(val)
			}
			if val, ok := usage["completion_tokens"].(float64); ok {
				outputTokens = int(val)
			}

			usageData = map[string]interface{}{
				"input_tokens":  inputTokens,
				"output_tokens": outputTokens,
			}

			// Add cache metrics if present.
			//
			// OpenAI's prompt_tokens is the TOTAL prompt size, cached tokens
			// included. Anthropic's input_tokens and cache_read_input_tokens are
			// DISJOINT: input_tokens is the uncached remainder. Copying the total
			// straight into input_tokens billed every cached token twice — once
			// as input, and again as cache_read (which cost accounting excludes,
			// so the cache appeared to save nothing and the total was inflated by
			// the entire cached prefix; 70x on a run with a 97% hit rate).
			if promptTokensDetails, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok {
				if cachedTokens, ok := promptTokensDetails["cached_tokens"].(float64); ok && cachedTokens > 0 {
					cached := int(cachedTokens)
					// A provider reporting more cached than total is malformed;
					// never let the subtraction go negative.
					if cached > inputTokens {
						cached = inputTokens
					}
					usageData["cache_read_input_tokens"] = cached
					usageData["input_tokens"] = inputTokens - cached
				}
			}
			if cfg.Debug {
				usageDataJSON, _ := json.Marshal(usageData)
				fmt.Printf("[DEBUG] Accumulated usageData: %s\n", string(usageDataJSON))
			}
		}

		// Extract delta from choices
		choices, ok := chunk["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			continue
		}

		choice := choices[0].(map[string]interface{})
		delta, ok := choice["delta"].(map[string]interface{})
		if !ok {
			continue
		}

		// Handle reasoning delta (thinking blocks)
		// Support both OpenRouter and OpenAI formats:
		// - OpenRouter: delta.reasoning_details (array)
		// - OpenAI o1/o3: delta.reasoning_content (string)

		// First, check for OpenAI's reasoning_content format (o1/o3 models)
		if reasoningContent, ok := delta["reasoning_content"].(string); ok && reasoningContent != "" {
			// Send content_block_start for thinking block on first thinking delta
			if !thinkingBlockStarted {
				writeSSEEvent(w, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": thinkingBlockIndex,
					"content_block": map[string]interface{}{
						"type": "thinking",
					},
				})
				thinkingBlockStarted = true
				thinkingBlockHasContent = true
			}

			// Send thinking delta
			writeSSEEvent(w, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": thinkingBlockIndex,
				"delta": map[string]interface{}{
					"type": "thinking_delta",
					"text": reasoningContent,
				},
			})
		}

		// Then, check for OpenRouter's reasoning_details format
		// Only process reasoning_details if we haven't already processed reasoning field
		if reasoningDetailsRaw, ok := delta["reasoning_details"]; ok && delta["reasoning"] == nil {
			if reasoningDetails, ok := reasoningDetailsRaw.([]interface{}); ok && len(reasoningDetails) > 0 {
				for _, detailRaw := range reasoningDetails {
					if detail, ok := detailRaw.(map[string]interface{}); ok {
						// Extract reasoning text from the detail
						thinkingText := ""
						detailType, _ := detail["type"].(string)

						switch detailType {
						case "reasoning.text":
							if text, ok := detail["text"].(string); ok {
								thinkingText = text
							}
						case "reasoning.summary":
							if summary, ok := detail["summary"].(string); ok {
								thinkingText = summary
							}
						case "reasoning.encrypted":
							// Skip encrypted/redacted reasoning in streaming
							continue
						}

						if thinkingText != "" {
							// Send content_block_start for thinking block on first thinking delta
							if !thinkingBlockStarted {
								writeSSEEvent(w, "content_block_start", map[string]interface{}{
									"type":  "content_block_start",
									"index": thinkingBlockIndex,
									"content_block": map[string]interface{}{
										"type":     "thinking",
										"thinking": "",
									},
								})
								thinkingBlockStarted = true
								_ = w.Flush()
							}

							// Send thinking block delta
							writeSSEEvent(w, "content_block_delta", map[string]interface{}{
								"type":  "content_block_delta",
								"index": thinkingBlockIndex,
								"delta": map[string]interface{}{
									"type":     "thinking_delta",
									"thinking": thinkingText,
								},
							})
							thinkingBlockHasContent = true
							_ = w.Flush()
						}
					}
				}
			}
		}

		// Handle reasoning field directly (simpler format from some models)
		if reasoning, ok := delta["reasoning"].(string); ok && reasoning != "" {
			// Send content_block_start for thinking block on first thinking delta
			if !thinkingBlockStarted {
				writeSSEEvent(w, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": thinkingBlockIndex,
					"content_block": map[string]interface{}{
						"type":     "thinking",
						"thinking": "",
					},
				})
				thinkingBlockStarted = true
				_ = w.Flush()
			}

			// Send thinking block delta
			writeSSEEvent(w, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": thinkingBlockIndex,
				"delta": map[string]interface{}{
					"type":     "thinking_delta",
					"thinking": reasoning,
				},
			})
			thinkingBlockHasContent = true
			_ = w.Flush()
		}

		// Handle text delta
		if content, ok := delta["content"].(string); ok && content != "" {
			// Send content_block_start for text block on first text delta
			if !textBlockStarted {
				writeSSEEvent(w, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": textBlockIndex,
					"content_block": map[string]interface{}{
						"type": "text",
						"text": "",
					},
				})
				textBlockStarted = true
				_ = w.Flush()
			}

			writeSSEEvent(w, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": textBlockIndex,
				"delta": map[string]interface{}{
					"type": "text_delta",
					"text": content,
				},
			})
			_ = w.Flush()
		}

		// Handle tool call deltas
		if toolCallsRaw, ok := delta["tool_calls"]; ok {
			// Debug: Log raw tool_calls from provider
			if cfg.Debug {
				toolCallsJSON, _ := json.Marshal(toolCallsRaw)
				fmt.Printf("[DEBUG] Raw tool_calls delta: %s\n", string(toolCallsJSON))
			}

			toolCalls, ok := toolCallsRaw.([]interface{})
			if ok && len(toolCalls) > 0 {
				for _, tcRaw := range toolCalls {
					tcDelta, ok := tcRaw.(map[string]interface{})
					if !ok {
						continue
					}

					// Get tool call index
					tcIndex := 0
					if idx, ok := tcDelta["index"].(float64); ok {
						tcIndex = int(idx)
					}

					// Initialize tool call tracking if not exists
					if _, exists := currentToolCalls[tcIndex]; !exists {
						currentToolCalls[tcIndex] = &ToolCallState{
							ID:          "",
							Name:        "",
							ArgsBuffer:  "",
							JSONSent:    false,
							ClaudeIndex: 0,
							Started:     false,
						}
					}

					toolCall := currentToolCalls[tcIndex]

					// Update tool call ID if provided
					if id, ok := tcDelta["id"].(string); ok {
						toolCall.ID = id
					}

					// Update function name
					if functionData, ok := tcDelta["function"].(map[string]interface{}); ok {
						if name, ok := functionData["name"].(string); ok {
							toolCall.Name = name
						}

						// Start content block when we have complete initial data
						if toolCall.ID != "" && toolCall.Name != "" && !toolCall.Started {
							toolBlockCounter++
							claudeIndex := textBlockIndex + toolBlockCounter
							toolCall.ClaudeIndex = claudeIndex
							toolCall.Started = true

							writeSSEEvent(w, "content_block_start", map[string]interface{}{
								"type":  "content_block_start",
								"index": claudeIndex,
								"content_block": map[string]interface{}{
									"type":  "tool_use",
									"id":    toolCall.ID,
									"name":  toolCall.Name,
									"input": map[string]interface{}{},
								},
							})
							_ = w.Flush()
						}

						// Handle function arguments
						// Type assertion handles nil check, Started flag, and we process even empty strings
						if args, ok := functionData["arguments"].(string); ok && toolCall.Started {
							// Only accumulate if args is not empty
							if args != "" {
								toolCall.ArgsBuffer += args
							}

							// Try to parse complete JSON and send delta when we have valid JSON
							if toolCall.ArgsBuffer != "" {
								var jsonTest interface{}
								if err := json.Unmarshal([]byte(toolCall.ArgsBuffer), &jsonTest); err == nil {
									// If parsing succeeds and we haven't sent this JSON yet
									if !toolCall.JSONSent {
										writeSSEEvent(w, "content_block_delta", map[string]interface{}{
											"type":  "content_block_delta",
											"index": toolCall.ClaudeIndex,
											"delta": map[string]interface{}{
												"type":         "input_json_delta",
												"partial_json": toolCall.ArgsBuffer,
											},
										})
										_ = w.Flush()
										toolCall.JSONSent = true
									}
								}
							}
							// If JSON is incomplete, continue accumulating (no action needed)
						}
					}
				}
			}
		}

		// Handle finish reason
		// NOTE: Don't break here - with stream_options.include_usage, OpenAI sends usage in a chunk AFTER finish_reason
		if finishReason, ok := choice["finish_reason"].(string); ok && finishReason != "" {
			switch finishReason {
			case "length":
				finalStopReason = "max_tokens"
			case "tool_calls", "function_call":
				finalStopReason = "tool_use"
			case "stop":
				finalStopReason = "end_turn"
			default:
				finalStopReason = "end_turn"
			}
			// Continue processing to capture usage chunk (don't break)
		}
	}

	// Send final SSE events

	// Send content_block_stop for text block if it was started
	if textBlockStarted {
		writeSSEEvent(w, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": textBlockIndex,
		})
		_ = w.Flush()
	}

	// Send content_block_stop for each tool call
	for _, toolData := range currentToolCalls {
		// Check both Started AND claude_index is not None
		if toolData.Started && toolData.ClaudeIndex != 0 {
			writeSSEEvent(w, "content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": toolData.ClaudeIndex,
			})
			_ = w.Flush()
		}
	}

	// Send content_block_stop for thinking block if it had content
	if thinkingBlockStarted && thinkingBlockHasContent {
		writeSSEEvent(w, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": thinkingBlockIndex,
		})
		_ = w.Flush()
	}

	// Debug: Check if usage data was received
	if cfg.Debug {
		inputTokens, _ := usageData["input_tokens"].(int)
		outputTokens, _ := usageData["output_tokens"].(int)
		if inputTokens == 0 && outputTokens == 0 {
			fmt.Printf("[DEBUG] OpenRouter streaming: Usage data unavailable (expected limitation of streaming API)\n")
		}
	}

	// Send message_delta with stop_reason and accumulated usage data
	// NOTE: We send the actual accumulated usage to fix the "0 tokens" issue in Claude Code
	if cfg.Debug {
		usageDataJSON, _ := json.Marshal(usageData)
		fmt.Printf("[DEBUG] Sending message_delta with usageData: %s\n", string(usageDataJSON))
	}
	writeSSEEvent(w, "message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   finalStopReason,
			"stop_sequence": nil,
		},
		"usage": usageData,
	})
	_ = w.Flush()

	// Send message_stop
	writeSSEEvent(w, "message_stop", map[string]interface{}{
		"type": "message_stop",
	})
	_ = w.Flush()

	// Simple log: one-line summary
	if cfg.SimpleLog {
		inputTokens := 0
		outputTokens := 0

		// Try to extract tokens from various possible formats
		if val, ok := usageData["input_tokens"].(int); ok {
			inputTokens = val
		} else if val, ok := usageData["input_tokens"].(float64); ok {
			inputTokens = int(val)
		}

		if val, ok := usageData["output_tokens"].(int); ok {
			outputTokens = val
		} else if val, ok := usageData["output_tokens"].(float64); ok {
			outputTokens = int(val)
		}

		// Debug: show what we actually have in usageData
		if cfg.Debug {
			fmt.Printf("[DEBUG] usageData: %+v\n", usageData)
		}

		// Calculate tokens per second
		duration := time.Since(startTime).Seconds()
		tokensPerSec := 0.0
		if duration > 0 && outputTokens > 0 {
			tokensPerSec = float64(outputTokens) / duration
		}

		timestamp := time.Now().Format("15:04:05")
		fmt.Printf("[%s] [REQ] %s model=%s in=%d out=%d tok/s=%.1f\n",
			timestamp,
			cfg.OpenAIBaseURL,
			providerModel,
			inputTokens,
			outputTokens,
			tokensPerSec)
	}

	// Check for scanner errors
	if err := scanner.Err(); err != nil {
		// Log before writing the SSE error: the client reports only a one-line
		// "API Error", so this line is the only record of what actually broke.
		bytesIn, firstChunkAt, lastChunkAt := streamReport()
		fmt.Printf(
			"[%s] [STREAM-ERROR] tag=%s %s model=%s elapsed=%.1fs "+
				"bytes_from_upstream=%d events=%d first_chunk_after=%s "+
				"quiet_for=%s err=%v\n",
			time.Now().Format("15:04:05"), tag, cfg.OpenAIBaseURL, providerModel,
			time.Since(startTime).Seconds(), bytesIn, eventsIn,
			firstChunkLatency(firstChunkAt, startTime),
			quietFor(lastChunkAt), err,
		)
		writeSSEError(w, fmt.Sprintf("stream read error: %v", err))
	}
}

// truncateForLog bounds an upstream error body so one verbose HTML/JSON error
// page cannot swamp the proxy log.  Newlines are flattened because each log line
// is meant to stay one line, grep-able.
func truncateForLog(b []byte, max int) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > max {
		return s[:max] + "…(truncated)"
	}
	return s
}

// streamTag produces a short identifier for one request so its STREAM-OPEN,
// STREAM-ERROR and STREAM-CLOSE lines can be matched up.
//
// The proxy serves several concurrent streams against the same base_url and the
// same model name, so those two fields alone do not say which CLOSE belongs to
// which OPEN — the very pairing needed when two runs on one machine hit the
// disconnect at the same time.  Nanoseconds are unique per call and are already
// the seed of message_start's id, which ties a log line to the SSE the client
// actually received.
func streamTag(startTime time.Time) string {
	return fmt.Sprintf("r%x", startTime.UnixNano())
}

// firstChunkLatency reports how long the upstream stayed silent AFTER its
// headers arrived.  That duration is the one fact that separates the two
// indistinguishable "stream read error" failures: an upstream that produced
// nothing at all (headers then a broken/empty body) versus one that streamed
// normally and was cut off later.  Returns "never" when no body byte ever
// arrived.
func firstChunkLatency(firstChunkAt, startTime time.Time) string {
	if firstChunkAt.IsZero() {
		return "never"
	}
	return fmt.Sprintf("%.1fs", firstChunkAt.Sub(startTime).Seconds())
}

// quietFor is time.Since(lastRead) with the zero-value case handled.  A stream
// that never delivered a byte has no meaningful "last read" — subtracting the
// zero Time prints ~9.2e9 seconds (1970-01-01), which reads like a bug in the
// log rather than the fact it is.
func quietFor(lastRead time.Time) string {
	if lastRead.IsZero() {
		return "n/a"
	}
	return fmt.Sprintf("%.1fs", time.Since(lastRead).Seconds())
}

// countingReader records how many bytes the upstream has delivered and when the
// most recent read returned them, AND enforces an idle (no-data) timeout.
//
// The idle deadline is the streaming-correct replacement for
// http.Client.Timeout, which cannot be used here because it caps total duration
// rather than silence. Resetting the timer on every successful read means a long
// generation is never cut off while a stalled one still fails promptly.
//
// The byte counters serve a second purpose: the last-read timestamp is what
// separates "slow but healthy" from "went silent". A stream that breaks after
// 300s of steady chunks is a different fault from one that breaks after 300s of
// nothing, but both surface as the same "context deadline exceeded" text at the
// client.
type countingReader struct {
	r io.Reader

	// mu guards the fields below. They are written by the read goroutine, which
	// can outlive Read() after an idle timeout, and read by the caller's
	// STREAM-CLOSE log line — genuinely concurrent, so they need a lock rather
	// than the "only one goroutine touches them" assumption that held before the
	// idle timer introduced a second one.
	mu       sync.Mutex
	n        int64
	last     time.Time
	first    time.Time
	detached bool // an abandoned read may still update the counters

	idle time.Duration
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.idle <= 0 {
		n, err := c.r.Read(p)
		c.record(n)
		return n, err
	}

	// Read in a goroutine so the timer can fire while the upstream is silent.
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := c.r.Read(p)
		c.record(n)
		done <- result{n, err}
	}()

	timer := time.NewTimer(c.idle)
	defer timer.Stop()
	select {
	case res := <-done:
		return res.n, res.err
	case <-timer.C:
		// The abandoned Read still owns p. Reading it again would race with the
		// goroutine above writing into the same buffer — and after the caller
		// has already turned this into an error, nothing more should be read
		// anyway. Latch that so the returned error is final.
		c.mu.Lock()
		c.detached = true
		received := c.n
		c.mu.Unlock()
		return 0, fmt.Errorf(
			"upstream sent no data for %s (idle timeout, %d bytes received so far)",
			c.idle, received,
		)
	}
}

func (c *countingReader) record(n int) {
	if n <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n += int64(n)
	now := time.Now()
	c.last = now
	if c.first.IsZero() {
		c.first = now
	}
}

// snapshot returns the byte total and read timestamps safely, even while an
// abandoned read goroutine is still running.
func (c *countingReader) snapshot() (n int64, first, last time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n, c.first, c.last
}

// writeSSEEvent writes a Server-Sent Event
func writeSSEEvent(w *bufio.Writer, event string, data interface{}) {
	dataJSON, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "event: %s\n", event)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", string(dataJSON))
}

// writeSSEError writes an error event
func writeSSEError(w *bufio.Writer, message string) {
	writeSSEEvent(w, "error", map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    "api_error",
			"message": message,
		},
	})
	_ = w.Flush()
}

// callOpenAI makes an HTTP request to the OpenAI API with automatic retry logic
// for max_completion_tokens parameter errors. Uses per-model capability caching.
func callOpenAI(req *models.OpenAIRequest, cfg *config.Config) (*models.OpenAIResponse, error) {
	// Try the request with the configured parameters
	resp, err := callOpenAIInternal(req, cfg)
	if err != nil {
		// Check if this is a max_tokens parameter error
		if isMaxTokensParameterError(err.Error()) {
			if cfg.Debug {
				fmt.Printf("[DEBUG] Detected max_completion_tokens parameter error for model %s, retrying without it\n", req.Model)
			}
			// Retry without max_completion_tokens and cache the capability per model
			return retryWithoutMaxCompletionTokens(req, cfg)
		}
		// Other errors - return as-is
		return nil, err
	}

	// Success on first try - cache that this (provider, model) supports max_completion_tokens
	// Only cache if we actually sent max_completion_tokens
	if req.MaxCompletionTokens > 0 {
		cacheKey := config.CacheKey{
			BaseURL: cfg.OpenAIBaseURL,
			Model:   req.Model,
		}
		config.SetModelCapabilities(cacheKey, &config.ModelCapabilities{
			UsesMaxCompletionTokens: true,
		})
		if cfg.Debug {
			fmt.Printf("[DEBUG] Cached: model %s supports max_completion_tokens\n", req.Model)
		}
	}

	return resp, nil
}

// callOpenAIStream makes a streaming HTTP request with retry logic for parameter errors.
// Uses per-model capability caching.
func callOpenAIStream(req *models.OpenAIRequest, cfg *config.Config) (*http.Response, error) {
	// Try with configured parameters
	resp, err := callOpenAIStreamInternal(req, cfg)
	if err != nil {
		// Check if this is a max_tokens parameter error
		if isMaxTokensParameterError(err.Error()) {
			if cfg.Debug {
				fmt.Printf("[DEBUG] Detected max_completion_tokens parameter error in stream for model %s, retrying without it\n", req.Model)
			}
			// Create retry request without max tokens
			retryReq := *req
			retryReq.MaxCompletionTokens = 0
			retryReq.MaxTokens = 0

			// Cache that this (provider, model) doesn't support max_completion_tokens
			cacheKey := config.CacheKey{
				BaseURL: cfg.OpenAIBaseURL,
				Model:   req.Model,
			}
			config.SetModelCapabilities(cacheKey, &config.ModelCapabilities{
				UsesMaxCompletionTokens: false,
			})

			return callOpenAIStreamInternal(&retryReq, cfg)
		}
		return nil, err
	}

	// Success - cache capability if we sent max_completion_tokens
	if req.MaxCompletionTokens > 0 {
		cacheKey := config.CacheKey{
			BaseURL: cfg.OpenAIBaseURL,
			Model:   req.Model,
		}
		config.SetModelCapabilities(cacheKey, &config.ModelCapabilities{
			UsesMaxCompletionTokens: true,
		})
		if cfg.Debug {
			fmt.Printf("[DEBUG] Cached: model %s supports max_completion_tokens (streaming)\n", req.Model)
		}
	}

	return resp, nil
}

// callOpenAIStreamInternal makes a streaming HTTP request without retry logic
func callOpenAIStreamInternal(req *models.OpenAIRequest, cfg *config.Config) (*http.Response, error) {
	// Marshal request to JSON
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Build API URL
	apiURL := cfg.OpenAIBaseURL + "/chat/completions"

	// Create HTTP request
	httpReq, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	httpReq.Header.Set("Content-Type", "application/json")

	// Skip auth for Ollama (localhost)
	if !cfg.IsLocalhost() {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.OpenAIAPIKey)
	}

	// OpenRouter-specific headers
	if cfg.DetectProvider() == config.ProviderOpenRouter {
		addOpenRouterHeaders(httpReq, cfg)
	}

	// No overall Timeout here — deliberately.
	//
	// http.Client.Timeout covers the ENTIRE exchange, response-body reads
	// included. On a streaming endpoint the body IS the response: a healthy
	// generation that keeps producing tokens for longer than the budget is cut
	// off mid-sentence. Measured locally against an upstream emitting one chunk
	// every 2s, the stream died at exactly 300.0s with
	// "context deadline exceeded (Client.Timeout ... while reading body)" and
	// quiet_for=0.8s — i.e. the connection was perfectly alive and still got
	// killed. The same thing happened in production runs: three calls ended at
	// 300.4-300.5s with zero tokens, which the client can only report as
	// "stream read error". Generation calls routinely run 345s, 395s, 552s.
	//
	// The two timeouts such a client actually needs are split instead:
	//   - time-to-first-byte is bounded by streamingResponseHeaderTimeout below
	//     (via a custom Transport), so a dead upstream still fails fast;
	//   - a mid-stream stall is bounded by streamIdleTimeout in
	//     streamOpenAIToClaude, which only fires when the upstream goes quiet —
	//     a slow-but-healthy generation is never punished for being long.
	client := newStreamingClient()

	// Make request
	startWait := time.Now()
	resp, err := client.Do(httpReq)
	if err != nil {
		// Log the upstream-side failure explicitly.  Until now a request that
		// never got headers produced no proxy log line at all (the success-only
		// [REQ] summary cannot fire), so "upstream never answered" and "upstream
		// answered and the body broke" were indistinguishable in the logs.
		fmt.Printf(
			"[%s] [UPSTREAM-ERROR] %s model=%s waited=%.1fs err=%v\n",
			time.Now().Format("15:04:05"), cfg.OpenAIBaseURL, req.Model,
			time.Since(startWait).Seconds(), err,
		)
		return nil, fmt.Errorf("request failed: %w", err)
	}

	// Check for errors
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		fmt.Printf(
			"[%s] [UPSTREAM-STATUS] %s model=%s status=%d waited=%.1fs body=%s\n",
			time.Now().Format("15:04:05"), cfg.OpenAIBaseURL, req.Model,
			resp.StatusCode, time.Since(startWait).Seconds(), truncateForLog(body, 300),
		)
		return nil, newUpstreamError(resp.StatusCode, body)
	}

	return resp, nil
}

// isMaxTokensParameterError checks if the error message indicates an unsupported
// max_tokens or max_completion_tokens parameter issue.
// Uses broad keyword matching to handle different error message formats across providers.
// No status code checking - relies on message content alone.
func isMaxTokensParameterError(errorMessage string) bool {
	errorLower := strings.ToLower(errorMessage)

	// Check for parameter error indicators
	hasParamIndicator := strings.Contains(errorLower, "parameter") ||
		strings.Contains(errorLower, "unsupported") ||
		strings.Contains(errorLower, "invalid")

	// Check for our specific parameter names
	hasOurParam := strings.Contains(errorLower, "max_tokens") ||
		strings.Contains(errorLower, "max_completion_tokens")

	// Require both indicators to reduce false positives
	return hasParamIndicator && hasOurParam
}

// retryWithoutMaxCompletionTokens attempts the request again without max_completion_tokens.
// Caches the result per (provider, model) combination for future requests.
func retryWithoutMaxCompletionTokens(req *models.OpenAIRequest, cfg *config.Config) (*models.OpenAIResponse, error) {
	// Create a copy of the request without max_completion_tokens
	retryReq := *req
	retryReq.MaxCompletionTokens = 0
	retryReq.MaxTokens = 0 // Also clear max_tokens to avoid issues

	if cfg.Debug {
		fmt.Printf("[DEBUG] Retrying without max_completion_tokens/max_tokens for model: %s\n", req.Model)
	}

	// Cache that this specific (provider, model) doesn't support max_completion_tokens
	cacheKey := config.CacheKey{
		BaseURL: cfg.OpenAIBaseURL,
		Model:   req.Model,
	}
	config.SetModelCapabilities(cacheKey, &config.ModelCapabilities{
		UsesMaxCompletionTokens: false,
	})

	// Make the retry request
	return callOpenAIInternal(&retryReq, cfg)
}

// callOpenAIInternal is the internal implementation without retry logic
func callOpenAIInternal(req *models.OpenAIRequest, cfg *config.Config) (*models.OpenAIResponse, error) {
	// Marshal request to JSON
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Build API URL
	apiURL := cfg.OpenAIBaseURL + "/chat/completions"

	// Create HTTP request
	httpReq, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	httpReq.Header.Set("Content-Type", "application/json")

	// Skip auth for Ollama (localhost) - Ollama doesn't require authentication
	if !cfg.IsLocalhost() {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.OpenAIAPIKey)
	}

	// OpenRouter-specific headers for better rate limits
	if cfg.DetectProvider() == config.ProviderOpenRouter {
		addOpenRouterHeaders(httpReq, cfg)
	}

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 90 * time.Second,
	}

	// Make request
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Check for errors
	if resp.StatusCode != http.StatusOK {
		return nil, newUpstreamError(resp.StatusCode, respBody)
	}

	// Parse response
	var openaiResp models.OpenAIResponse
	if err := json.Unmarshal(respBody, &openaiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &openaiResp, nil
}

func handleCountTokens(c *fiber.Ctx, cfg *config.Config) error {
	// Simple token counting endpoint
	return c.JSON(fiber.Map{
		"input_tokens": 100,
	})
}
