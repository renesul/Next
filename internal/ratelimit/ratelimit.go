package ratelimit

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimiter provides per-IP HTTP rate limiting and per-chatID message rate limiting.
type RateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*entry

	// HTTP API limits
	apiRate  rate.Limit
	apiBurst int

	// Login limits (stricter)
	loginRate  rate.Limit
	loginBurst int

	// WhatsApp message limits
	chatRate  rate.Limit
	chatBurst int

	stopCh chan struct{}
}

type entry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// New creates a RateLimiter and starts the cleanup goroutine.
func New(apiRate float64, apiBurst int, chatRate float64, chatBurst int, loginPerMin int) *RateLimiter {
	rl := &RateLimiter{
		limiters:   make(map[string]*entry),
		apiRate:    rate.Limit(apiRate),
		apiBurst:   apiBurst,
		chatRate:   rate.Limit(chatRate),
		chatBurst:  chatBurst,
		loginRate:  rate.Limit(float64(loginPerMin) / 60.0),
		loginBurst: loginPerMin,
		stopCh:     make(chan struct{}),
	}
	go rl.cleanup()
	return rl
}

// Stop halts the cleanup goroutine.
func (rl *RateLimiter) Stop() {
	close(rl.stopCh)
}

func (rl *RateLimiter) getLimiter(key string, r rate.Limit, burst int) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	e, ok := rl.limiters[key]
	if !ok {
		lim := rate.NewLimiter(r, burst)
		rl.limiters[key] = &entry{limiter: lim, lastSeen: time.Now()}
		return lim
	}
	e.lastSeen = time.Now()
	return e.limiter
}

// AllowMessage checks if a WhatsApp message from chatID is within rate limits.
func (rl *RateLimiter) AllowMessage(chatID string) bool {
	lim := rl.getLimiter("chat:"+chatID, rl.chatRate, rl.chatBurst)
	return lim.Allow()
}

// HTTPMiddleware wraps an http.Handler with per-IP rate limiting.
// Exempt: /health
func (rl *RateLimiter) HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		ip := r.RemoteAddr
		if host, _, err := net.SplitHostPort(ip); err == nil {
			ip = host
		}
		if os.Getenv("TRUSTED_PROXY") != "" {
			if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
				ip = strings.TrimSpace(strings.Split(fwd, ",")[0])
			}
		}

		// Use stricter limits for login
		var lim *rate.Limiter
		if r.URL.Path == "/api/login" {
			lim = rl.getLimiter("login:"+ip, rl.loginRate, rl.loginBurst)
		} else {
			lim = rl.getLimiter("api:"+ip, rl.apiRate, rl.apiBurst)
		}

		if !lim.Allow() {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", strconv.Itoa(1))
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limit exceeded","retry_after":1}`))
			return
		}

		next.ServeHTTP(w, r)
	})
}

// cleanup evicts idle limiters every 5 minutes.
func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			rl.mu.Lock()
			cutoff := time.Now().Add(-10 * time.Minute)
			for key, e := range rl.limiters {
				if e.lastSeen.Before(cutoff) {
					delete(rl.limiters, key)
				}
			}
			rl.mu.Unlock()
		case <-rl.stopCh:
			return
		}
	}
}
