package httpapi

import (
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type bucket struct {
	limiter *rate.Limiter
	last    time.Time
}
type limiter struct {
	mu          sync.Mutex
	entries     map[string]*bucket
	rate        rate.Limit
	burst       int
	lastCleanup time.Time
}

func newLimiter(rpm, burst int) *limiter {
	return &limiter{entries: map[string]*bucket{}, rate: rate.Limit(float64(rpm) / 60), burst: burst}
}
func (l *limiter) allow(key string, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastCleanup) >= time.Minute {
		for k, b := range l.entries {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.entries, k)
			}
		}
		l.lastCleanup = now
	}
	b, ok := l.entries[key]
	if !ok {
		// Bound memory even when many distinct clients arrive within the idle TTL.
		if len(l.entries) >= 10000 {
			return false, 60
		}
		b = &bucket{limiter: rate.NewLimiter(l.rate, l.burst)}
		l.entries[key] = b
	}
	b.last = now
	if b.limiter.AllowN(now, 1) {
		return true, 0
	}
	wait := int(math.Ceil((1 - b.limiter.TokensAt(now)) / float64(l.rate)))
	if wait < 1 {
		wait = 1
	}
	return false, wait
}
