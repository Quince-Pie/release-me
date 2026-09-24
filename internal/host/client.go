package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// APIError is a non-2xx response.
type APIError struct {
	Method  string
	URL     string
	Status  int
	Message string
	Body    string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = strings.TrimSpace(e.Body)
		if len(msg) > 200 {
			msg = msg[:200] + "…"
		}
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.URL, e.Status, msg)
}

// IsStatus reports whether err is an APIError with the given status.
func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

// Client is the shared HTTP layer: authentication, JSON, bounded retries
// with backoff for idempotent requests, and rate-limit waits. Non-idempotent
// requests are never retried here; their callers resolve uncertain outcomes
// by re-reading server state.
type Client struct {
	HTTP      *http.Client
	Token     string
	AuthStyle string // "Bearer" (GitHub) or "token" (Forgejo)
	UserAgent string
	Headers   map[string]string
	Clock     Clock
	// MaxAttempts bounds retries of idempotent requests (default 5).
	MaxAttempts int
	// MaxWait bounds a single rate-limit or backoff wait (default 2 minutes).
	MaxWait time.Duration
	Logf    func(format string, args ...any)
}

func (c *Client) attempts() int {
	if c.MaxAttempts <= 0 {
		return 5
	}
	return c.MaxAttempts
}

func (c *Client) maxWait() time.Duration {
	if c.MaxWait <= 0 {
		return 2 * time.Minute
	}
	return c.MaxWait
}

func (c *Client) clock() Clock {
	if c.Clock.Now == nil {
		return RealClock
	}
	return c.Clock
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func (c *Client) prepare(req *http.Request) {
	if c.Token != "" {
		style := c.AuthStyle
		if style == "" {
			style = "Bearer"
		}
		req.Header.Set("Authorization", style+" "+c.Token)
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	for k, v := range c.Headers {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}
}

// Do performs one attempt without retries.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	c.prepare(req)
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	return h.Do(req)
}

// retryable reports whether a failed attempt may be repeated for an
// idempotent request, and how long to wait first.
func (c *Client) retryable(resp *http.Response, err error) (bool, time.Duration) {
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return true, 0
		}
		return false, 0
	}
	switch {
	case resp.StatusCode >= 500:
		return true, 0
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0":
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil {
				return true, time.Duration(secs) * time.Second
			}
		}
		if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
			if secs, err := strconv.ParseInt(reset, 10, 64); err == nil {
				return true, time.Until(time.Unix(secs, 0)) + time.Second
			}
		}
		return true, 30 * time.Second
	}
	return false, 0
}

// DoIdempotent performs req with bounded retries. body, if non-nil, is
// re-sent on each attempt.
func (c *Client) DoIdempotent(ctx context.Context, method, url string, body []byte, contentType string) (*http.Response, error) {
	var lastErr error
	backoff := time.Second
	for attempt := 1; attempt <= c.attempts(); attempt++ {
		var r io.Reader
		if body != nil {
			r = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, r)
		if err != nil {
			return nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := c.Do(req)
		ok, wait := c.retryable(resp, err)
		if !ok {
			return resp, err
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = readError(req, resp)
		}
		if attempt == c.attempts() {
			break
		}
		if wait == 0 {
			wait = backoff
			backoff *= 2
		}
		if wait > c.maxWait() {
			return nil, fmt.Errorf("%w (would need to wait %s)", lastErr, wait.Round(time.Second))
		}
		c.logf("%s %s: %v; retrying in %s (attempt %d/%d)", method, url, lastErr, wait.Round(time.Second), attempt, c.attempts())
		if err := c.clock().Sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// readError drains a failed response into an APIError.
func readError(req *http.Request, resp *http.Response) error {
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	ae := &APIError{Method: req.Method, URL: req.URL.String(), Status: resp.StatusCode, Body: string(b)}
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &m) == nil {
		ae.Message = m.Message
	}
	return ae
}

// JSON performs an idempotent request and decodes a JSON response into out
// (which may be nil). A 404 becomes ErrNotFound.
func (c *Client) JSON(ctx context.Context, method, url string, in, out any) error {
	var body []byte
	ct := ""
	if in != nil {
		var err error
		body, err = json.Marshal(in)
		if err != nil {
			return err
		}
		ct = "application/json"
	}
	resp, err := c.DoIdempotent(ctx, method, url, body, ct)
	if err != nil {
		return err
	}
	return decode(method, url, resp, out)
}

// PostJSON performs a NON-idempotent POST exactly once.
func (c *Client) PostJSON(ctx context.Context, url string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	return decode(http.MethodPost, url, resp, out)
}

func decode(method, url string, resp *http.Response, out any) error {
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("%s %s: %w", method, url, ErrNotFound)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return readError(&http.Request{Method: method, URL: resp.Request.URL}, resp)
	}
	if out == nil {
		_, err := io.Copy(io.Discard, resp.Body)
		return err
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decoding response: %v", method, url, err)
	}
	return nil
}

// Stream performs an idempotent GET and returns the body for a 2xx response.
func (c *Client) Stream(ctx context.Context, url string, accept string) (io.ReadCloser, error) {
	var lastErr error
	backoff := time.Second
	for attempt := 1; attempt <= c.attempts(); attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		resp, err := c.Do(req)
		ok, wait := c.retryable(resp, err)
		if !ok {
			if err != nil {
				return nil, err
			}
			if resp.StatusCode == http.StatusNotFound {
				resp.Body.Close()
				return nil, fmt.Errorf("GET %s: %w", url, ErrNotFound)
			}
			if resp.StatusCode < 200 || resp.StatusCode > 299 {
				return nil, readError(req, resp)
			}
			return resp.Body, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = readError(req, resp)
		}
		if attempt == c.attempts() {
			break
		}
		if wait == 0 {
			wait = backoff
			backoff *= 2
		}
		if wait > c.maxWait() {
			return nil, lastErr
		}
		if err := c.clock().Sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// nextPage parses a Link header for rel="next".
func nextPage(h http.Header) string {
	for _, part := range strings.Split(h.Get("Link"), ",") {
		part = strings.TrimSpace(part)
		if strings.HasSuffix(part, `rel="next"`) {
			if i, j := strings.IndexByte(part, '<'), strings.IndexByte(part, '>'); i >= 0 && j > i {
				return part[i+1 : j]
			}
		}
	}
	return ""
}
