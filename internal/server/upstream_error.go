package server

import (
	"fmt"
	"net/http"
)

// UpstreamError carries the HTTP status an OpenAI-format upstream returned,
// together with its raw response body.
//
// Why this type exists: the proxy used to flatten every upstream failure into
// `fmt.Errorf("OpenAI API returned status %d: %s", ...)`, and the non-streaming
// handler then answered a hard-coded 500. That erased the status code, and a
// quota rejection (403 + pre_consume_token_quota_failed) reached Claude Code as
// a 5xx — which Claude Code retries with exponential backoff. In the field that
// cost ~181s and 12 requests per call before the agent gave up.
//
// Keeping the status lets the caller forward it verbatim: 401 and 403 are not
// retried by Claude Code, so a dead quota fails in about a second.
type UpstreamError struct {
	// Status is the HTTP status the upstream answered with.
	Status int
	// Body is the upstream response body, unmodified. Callers may parse it or
	// forward it; it is what carries wording like "token quota is not enough"
	// that downstream quota detection matches on.
	Body string
}

// Error renders the upstream failure in the same shape this proxy has always
// logged and reported ("OpenAI API returned status N: body"), so existing logs
// and human-facing messages do not change.
func (e *UpstreamError) Error() string {
	return fmt.Sprintf("OpenAI API returned status %d: %s", e.Status, e.Body)
}

// newUpstreamError builds an UpstreamError for a non-200 upstream response.
func newUpstreamError(status int, body []byte) *UpstreamError {
	return &UpstreamError{Status: status, Body: string(body)}
}

// downstreamStatus maps an upstream status onto the status this proxy reports to
// its own client.
//
// The pass-through is only sound for statuses that mean the same thing to both
// sides; anything the upstream reports must remain in the class Claude Code
// reacts to (4xx not retried, 429 and 5xx retried). Today every status the
// OpenAI-format gateways we support emit is shared with the Anthropic API, so
// this is the identity. It exists as a single choke point so a future mismatch
// (e.g. an upstream that answers 418 where Anthropic has no equivalent) is a
// one-line change instead of a hunt.
func downstreamStatus(upstream int) int {
	if upstream < 400 || upstream > 599 {
		// A non-error status arriving here means the upstream answered
		// something we did not expect (e.g. a 3xx redirect); do not invent a
		// success. Report it as a bad gateway so it is visible.
		return http.StatusBadGateway
	}
	return upstream
}
