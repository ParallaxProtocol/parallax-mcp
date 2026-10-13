package transport

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// keepAliveInterval is how long an event stream may stay silent before a
// comment line is sent. Proxies close idle responses: Cloudflare after 100
// seconds, many load balancers after 60.
var keepAliveInterval = 25 * time.Second

// keepAlive wraps the response writer of an MCP request. When the response
// is a server-sent event stream (a waiting tool call, or the stream that
// carries notifications), it writes an SSE comment, which clients ignore,
// whenever nothing was written for keepAliveInterval. The SDK writes each
// event with a single Write, and the mutex keeps comments between events.
type keepAlive struct {
	http.ResponseWriter
	mu      sync.Mutex
	last    time.Time
	started bool
	stopped bool
	done    chan struct{}
	wg      sync.WaitGroup
}

func newKeepAlive(w http.ResponseWriter) *keepAlive {
	return &keepAlive{ResponseWriter: w, done: make(chan struct{})}
}

func (k *keepAlive) WriteHeader(code int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.ResponseWriter.WriteHeader(code)
	k.last = time.Now()
	if !k.started && code == http.StatusOK && strings.HasPrefix(k.Header().Get("Content-Type"), "text/event-stream") {
		k.started = true
		k.wg.Add(1)
		go k.loop()
	}
}

func (k *keepAlive) Write(b []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.last = time.Now()
	return k.ResponseWriter.Write(b)
}

// Flush is defined here, not reached through Unwrap, so that flushes are
// serialized with the keep-alive writes.
func (k *keepAlive) Flush() {
	k.mu.Lock()
	defer k.mu.Unlock()
	_ = http.NewResponseController(k.ResponseWriter).Flush()
}

func (k *keepAlive) Unwrap() http.ResponseWriter { return k.ResponseWriter }

// stop ends the keep-alive loop and waits for it, so nothing is written
// after the handler returns and net/http finishes the response.
func (k *keepAlive) stop() {
	k.mu.Lock()
	k.stopped = true
	k.mu.Unlock()
	close(k.done)
	k.wg.Wait()
}

func (k *keepAlive) loop() {
	defer k.wg.Done()
	t := time.NewTicker(keepAliveInterval / 5)
	defer t.Stop()
	for {
		select {
		case <-k.done:
			return
		case <-t.C:
			k.mu.Lock()
			if k.stopped {
				k.mu.Unlock()
				return
			}
			if time.Since(k.last) >= keepAliveInterval {
				if _, err := k.ResponseWriter.Write([]byte(": keepalive\n\n")); err != nil {
					k.mu.Unlock()
					return
				}
				_ = http.NewResponseController(k.ResponseWriter).Flush()
				k.last = time.Now()
			}
			k.mu.Unlock()
		}
	}
}
