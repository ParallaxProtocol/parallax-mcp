package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetriesTransientFailures(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 3 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := New(Options{Name: "t", BaseURL: srv.URL, Timeout: time.Second, Retries: 2, BaseDelay: time.Millisecond})
	var out struct{ OK bool }
	if err := c.GetJSON(context.Background(), "/", nil, &out); err != nil || !out.OK || hits.Load() != 3 {
		t.Fatalf("err %v, ok %v, hits %d", err, out.OK, hits.Load())
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	c := New(Options{Name: "t", BaseURL: srv.URL, Timeout: time.Second, Retries: 3, BaseDelay: time.Millisecond})
	err := c.GetJSON(context.Background(), "/", nil, &struct{}{})
	if !NotFound(err) || hits.Load() != 1 {
		t.Fatalf("err %v, hits %d", err, hits.Load())
	}
}

func TestResponseSizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 2048)))
	}))
	defer srv.Close()
	c := New(Options{Name: "t", BaseURL: srv.URL, Timeout: time.Second, MaxBody: 1024})
	if _, err := c.Get(context.Background(), "/", nil); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("got %v", err)
	}
}

func TestPerAttemptTimeoutAndCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	c := New(Options{Name: "t", BaseURL: srv.URL, Timeout: 50 * time.Millisecond, Retries: 1, BaseDelay: time.Millisecond})
	start := time.Now()
	if _, err := c.Get(context.Background(), "/", nil); err == nil {
		t.Fatal("want a timeout")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %s", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Get(ctx, "/", nil); err == nil {
		t.Fatal("want cancellation")
	}
}
