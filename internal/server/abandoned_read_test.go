package server

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

// TestAbandonedReadUnblocksWhenBodyClosed answers a question the idle-timeout
// implementation depends on: countingReader.Read abandons a blocked
// r.Read in a goroutine when the idle timer fires. If closing the response body
// afterwards does NOT unblock that goroutine, every timeout leaks one goroutine
// (plus its read buffer) for the life of the proxy process.
//
// This is a behaviour claim about net/http, not about our code, so it is tested
// directly rather than asserted in a comment.
func TestAbandonedReadUnblocksWhenBodyClosed(t *testing.T) {
	hold := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			_, _ = w.Write([]byte("data: hello\n\n"))
			f.Flush()
		}
		<-hold
	}))
	t.Cleanup(func() { close(hold); upstream.Close() })

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// Consume the one chunk, then block on a read that will never return data.
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("first read: %v", err)
	}

	exited := make(chan struct{})
	go func() {
		defer close(exited)
		blocked := make([]byte, 64)
		_, _ = resp.Body.Read(blocked) // blocks until the body is closed
	}()

	time.Sleep(50 * time.Millisecond)
	select {
	case <-exited:
		t.Fatal("read returned before the body was closed; test premise is wrong")
	default:
	}

	_ = resp.Body.Close()

	select {
	case <-exited:
		// The abandoned read unblocked: no leak.
	case <-time.After(3 * time.Second):
		t.Fatalf("read still blocked 3s after Close; goroutines=%d", runtime.NumGoroutine())
	}
}
