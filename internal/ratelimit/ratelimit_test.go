package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBucketPacesAcquisitions(t *testing.T) {
	b := NewPerSecond(1000) // 1ms interval
	ctx := context.Background()
	start := time.Now()
	const n = 5
	for i := 0; i < n; i++ {
		if err := b.Wait(ctx); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}
	// Five acquisitions at 1ms spacing should take at least ~4ms (the first is
	// free). Assert a conservative lower bound to avoid flakiness.
	if elapsed := time.Since(start); elapsed < 3*time.Millisecond {
		t.Fatalf("acquisitions not paced: %v for %d tokens", elapsed, n)
	}
}

func TestBucketRespectsContextCancel(t *testing.T) {
	b := NewPerSecond(1) // 1s interval; the second Wait must block ~1s
	ctx, cancel := context.WithCancel(context.Background())
	_ = b.Wait(ctx) // consume the free slot
	cancel()
	if err := b.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait after cancel = %v, want context.Canceled", err)
	}
}

func TestBackoffRetriesThenSucceeds(t *testing.T) {
	bo := Backoff{Base: time.Millisecond, Max: time.Millisecond, Attempts: 4}
	calls := 0
	err := bo.Retry(context.Background(), nil, func() (time.Duration, error) {
		calls++
		if calls < 3 {
			return 0, errors.New("429 too many requests")
		}
		return 0, nil
	})
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestBackoffStopsOnNonRetryable(t *testing.T) {
	bo := Backoff{Base: time.Millisecond, Max: time.Millisecond, Attempts: 5}
	calls := 0
	sentinel := errors.New("fatal")
	err := bo.Retry(context.Background(), func(error) bool { return false }, func() (time.Duration, error) {
		calls++
		return 0, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (no retry on non-retryable)", calls)
	}
}

func TestBackoffExhausts(t *testing.T) {
	bo := Backoff{Base: time.Millisecond, Max: time.Millisecond, Attempts: 3}
	calls := 0
	err := bo.Retry(context.Background(), nil, func() (time.Duration, error) {
		calls++
		return 0, errors.New("rate limit")
	})
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}
