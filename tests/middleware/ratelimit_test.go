package middleware_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swiftly-headquarters/minato/middleware"
	"github.com/swiftly-headquarters/minato/server"
)

func okHandler(ctx context.Context, _ *empty) (*empty, error) { return nil, nil }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func limited(cfg middleware.RateLimitConfig) http.Handler {
	api := server.New("t", "1")
	api.Use(middleware.RateLimit(cfg))
	server.Get(api, "/x", okHandler)
	return api
}

func hit(h http.Handler, remote string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = remote
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const peer = "192.0.2.7:4000"

func TestRateLimitRefill(t *testing.T) {
	clk := newClock()
	h := limited(middleware.RateLimitConfig{Rate: 1, Burst: 2, Now: clk.Now})

	for i := 1; i <= 2; i++ {
		if rec := hit(h, peer); rec.Code != 204 {
			t.Fatalf("request %d within the burst: %d", i, rec.Code)
		}
	}
	rec := hit(h, peer)
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "1" || rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("over the burst: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	clk.Advance(500 * time.Millisecond)
	if rec := hit(h, peer); rec.Code != 429 || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("half a token is not enough: %d %v", rec.Code, rec.Header())
	}
	clk.Advance(500 * time.Millisecond)
	if rec := hit(h, peer); rec.Code != 204 {
		t.Fatalf("a full second refills one token: %d", rec.Code)
	}
	if rec := hit(h, peer); rec.Code != 429 {
		t.Fatalf("and only one: %d", rec.Code)
	}
	clk.Advance(time.Hour)
	allowed := 0
	for range 5 {
		if hit(h, peer).Code == 204 {
			allowed++
		}
	}
	if allowed != 2 {
		t.Errorf("an idle client gets the burst back, not more: %d", allowed)
	}
	clk.Advance(-time.Hour)
	if hit(h, peer).Code != 429 {
		t.Error("a clock that moves backwards must not hand out tokens")
	}
	clk.Advance(3 * time.Second)
	if hit(h, peer).Code != 204 {
		t.Error("and must not leave the client in debt once time moves on")
	}
}

func TestRateLimitRetryAfterScalesWithRate(t *testing.T) {
	h := limited(middleware.RateLimitConfig{Rate: 0.1, Burst: 1, Now: newClock().Now})
	hit(h, peer)
	if rec := hit(h, peer); rec.Code != 429 || rec.Header().Get("Retry-After") != "10" {
		t.Errorf("one token per ten seconds: %d %v", rec.Code, rec.Header())
	}
}

func TestRateLimitDefaultBurst(t *testing.T) {
	for rate, want := range map[float64]int{5: 5, 0.2: 1, 2.5: 3} {
		h := limited(middleware.RateLimitConfig{Rate: rate, Now: newClock().Now})
		allowed := 0
		for range 20 {
			if hit(h, peer).Code == 204 {
				allowed++
			}
		}
		if allowed != want {
			t.Errorf("rate %v: default burst allowed %d, want %d", rate, allowed, want)
		}
	}
}

func TestRateLimitKeys(t *testing.T) {
	h := limited(middleware.RateLimitConfig{Rate: 0.001, Burst: 1, Now: newClock().Now})
	for _, c := range []struct {
		remote string
		code   int
		why    string
	}{
		{"1.1.1.1:1000", 204, "first client"},
		{"1.1.1.1:2000", 429, "another port is the same client"},
		{"2.2.2.2:1000", 204, "another address has its own bucket"},
		{"[2001:db8:1:2::1]:80", 204, "first ipv6 client"},
		{"[2001:db8:1:2:ffff::9]:80", 429, "the same /64 is one client"},
		{"[2001:db8:1:3::1]:80", 204, "another /64"},
		{"[::ffff:3.3.3.3]:1", 204, "mapped ipv4"},
		{"3.3.3.3:2", 429, "is the same client as plain ipv4"},
		{"bogus", 204, "an unparsable address still gets a bucket"},
		{"bogus", 429, "and keeps it"},
	} {
		if rec := hit(h, c.remote); rec.Code != c.code {
			t.Errorf("%s (%s): got %d, want %d", c.remote, c.why, rec.Code, c.code)
		}
	}
}

func TestRateLimitCustomKey(t *testing.T) {
	h := limited(middleware.RateLimitConfig{
		Rate: 0.001, Burst: 1, Now: newClock().Now,
		Key: func(r *http.Request) string { return r.Header.Get("X-API-Key") },
	})
	for range 5 {
		if rec := hit(h, peer); rec.Code != 204 {
			t.Fatalf("an empty key is not limited: %d", rec.Code)
		}
	}
	if hit(h, peer, "X-API-Key", "a").Code != 204 || hit(h, peer, "X-API-Key", "a").Code != 429 || hit(h, peer, "X-API-Key", "b").Code != 204 {
		t.Error("buckets follow the key, not the address")
	}
}

func TestRateLimitConcurrent(t *testing.T) {
	h := limited(middleware.RateLimitConfig{Rate: 0.001, Burst: 10, Now: newClock().Now})
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if hit(h, peer).Code == 204 {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 10 {
		t.Errorf("exactly the burst must get through, got %d", allowed.Load())
	}
}

func TestRateLimitBoundedKeys(t *testing.T) {
	clk := newClock()
	h := limited(middleware.RateLimitConfig{Rate: 1, Burst: 1, MaxKeys: 3, Now: clk.Now})
	for i := range 200 {
		if rec := hit(h, fmt.Sprintf("10.0.%d.%d:1", i/250, i%250)); rec.Code != 204 {
			t.Fatalf("a new client always gets its first request, even at the key limit: %d (client %d)", rec.Code, i)
		}
		if i%50 == 49 {
			clk.Advance(time.Minute)
		}
	}
}

func TestRateLimitConfigErrors(t *testing.T) {
	for name, cfg := range map[string]middleware.RateLimitConfig{
		"zero rate": {}, "negative rate": {Rate: -1}, "nan rate": {Rate: nan()}, "negative burst": {Rate: 1, Burst: -1},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s must panic at setup", name)
				}
			}()
			middleware.RateLimit(cfg)
		}()
	}
}

func nan() float64 {
	zero := 0.0
	return zero / zero
}
