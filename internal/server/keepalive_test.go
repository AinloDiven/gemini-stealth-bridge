package server

import (
	"bytes"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// safeBuffer is a bytes.Buffer that tolerates concurrent writes, so a stray
// pinger goroutine surfaces as a failed assertion instead of a data race.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// lateFlusher fails loudly if the response is flushed after the handler is gone.
// net/http nils the bufio.Writer at that point, so a late Flush is a real panic
// in production.
type lateFlusher struct {
	flushes atomic.Int32
	after   atomic.Bool
}

func (f *lateFlusher) Flush() {
	if f.after.Load() {
		panic("Flush called after handler return")
	}
	f.flushes.Add(1)
}

var _ http.Flusher = (*lateFlusher)(nil)

func TestStreamPingerStopWaitsForGoroutine(t *testing.T) {
	var mu sync.Mutex
	var buf safeBuffer
	flusher := &lateFlusher{}
	p := startStreamPinger(&mu, &buf, flusher, time.Millisecond, nil)

	time.Sleep(20 * time.Millisecond)
	p.Stop()

	// After Stop returns the pinger must be gone: mark the writer as torn down
	// and give a stray goroutine a chance to blow up.
	before := flusher.flushes.Load()
	flusher.after.Store(true)
	time.Sleep(30 * time.Millisecond)

	if got := flusher.flushes.Load(); got != before {
		t.Fatalf("pinger flushed after Stop returned: %d -> %d", before, got)
	}
	if buf.Len() == 0 {
		t.Fatal("expected at least one keepalive comment")
	}
}

func TestStreamPingerStopIsIdempotent(t *testing.T) {
	var mu sync.Mutex
	var buf safeBuffer
	p := startStreamPinger(&mu, &buf, nil, time.Hour, nil)
	p.Stop()
	p.Stop() // closing twice must not panic
	var nilPinger *streamPinger
	nilPinger.Stop()
}
