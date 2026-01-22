package app

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

// Ensure we preserve streaming support (SSE) when wrapping ResponseWriter.
// Without this, handlers that require http.Flusher (e.g. /api/chat/stream)
// will see a 500 "internal server error".
func (w *statusRecorder) Flush() {
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// Unwrap returns the underlying ResponseWriter (used by net/http helpers like ResponseController).
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack delegates to the underlying http.Hijacker when supported (e.g., WebSockets).
func (w *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("http hijacker not supported")
	}
	return hj.Hijack()
}

// Push delegates to the underlying http.Pusher when supported (HTTP/2 server push).
func (w *statusRecorder) Push(target string, opts *http.PushOptions) error {
	p, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return p.Push(target, opts)
}

// CloseNotify delegates when supported (deprecated but still used by some libs).
func (w *statusRecorder) CloseNotify() <-chan bool {
	if cn, ok := w.ResponseWriter.(http.CloseNotifier); ok {
		return cn.CloseNotify()
	}
	ch := make(chan bool, 1)
	return ch
}

// ReadFrom preserves io.Copy optimizations and keeps our byte counters accurate.
func (w *statusRecorder) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(r)
		if n > 0 {
			if n > int64(^uint(0)>>1) {
				w.bytes = int(^uint(0) >> 1)
			} else {
				w.bytes += int(n)
			}
		}
		return n, err
	}
	n, err := io.Copy(w.ResponseWriter, r)
	if n > 0 {
		if n > int64(^uint(0)>>1) {
			w.bytes = int(^uint(0) >> 1)
		} else {
			w.bytes += int(n)
		}
	}
	return n, err
}

// WriteString preserves StringWriter implementations and keeps our byte counters accurate.
func (w *statusRecorder) WriteString(s string) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if sw, ok := w.ResponseWriter.(io.StringWriter); ok {
		n, err := sw.WriteString(s)
		w.bytes += n
		return n, err
	}
	n, err := w.ResponseWriter.Write([]byte(s))
	w.bytes += n
	return n, err
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	// fallback
	return hex.EncodeToString([]byte(time.Now().Format("150405.000")))
}

func clientIP(r *http.Request) string {
	// Prefer RemoteAddr for trust; XFF is not trusted by default.
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

type ipRateLimiter struct {
	mu     sync.Mutex
	rpm    int
	burst  float64
	states map[string]*bucket
	ttl    time.Duration
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newIPRateLimiter(rpm int) *ipRateLimiter {
	if rpm <= 0 {
		rpm = 0
	}
	return &ipRateLimiter{
		rpm:    rpm,
		burst:  float64(maxInt(1, rpm/6)), // ~10s burst
		states: make(map[string]*bucket),
		ttl:    10 * time.Minute,
	}
}

func (l *ipRateLimiter) allow(ip string) bool {
	if l == nil || l.rpm <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	b := l.states[ip]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.states[ip] = b
	}

	// refill
	perSec := float64(l.rpm) / 60.0
	dt := now.Sub(b.last).Seconds()
	if dt > 0 {
		b.tokens += dt * perSec
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}

	if b.tokens < 1.0 {
		return false
	}
	b.tokens -= 1.0

	// lazy cleanup
	if len(l.states) > 2048 {
		cutoff := now.Add(-l.ttl)
		for k, v := range l.states {
			if v.last.Before(cutoff) {
				delete(l.states, k)
			}
		}
	}

	return true
}

func applyHTTPMiddleware(cfg Config, h http.Handler) http.Handler {
	limiter := newIPRateLimiter(cfg.HTTPRateLimitRPM)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := newRequestID()
		r.Header.Set("X-Request-Id", reqID)

		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()

		defer func() {
			if v := recover(); v != nil {
				log.Printf("[http] panic req_id=%s method=%s path=%s err=%v", reqID, r.Method, r.URL.Path, v)
				http.Error(rec, "internal server error", http.StatusInternalServerError)
			}
			dur := time.Since(start)
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			log.Printf("[http] req_id=%s ip=%s method=%s path=%s status=%d bytes=%d dur=%s", reqID, clientIP(r), r.Method, r.URL.Path, status, rec.bytes, dur)
		}()

		// Basic security headers (avoid CSP here to not break existing UI).
		rec.Header().Set("X-Content-Type-Options", "nosniff")
		rec.Header().Set("X-Frame-Options", "DENY")
		rec.Header().Set("Referrer-Policy", "no-referrer")

		// Per-IP rate limit for API endpoints.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if !limiter.allow(clientIP(r)) {
				http.Error(rec, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
		}

		// Token auth (only for API routes; UI+static remain accessible so the app can load).
		if cfg.HTTPAuthToken != "" && strings.HasPrefix(r.URL.Path, "/api/") {
			if !checkAuthToken(cfg.HTTPAuthToken, r) {
				rec.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(rec, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		h.ServeHTTP(rec, r)
	})
}

func checkAuthToken(token string, r *http.Request) bool {
	if token == "" {
		return true
	}

	// ✅ Convenience + safety: allow local loopback requests without a token.
	// This keeps the "protect others, not me" workflow frictionless when you
	// access the UI via http://127.0.0.1 / http://localhost on the same machine.
	//
	// Security note:
	// - We only bypass when the TCP peer is loopback (RemoteAddr is 127.0.0.1/::1)
	// - If common proxy forwarding headers are present, we DO NOT bypass
	//   (prevents accidental bypass behind a reverse proxy)
	if isLoopbackRemoteAddr(r.RemoteAddr) && !hasForwardedHeaders(r) {
		return true
	}

	if t := strings.TrimSpace(r.Header.Get("X-Auth-Token")); t != "" {
		return subtleEqual(t, token)
	}
	if a := strings.TrimSpace(r.Header.Get("Authorization")); a != "" {
		if strings.HasPrefix(strings.ToLower(a), "bearer ") {
			v := strings.TrimSpace(a[7:])
			return subtleEqual(v, token)
		}
	}
	return false
}

func hasForwardedHeaders(r *http.Request) bool {
	if r == nil {
		return false
	}
	// Any of these implies we're likely behind a proxy.
	// We treat them as "untrusted" for local bypass.
	if strings.TrimSpace(r.Header.Get("Forwarded")) != "" {
		return true
	}
	if strings.TrimSpace(r.Header.Get("X-Forwarded-For")) != "" {
		return true
	}
	if strings.TrimSpace(r.Header.Get("X-Real-IP")) != "" {
		return true
	}
	if strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")) != "" {
		return true
	}
	return false
}

func isLoopbackRemoteAddr(remoteAddr string) bool {
	host := strings.TrimSpace(remoteAddr)
	if host == "" {
		return false
	}
	// remoteAddr is usually "ip:port".
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	if host == "127.0.0.1" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

func subtleEqual(a, b string) bool {
	// constant-time-ish compare without importing crypto/subtle
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
