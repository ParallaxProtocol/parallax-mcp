// Package httpx is the HTTP client the explorer and status backends share:
// per-request timeouts, retries with exponential backoff and jitter for
// idempotent requests, Retry-After support, context cancellation and a cap
// on response size.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ParallaxProtocol/parallax-mcp/pkg/metrics"
)

// DefaultMaxBody is the largest response body read, in bytes.
const DefaultMaxBody = 8 << 20

// Options configures a Client.
type Options struct {
	// Name labels metrics and errors, such as "explorer".
	Name string
	// BaseURL is prepended to every request path.
	BaseURL string
	// Timeout bounds each attempt.
	Timeout time.Duration
	// Retries is the number of extra attempts after a retryable failure.
	Retries int
	// MaxBody caps the response size; DefaultMaxBody when zero.
	MaxBody int64
	// UserAgent is sent with every request.
	UserAgent string
	// HTTPClient overrides the transport, for tests.
	HTTPClient *http.Client
	// BaseDelay is the first backoff delay; 250ms when zero.
	BaseDelay time.Duration
}

// Client performs GET requests against one base URL.
type Client struct {
	opts Options
	http *http.Client
}

// New returns a Client.
func New(opts Options) *Client {
	if opts.MaxBody == 0 {
		opts.MaxBody = DefaultMaxBody
	}
	if opts.BaseDelay == 0 {
		opts.BaseDelay = 250 * time.Millisecond
	}
	opts.BaseURL = strings.TrimRight(opts.BaseURL, "/")
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{opts: opts, http: hc}
}

// StatusError is a non-2xx response.
type StatusError struct {
	Backend string
	Code    int
	Body    string
}

func (e *StatusError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("%s: HTTP %d: %s", e.Backend, e.Code, e.Body)
	}
	return fmt.Sprintf("%s: HTTP %d", e.Backend, e.Code)
}

// NotFound reports whether err is a 404 from a backend.
func NotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

// GetJSON fetches path with the query parameters and decodes the JSON body
// into out.
func (c *Client) GetJSON(ctx context.Context, path string, query url.Values, out any) error {
	body, err := c.Get(ctx, path, query)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: decoding %s: %w", c.opts.Name, path, err)
	}
	return nil
}

// Get fetches path and returns the body of a 2xx response.
func (c *Client) Get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	u := c.opts.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	start := time.Now()
	defer func() { metrics.BackendLatency(c.opts.Name, time.Since(start)) }()

	var lastErr error
	for attempt := 0; attempt <= c.opts.Retries; attempt++ {
		if attempt > 0 {
			metrics.Backend(c.opts.Name, "retry")
			if err := sleep(ctx, c.backoff(attempt, lastErr)); err != nil {
				return nil, lastErr
			}
		}
		body, retry, err := c.once(ctx, u)
		if err == nil {
			metrics.Backend(c.opts.Name, "ok")
			return body, nil
		}
		lastErr = err
		if !retry || ctx.Err() != nil {
			break
		}
	}
	metrics.Backend(c.opts.Name, "error")
	return nil, lastErr
}

// once makes a single attempt and reports whether a failure is worth
// retrying: network errors, timeouts, 429 and 5xx are; other statuses are not.
func (c *Client) once(ctx context.Context, u string) ([]byte, bool, error) {
	actx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(actx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", c.opts.Name, err)
	}
	req.Header.Set("Accept", "application/json")
	if c.opts.UserAgent != "" {
		req.Header.Set("User-Agent", c.opts.UserAgent)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, fmt.Errorf("%s: %w", c.opts.Name, ctx.Err())
		}
		return nil, true, fmt.Errorf("%s: %w", c.opts.Name, redactURL(err))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, c.opts.MaxBody+1))
	if err != nil {
		return nil, true, fmt.Errorf("%s: reading response: %w", c.opts.Name, err)
	}
	if int64(len(body)) > c.opts.MaxBody {
		return nil, false, fmt.Errorf("%s: response larger than %d bytes", c.opts.Name, c.opts.MaxBody)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, false, nil
	}
	se := &StatusError{Backend: c.opts.Name, Code: resp.StatusCode, Body: snippet(body)}
	retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
	if ra := resp.Header.Get("Retry-After"); retry && ra != "" {
		return nil, retry, &retryAfterError{StatusError: se, after: parseRetryAfter(ra)}
	}
	return nil, retry, se
}

type retryAfterError struct {
	*StatusError
	after time.Duration
}

func (e *retryAfterError) Unwrap() error { return e.StatusError }

func (c *Client) backoff(attempt int, lastErr error) time.Duration {
	var ra *retryAfterError
	if errors.As(lastErr, &ra) && ra.after > 0 {
		// Honour the server, within reason: a tool call cannot wait a minute.
		return min(ra.after, 5*time.Second)
	}
	d := c.opts.BaseDelay << (attempt - 1)
	return d/2 + rand.N(d/2+1)
}

func parseRetryAfter(v string) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// snippet keeps error bodies short; they come from a remote server.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// redactURL strips the query string from url.Error messages.
func redactURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil {
			u.RawQuery = ""
			ue.URL = u.String()
		}
	}
	return err
}
