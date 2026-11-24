package middleware

import (
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/swiftly-headquarters/minato/server"
)

type RateLimitConfig struct {
	Rate    float64
	Burst   int
	Key     func(r *http.Request) string
	MaxKeys int
	Now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

type limiter struct {
	mu        sync.Mutex
	buckets   map[string]*bucket
	rate      float64
	burst     float64
	maxKeys   int
	now       func() time.Time
	lastSweep time.Time
}

func RateLimit(c RateLimitConfig) func(http.Handler) http.Handler {
	if !(c.Rate > 0) {
		panic("minato: RateLimit Rate must be above 0")
	}
	if c.Burst < 0 {
		panic("minato: RateLimit Burst must not be negative")
	}
	if c.Burst == 0 {
		c.Burst = max(1, int(math.Ceil(c.Rate)))
	}
	if c.Key == nil {
		c.Key = clientIP
	}
	if c.MaxKeys <= 0 {
		c.MaxKeys = 10000
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	l := &limiter{buckets: map[string]*bucket{}, rate: c.Rate, burst: float64(c.Burst), maxKeys: c.MaxKeys, now: c.Now}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if key := c.Key(r); key != "" {
				if wait, ok := l.take(key); !ok {
					w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
					server.WriteError(w, server.NewError(http.StatusTooManyRequests, "rate limit exceeded"))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (l *limiter) take(key string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= l.maxKeys {
			l.makeRoom(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else {
		b.tokens = l.refilled(b, now)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return 0, true
	}
	return time.Duration((1 - b.tokens) / l.rate * float64(time.Second)), false
}

func (l *limiter) refilled(b *bucket, now time.Time) float64 {
	return min(l.burst, b.tokens+max(0, now.Sub(b.last).Seconds())*l.rate)
}

func (l *limiter) makeRoom(now time.Time) {
	if now.Sub(l.lastSweep) >= time.Second {
		l.lastSweep = now
		for k, b := range l.buckets {
			if l.refilled(b, now) >= l.burst {
				delete(l.buckets, k)
			}
		}
	}
	for k := range l.buckets {
		if len(l.buckets) < l.maxKeys {
			return
		}
		delete(l.buckets, k)
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	ip = ip.Unmap().WithZone("")
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.String()
	}
	return ip.String()
}
