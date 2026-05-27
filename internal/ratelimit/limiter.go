package ratelimit

import (
	"sync"
	"time"
)

// Endpoint-specific rate limits
const (
	MessagePerSecond  = 5
	MessagePerMinute  = 100
	FilePerMinute     = 2
	StatusPerMinute   = 10
	HistoryPerMinute  = 30
)

type bucket struct {
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second
	lastRefill time.Time
	mu         sync.Mutex
}

func newBucket(maxTokens float64, refillRate float64) *bucket {
	return &bucket{
		tokens:     maxTokens,
		maxTokens:  maxTokens,
		refillRate: refillRate,
		lastRefill: time.Now(),
	}
}

func (b *bucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens = min(b.maxTokens, b.tokens+elapsed*b.refillRate)
	b.lastRefill = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sessionLimiter holds all per-session rate limit buckets.
type sessionLimiter struct {
	message  *bucket // 5/sec, 100/min -> use the more restrictive: 5/sec
	file     *bucket // 2/min
	status   *bucket // 10/min
	history  *bucket // 30/min
	lastSeen time.Time
	mu       sync.Mutex
}

func newSessionLimiter() *sessionLimiter {
	return &sessionLimiter{
		// 5/sec burst, refills at 5/sec
		message: newBucket(5, 5),
		// 2/min burst, refills at 2/60 per second
		file: newBucket(2, float64(2)/60),
		// 10/min
		status: newBucket(10, float64(10)/60),
		// 30/min
		history: newBucket(30, float64(30)/60),
		lastSeen: time.Now(),
	}
}

// Limiter manages per-session rate limiting with auto-cleanup of idle sessions.
type Limiter struct {
	mu       sync.Mutex
	sessions map[string]*sessionLimiter
}

// New creates a new Limiter. Call StartCleanup to periodically evict idle sessions.
func New() *Limiter {
	return &Limiter{
		sessions: make(map[string]*sessionLimiter),
	}
}

func (l *Limiter) get(sessionID string) *sessionLimiter {
	l.mu.Lock()
	defer l.mu.Unlock()

	sl, ok := l.sessions[sessionID]
	if !ok {
		sl = newSessionLimiter()
		l.sessions[sessionID] = sl
	}
	sl.mu.Lock()
	sl.lastSeen = time.Now()
	sl.mu.Unlock()
	return sl
}

// AllowMessage returns true if the session is within the message rate limit.
func (l *Limiter) AllowMessage(sessionID string) bool {
	return l.get(sessionID).message.Allow()
}

// AllowFile returns true if the session is within the file rate limit.
func (l *Limiter) AllowFile(sessionID string) bool {
	return l.get(sessionID).file.Allow()
}

// AllowStatus returns true if the session is within the status rate limit.
func (l *Limiter) AllowStatus(sessionID string) bool {
	return l.get(sessionID).status.Allow()
}

// AllowHistory returns true if the session is within the history rate limit.
func (l *Limiter) AllowHistory(sessionID string) bool {
	return l.get(sessionID).history.Allow()
}

// StartCleanup runs a background goroutine that evicts session limiters idle for >1 hour.
func (l *Limiter) StartCleanup() {
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			l.cleanup()
		}
	}()
}

func (l *Limiter) cleanup() {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := time.Now().Add(-1 * time.Hour)
	for id, sl := range l.sessions {
		sl.mu.Lock()
		idle := sl.lastSeen.Before(cutoff)
		sl.mu.Unlock()
		if idle {
			delete(l.sessions, id)
		}
	}
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
