package notion

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTransport is an http.RoundTripper that serves canned responses via a
// user-supplied handler, recording every request path for assertions. It lets
// the adapter be exercised with zero network.
type fakeTransport struct {
	mu      sync.Mutex
	handler func(req *http.Request) (*http.Response, error)
	calls   []string
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req.Method+" "+req.URL.Path)
	f.mu.Unlock()
	// Drain the body so a POST's request body is not left dangling.
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	return f.handler(req)
}

// jsonResp builds an HTTP response with the given status and JSON body.
func jsonResp(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     header,
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := map[string]time.Duration{
		"":     0,
		"5":    5 * time.Second,
		"0":    0,
		"abc":  0,
		"  3 ": 3 * time.Second,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDoAPIRetriesOn429ThenSucceeds(t *testing.T) {
	var n int
	ft := &fakeTransport{}
	ft.handler = func(req *http.Request) (*http.Response, error) {
		n++
		if n == 1 {
			h := http.Header{}
			h.Set("Retry-After", "0")
			return jsonResp(http.StatusTooManyRequests, `{"message":"rate limited"}`, h), nil
		}
		return jsonResp(http.StatusOK, `{"ok":true}`, nil), nil
	}
	c := NewClient("tok", ft)
	out, err := c.doAPI(context.Background(), "GET", "/v1/x", nil)
	if err != nil {
		t.Fatalf("doAPI: %v", err)
	}
	if string(out) != `{"ok":true}` {
		t.Fatalf("body = %q", out)
	}
	if n != 2 {
		t.Fatalf("expected 1 retry (2 calls), got %d", n)
	}
}

func TestDoAPINonRetryableFailsFast(t *testing.T) {
	var n int
	ft := &fakeTransport{handler: func(req *http.Request) (*http.Response, error) {
		n++
		return jsonResp(http.StatusUnauthorized, `{"message":"unauthorized"}`, nil), nil
	}}
	c := NewClient("tok", ft)
	if _, err := c.doAPI(context.Background(), "GET", "/v1/x", nil); err == nil {
		t.Fatal("expected error on 401")
	}
	if n != 1 {
		t.Fatalf("401 must not retry; got %d calls", n)
	}
}

// TestRateLimitersAreIndependent proves the API and asset buckets are separate:
// interleaving 3 API calls and 3 asset downloads costs ~2 intervals per bucket
// (they run in parallel), not the ~5 intervals a single shared bucket would
// impose. The fake transport answers instantly, so elapsed time is purely the
// limiters.
func TestRateLimitersAreIndependent(t *testing.T) {
	ft := &fakeTransport{handler: func(req *http.Request) (*http.Response, error) {
		return jsonResp(http.StatusOK, `{"markdown":""}`, nil), nil
	}}
	c := NewClient("tok", ft)
	ctx := context.Background()

	// Burst 1 means the first acquisition on each bucket is free; three calls on
	// one bucket therefore cost ~2 intervals.
	interval := time.Second / time.Duration(APIRPS)

	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := c.doAPI(ctx, "GET", "/v1/pages/x/markdown", nil); err != nil {
			t.Fatalf("api call: %v", err)
		}
		if err := c.downloadTo(ctx, "https://example.com/a", t.TempDir()+"/f"); err != nil {
			t.Fatalf("asset call: %v", err)
		}
	}
	elapsed := time.Since(start)

	// Independent buckets: each sees 3 calls → ~2 intervals; the two proceed
	// concurrently, so total ≈ 2 intervals. A shared bucket would see 6 calls →
	// ~5 intervals. The midpoint threshold cleanly separates the two.
	if elapsed > time.Duration(3.5*float64(interval)) {
		t.Fatalf("interleaved calls took %v (> 3.5 intervals); buckets appear shared", elapsed)
	}
}
