package web

import (
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Limiter is an in-memory per-key token bucket. Originally keyed by
// client IP for /scan/check, also reused for /login/request and other
// public-side endpoints that want abuse mitigation without pulling in
// a dependency.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens added per second
	burst   float64 // bucket size
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewLimiter returns a token bucket that refills at ratePerSec tokens
// per second up to burst tokens total. Allow consumes one token.
func NewLimiter(ratePerSec, burst float64) *Limiter {
	return &Limiter{
		buckets: make(map[string]*bucket),
		rate:    ratePerSec,
		burst:   burst,
	}
}

// Allow reports whether the key has a token to spend. Caller decides
// the key (IP, email, hash thereof) — Limiter only cares that it's
// comparable as a string.
func (rl *Limiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: rl.burst, last: now}
		rl.buckets[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens = math.Min(rl.burst, b.tokens+elapsed*rl.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// GC drops buckets idle longer than max — keeps the map from growing
// unbounded across the show's lifetime. Caller should run on a ticker.
func (rl *Limiter) GC(max time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	for k, b := range rl.buckets {
		if now.Sub(b.last) > max {
			delete(rl.buckets, k)
		}
	}
}

// ClientIP extracts the caller's IP, honouring X-Forwarded-For when
// set (we always sit behind nginx/Caddy/cloudflared in practice). When
// XFF is unset, falls back to RemoteAddr. Callers that don't trust
// the proxy chain should strip XFF before calling.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(first)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
