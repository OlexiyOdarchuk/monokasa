package web

import (
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// rateLimiter is an in-memory per-key token bucket. Keyed by client IP for
// /scan/check, it stops a leaked scanner URL from being used to brute-force
// QR payloads against the HMAC verifier.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens added per second
	burst   float64 // bucket size
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(ratePerSec, burst float64) *rateLimiter {
	return &rateLimiter{
		buckets: make(map[string]*bucket),
		rate:    ratePerSec,
		burst:   burst,
	}
}

func (rl *rateLimiter) allow(key string) bool {
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

// gc drops buckets idle longer than max — keeps the map from growing
// unbounded across the show's lifetime.
func (rl *rateLimiter) gc(max time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	for k, b := range rl.buckets {
		if now.Sub(b.last) > max {
			delete(rl.buckets, k)
		}
	}
}

// clientIP extracts the caller's IP, honouring X-Forwarded-For when set
// (we always sit behind nginx/Caddy/cloudflared in practice).
func clientIP(r *http.Request) string {
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
