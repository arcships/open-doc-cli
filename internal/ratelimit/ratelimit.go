// Package ratelimit provides a small, dependency-free token bucket and an
// exponential backoff helper for honouring platform QPS limits (Feishu fetch
// 5 QPS, assets 5 QPS; exponential backoff on 429/Retry-After).
//
// The bucket is safe for concurrent use, ready for goroutine-based fetch even
// though the engine currently drives it sequentially.
package ratelimit

import (
	"context"
	"math"
	"sync"
	"time"
)

// Bucket paces acquisitions to at most one per interval (a steady-rate token
// bucket with burst 1). Wait blocks until the next slot is available.
type Bucket struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
	now      func() time.Time
}

// NewPerSecond builds a Bucket that admits qps acquisitions per second. qps must
// be positive.
func NewPerSecond(qps float64) *Bucket {
	return &Bucket{
		interval: time.Duration(float64(time.Second) / qps),
		now:      time.Now,
	}
}

// Wait blocks until the next slot is due or ctx is cancelled. It reserves the
// slot before sleeping so concurrent callers are serialised onto distinct slots.
func (b *Bucket) Wait(ctx context.Context) error {
	b.mu.Lock()
	now := b.now()
	if b.next.Before(now) {
		b.next = now
	}
	wait := b.next.Sub(now)
	b.next = b.next.Add(b.interval)
	b.mu.Unlock()

	if wait <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Backoff describes exponential backoff with a capped number of attempts.
type Backoff struct {
	// Base is the first delay; each retry doubles it up to Max.
	Base time.Duration
	// Max caps a single delay.
	Max time.Duration
	// Attempts is the total number of tries (including the first).
	Attempts int
}

// DefaultBackoff is a reasonable policy for lark-cli fetch retries.
var DefaultBackoff = Backoff{Base: 500 * time.Millisecond, Max: 30 * time.Second, Attempts: 5}

// Retry calls fn until it succeeds, fn signals the error is not retryable, or
// attempts are exhausted. retryable decides, per error, whether another attempt
// should be made; if nil, every error is retried. The delay before retry N uses
// exponential growth unless fn returns a non-zero retryAfter (honouring an HTTP
// Retry-After hint), which takes precedence.
func (bo Backoff) Retry(ctx context.Context, retryable func(error) bool, fn func() (retryAfter time.Duration, err error)) error {
	attempts := bo.Attempts
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		retryAfter, err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		if retryable != nil && !retryable(err) {
			return err
		}
		if attempt == attempts-1 {
			break
		}
		delay := bo.delay(attempt)
		if retryAfter > 0 {
			delay = retryAfter
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	return lastErr
}

// delay returns the exponential delay for a zero-based attempt index, capped at
// Max.
func (bo Backoff) delay(attempt int) time.Duration {
	d := float64(bo.Base) * math.Pow(2, float64(attempt))
	if d > float64(bo.Max) {
		return bo.Max
	}
	return time.Duration(d)
}
