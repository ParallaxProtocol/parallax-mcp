package status

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ParallaxProtocol/parallax-mcp/pkg/httpx"
)

func server(t *testing.T, fail *atomic.Bool, hits *atomic.Int32, delay time.Duration) *httptest.Server {
	body, err := os.ReadFile("../../tools/network/testdata/stats.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(delay)
		if fail.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDecodesRealResponse(t *testing.T) {
	var fail atomic.Bool
	var hits atomic.Int32
	c := New(httpx.Options{BaseURL: server(t, &fail, &hits, 0).URL, Timeout: time.Second}, time.Minute)
	snap, err := c.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := snap.Stats
	if s.BestBlock == nil || s.BestBlock.Number != 59636 || s.Decentralization.Grade != "critical" || len(s.NodeList) != 5 || len(s.Alerts) != 2 {
		t.Fatalf("decoded %+v", s)
	}
	if s.Confirmations == nil || len(s.Confirmations.Levels) != 3 || s.Incidents == nil || len(s.Incidents.List) != 1 {
		t.Fatalf("confirmations/incidents: %+v %+v", s.Confirmations, s.Incidents)
	}
}

func TestConcurrentMissesShareOneRequest(t *testing.T) {
	var fail atomic.Bool
	var hits atomic.Int32
	c := New(httpx.Options{BaseURL: server(t, &fail, &hits, 50*time.Millisecond).URL, Timeout: time.Second}, time.Minute)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Stats(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Fatalf("want 1 request, got %d", hits.Load())
	}
}

func TestServesStaleCopyWhenDown(t *testing.T) {
	var fail atomic.Bool
	var hits atomic.Int32
	c := New(httpx.Options{BaseURL: server(t, &fail, &hits, 0).URL, Timeout: time.Second}, time.Second)
	now := time.Now()
	c.now = func() time.Time { return now }
	if _, err := c.Stats(context.Background()); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	now = now.Add(2 * time.Second) // past the TTL
	snap, err := c.Stats(context.Background())
	if err != nil || !snap.Stale || snap.Error == nil {
		t.Fatalf("want a stale copy, got %+v, %v", snap, err)
	}
	now = now.Add(maxStale) // too old to serve
	if _, err := c.Stats(context.Background()); err == nil {
		t.Fatal("want an error once the copy is too old")
	}
}

func TestCallerCancellationDoesNotPoisonSharedFetch(t *testing.T) {
	var fail atomic.Bool
	var hits atomic.Int32
	c := New(httpx.Options{BaseURL: server(t, &fail, &hits, 100*time.Millisecond).URL, Timeout: time.Second}, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.Stats(ctx); err == nil {
		t.Fatal("want the caller's deadline")
	}
	if _, err := c.Stats(context.Background()); err != nil {
		t.Fatalf("second caller: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("the shared fetch should have completed once, got %d requests", hits.Load())
	}
}
