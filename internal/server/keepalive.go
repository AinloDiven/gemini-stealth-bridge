package server

import (
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// defaultKeepaliveInterval is the SSE comment cadence used while the upstream is
// still thinking. Tests lower it to exercise the shutdown path quickly.
var defaultKeepaliveInterval = 15 * time.Second

// streamPinger writes SSE keepalive comments on a ticker.
//
// Stop must be called before the handler returns. net/http nils the response
// writer once the handler returns, so a late write or flush dereferences a nil
// bufio.Writer inside Flush and takes the whole process down: the panic happens
// on a non-handler goroutine, where net/http's recover does not apply. Stop
// therefore waits for the pinger to exit instead of only signalling it.
type streamPinger struct {
	mu      *sync.Mutex
	w       io.Writer
	flusher http.Flusher
	onPing  func()

	stop    chan struct{}
	done    chan struct{}
	stopOne sync.Once
	stopped atomic.Bool
}

// startStreamPinger starts the ticker. mu must be the same mutex the handler
// uses for response writes so a ping cannot interleave with a data chunk.
func startStreamPinger(mu *sync.Mutex, w io.Writer, flusher http.Flusher, interval time.Duration, onPing func()) *streamPinger {
	if interval <= 0 {
		interval = defaultKeepaliveInterval
	}
	p := &streamPinger{
		mu:      mu,
		w:       w,
		flusher: flusher,
		onPing:  onPing,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ticker.C:
				p.mu.Lock()
				if p.stopped.Load() {
					p.mu.Unlock()
					return
				}
				_, _ = io.WriteString(p.w, ": ping\n\n")
				if p.flusher != nil {
					p.flusher.Flush()
				}
				p.mu.Unlock()
				if p.onPing != nil {
					p.onPing()
				}
			}
		}
	}()
	return p
}

// Stop halts the ticker and blocks until the pinger goroutine has exited, so no
// write can land after the handler returns.
func (p *streamPinger) Stop() {
	if p == nil {
		return
	}
	p.stopOne.Do(func() {
		p.stopped.Store(true)
		close(p.stop)
		<-p.done
	})
}
