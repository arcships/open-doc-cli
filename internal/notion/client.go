// Package notion is the Notion platform adapter. Enumeration walks
// the flat POST /v1/search inventory and rebuilds the tree from parent pointers;
// document bodies come from the official GET /v1/pages/{id}/markdown endpoint;
// assets are the S3 signed URLs embedded in the markdown. Everything is plain
// HTTPS over net/http — no third-party SDK.
//
// The adapter is strictly read-only: it issues only GET requests and the read
// POST /v1/search. All network calls funnel through an injected http.RoundTripper
// so tests can substitute canned responses with no network dependency.
package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/arcships/open-doc-cli/internal/ratelimit"
)

// Base is the Notion API host.
const Base = "https://api.notion.com"

// Version is the Notion API version this adapter targets (the markdown endpoint
// and the database/data-source split model, P0-verified).
const Version = "2026-03-11"

// APIRPS is the request cap (3 rps) for the Notion API token bucket.
const APIRPS = 3.0

// AssetRPS is the separate cap for asset downloads from S3 (independent bucket).
const AssetRPS = 3.0

// Client is a minimal read-only Notion API client. It holds the integration
// token (never logged), paces API calls through a token bucket, and retries on
// 429 with the server's Retry-After hint.
type Client struct {
	token    string
	base     string
	http     *http.Client
	apiLimit *ratelimit.Bucket
	assetLim *ratelimit.Bucket
	backoff  ratelimit.Backoff
}

// NewClient builds a Client for token, using rt as the HTTP transport (nil ⇒
// http.DefaultTransport). The token is required; callers gate construction on it
// being non-empty.
func NewClient(token string, rt http.RoundTripper) *Client {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return &Client{
		token:    token,
		base:     Base,
		http:     &http.Client{Transport: rt},
		apiLimit: ratelimit.NewPerSecond(APIRPS),
		assetLim: ratelimit.NewPerSecond(AssetRPS),
		backoff:  ratelimit.DefaultBackoff,
	}
}

// apiError is a non-2xx API response classified for retry decisions.
type apiError struct {
	status     int
	retryAfter time.Duration
	body       string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("notion api status %d: %s", e.status, truncateForError(e.body))
}

// isRetryable reports whether err is a transient Notion API failure worth
// retrying: rate limiting (429) or a server-side 5xx.
func isRetryable(err error) bool {
	ae, ok := err.(*apiError)
	if !ok {
		return false
	}
	return ae.status == http.StatusTooManyRequests || ae.status >= 500
}

// doAPI issues one rate-limited, backoff-wrapped API request and returns the
// response body bytes. method is GET or POST; body is nil for GET.
func (c *Client) doAPI(ctx context.Context, method, path string, reqBody []byte) ([]byte, error) {
	var out []byte
	err := c.backoff.Retry(ctx, isRetryable, func() (time.Duration, error) {
		if err := c.apiLimit.Wait(ctx); err != nil {
			return 0, err
		}
		var bodyReader io.Reader
		if reqBody != nil {
			bodyReader = bytes.NewReader(reqBody)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, bodyReader)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Notion-Version", Version)
		if reqBody != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		data, readErr := io.ReadAll(resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			ae := &apiError{status: resp.StatusCode, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")), body: string(data)}
			return ae.retryAfter, ae
		}
		if readErr != nil {
			return 0, readErr
		}
		out = data
		return 0, nil
	})
	return out, err
}

// Me verifies the integration token via GET /v1/users/me, the cheapest
// authenticated read. It is the credential self-check backing `opendoc doctor`;
// GET-only keeps the adapter read-only. Returns nil on a valid token.
func (c *Client) Me(ctx context.Context) error {
	_, err := c.doAPI(ctx, "GET", "/v1/users/me", nil)
	return err
}

// VisibleCount reports how many pages/databases the integration sees on the
// first POST /v1/search page. It backs `opendoc doctor`'s N3 probe: a valid token
// connected to nothing returns 0 — the N3-EMPTY case, indistinguishable at the
// API level from a genuinely empty workspace and so surfaced explicitly.
// Single page only: a cheap, read-only connectivity check.
func (c *Client) VisibleCount(ctx context.Context) (int, error) {
	reqBody, _ := json.Marshal(map[string]any{"page_size": 100})
	raw, err := c.doAPI(ctx, "POST", "/v1/search", reqBody)
	if err != nil {
		return 0, err
	}
	var page searchResponse
	if err := json.Unmarshal(raw, &page); err != nil {
		return 0, fmt.Errorf("decode search page: %w", err)
	}
	return len(page.Results), nil
}

// downloadTo streams the (unauthenticated) S3 signed URL to destPath. Notion S3
// URLs carry their own credentials in the query string, so no Authorization
// header is sent. Paced by the dedicated asset bucket and retried on 429/5xx.
func (c *Client) downloadTo(ctx context.Context, url, destPath string) error {
	return c.backoff.Retry(ctx, isRetryable, func() (time.Duration, error) {
		if err := c.assetLim.Wait(ctx); err != nil {
			return 0, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return 0, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			ae := &apiError{status: resp.StatusCode, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")), body: string(data)}
			return ae.retryAfter, ae
		}
		f, err := os.Create(destPath)
		if err != nil {
			return 0, err
		}
		if _, err := io.Copy(f, resp.Body); err != nil {
			_ = f.Close()
			return 0, err
		}
		if err := f.Close(); err != nil {
			return 0, err
		}
		return 0, nil
	})
}

// parseRetryAfter parses a Retry-After header value (delta-seconds form) into a
// duration, returning 0 when absent or malformed (fall back to exponential
// backoff).
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// truncateForError caps an error body so a large HTML/JSON error page does not
// flood logs.
func truncateForError(s string) string {
	const max = 200
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
