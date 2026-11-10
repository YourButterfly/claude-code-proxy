package server

import (
	"bufio"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// failingWriter is a downstream that has gone away: every write fails the way a
// closed socket does.
type failingWriter struct {
	flushErr error
}

func (w *failingWriter) Write(p []byte) (int, error) {
	return 0, errors.New("write: broken pipe")
}

func (w *failingWriter) Flush() error { return w.flushErr }

// TestUpstreamReadStopsWhenDownstreamIsGone pins the resource-leak half of the
// idle-timeout change.
//
// CountingReader now owns the only bound on upstream body reads. If the loop
// kept calling Scan() after the downstream client disconnected, the proxy would
// keep pulling from the upstream (and keep buffering) for as long as the model
// kept generating, once per abandoned request. Before this change
// http.Client.Timeout capped that at 300s; now nothing else would.
func TestUpstreamReadStopsWhenDownstreamIsGone(t *testing.T) {
	cfg := newTestConfig("http://127.0.0.1:1")
	reader := &endlessReader{}
	w := &failingWriter{}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// A long idle window: if the loop depended on the idle timer to stop, it
		// would block for this long.
		streamOpenAIToClaudeWithIdle(
			bufio.NewWriter(w), reader, "test-model", cfg, time.Now(), time.Hour,
		)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("streaming loop did not stop after downstream writes failed; " +
			"it would leak one upstream connection per abandoned request")
	}
}

// endlessReader yields a fresh SSE chunk forever, never going idle.
type endlessReader struct{}

func (e *endlessReader) Read(p []byte) (int, error) {
	n := copy(p, sseChunk)
	return n, nil
}

// lateReader answers each read only after the supplied delay, so the idle timer
// reliably fires first and the read goroutine is left behind — the exact
// situation that made the counters concurrently accessed.
type lateReader struct {
	delay  time.Duration
	reads  atomic.Int64
	closed chan struct{}
}

func (r *lateReader) Read(p []byte) (int, error) {
	r.reads.Add(1)
	select {
	case <-time.After(r.delay):
		n := copy(p, sseChunk)
		return n, nil
	case <-r.closed:
		return 0, errors.New("reader closed")
	}
}

// TestIdleTimeoutCountersAreRaceFree hammers the abandoned-read path, because
// the race it guards is timing-dependent and the normal suite never reaches it:
// a read goroutine must still be in flight when STREAM-CLOSE samples the byte
// counters. Run under -race.
func TestIdleTimeoutCountersAreRaceFree(t *testing.T) {
	cfg := newTestConfig("http://127.0.0.1:1")

	for i := 0; i < 40; i++ {
		reader := &lateReader{delay: 5 * time.Millisecond, closed: make(chan struct{})}
		var out strings.Builder
		start := time.Now()
		// Idle window shorter than the read latency: every read times out and
		// leaves its goroutine running.
		streamOpenAIToClaudeWithIdle(
			bufio.NewWriter(&out), reader, "test-model", cfg, start, time.Millisecond,
		)
		if reader.reads.Load() == 0 {
			t.Fatalf("iteration %d: reader never read", i)
		}
		if !strings.Contains(out.String(), "idle timeout") {
			t.Fatalf("iteration %d: expected an idle-timeout error", i)
		}
		close(reader.closed)
	}

	// Let the abandoned goroutines finish writing before the test ends, so
	// -race reports on this test rather than on the next one.
	time.Sleep(50 * time.Millisecond)
}
