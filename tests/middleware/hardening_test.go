package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swiftly-headquarters/minato/middleware"
	"github.com/swiftly-headquarters/minato/middleware/idempotencytest"
	"github.com/swiftly-headquarters/minato/server"
)

func TestSecureHeadersDefaults(t *testing.T) {
	api := server.New("t", "1")
	api.Use(middleware.SecureHeaders(middleware.SecureHeadersConfig{}))
	server.Get(api, "/x", okHandler)
	h := do(api, "GET", "/x").Header()
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("defaults: %v", h)
	}
	if h.Get("Content-Security-Policy") != "" || h.Get("Strict-Transport-Security") != "" {
		t.Fatalf("CSP and HSTS are opt-in: %v", h)
	}
}

func TestSecureHeadersConfigAndOverride(t *testing.T) {
	api := server.New("t", "1")
	api.Use(middleware.SecureHeaders(middleware.SecureHeadersConfig{
		FrameOptions:          "SAMEORIGIN",
		ContentSecurityPolicy: "default-src 'none'",
		HSTSMaxAge:            365 * 24 * time.Hour,
		HSTSIncludeSubdomains: true,
	}))
	server.Handle(api, "GET /framed", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "ALLOW-FROM https://a.example")
		w.Write([]byte("ok"))
	}))
	h := do(api, "GET", "/framed").Header()
	if h.Get("X-Frame-Options") != "ALLOW-FROM https://a.example" {
		t.Fatalf("a handler may override a header: %v", h)
	}
	if h.Get("Content-Security-Policy") != "default-src 'none'" {
		t.Fatalf("csp: %v", h)
	}
	if h.Get("Strict-Transport-Security") != "max-age=31536000; includeSubDomains" {
		t.Fatalf("hsts: %q", h.Get("Strict-Transport-Security"))
	}
}

type countingHandler struct {
	calls atomic.Int32
	fail  func(n int32) (status int, body string)
}

func (c *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := c.calls.Add(1)
	status, body := http.StatusCreated, "made"
	if c.fail != nil {
		status, body = c.fail(n)
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Location", "/things/1")
	w.WriteHeader(status)
	w.Write([]byte(body))
}

func idemAPI(h http.Handler, cfg middleware.IdempotencyConfig) *server.API {
	api := server.New("t", "1")
	api.Use(middleware.Idempotency(cfg))
	server.Handle(api, "POST /things", h)
	server.Handle(api, "PATCH /things", h)
	server.Handle(api, "GET /things", h)
	return api
}

func post(api http.Handler, key, body string, method ...string) *httptest.ResponseRecorder {
	m := "POST"
	if len(method) > 0 {
		m = method[0]
	}
	hdr := []string{}
	if key != "" {
		hdr = []string{"Idempotency-Key", key}
	}
	return do2(api, m, "/things", body, hdr...)
}

func TestIdempotencyReplaysTheSameResponse(t *testing.T) {
	h := &countingHandler{}
	api := idemAPI(h, middleware.IdempotencyConfig{})
	first := post(api, "k-1", `{"a":1}`)
	second := post(api, "k-1", `{"a":1}`)
	if h.calls.Load() != 1 {
		t.Fatalf("the handler ran %d times", h.calls.Load())
	}
	if second.Code != 201 || second.Body.String() != first.Body.String() || second.Header().Get("Location") != "/things/1" {
		t.Fatalf("replay: %d %q %v", second.Code, second.Body, second.Header())
	}
	if second.Header().Get("Idempotent-Replayed") != "true" || first.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("replays are marked: first=%v second=%v", first.Header(), second.Header())
	}
}

func TestIdempotencyRejectsAReusedKeyForAnotherRequest(t *testing.T) {
	h := &countingHandler{}
	api := idemAPI(h, middleware.IdempotencyConfig{})
	post(api, "k-2", `{"a":1}`)
	rec := post(api, "k-2", `{"a":2}`)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "different request") || h.calls.Load() != 1 {
		t.Fatalf("different body: %d %s calls=%d", rec.Code, rec.Body, h.calls.Load())
	}
	rec = do2(api, "POST", "/other", `{"a":1}`, "Idempotency-Key", "k-2")
	if rec.Code != 422 {
		t.Fatalf("a different path with the same key: %d", rec.Code)
	}
}

func TestIdempotencyWithoutAKeyOrOnSafeMethods(t *testing.T) {
	h := &countingHandler{}
	api := idemAPI(h, middleware.IdempotencyConfig{})
	post(api, "", "x")
	post(api, "", "x")
	if h.calls.Load() != 2 {
		t.Fatalf("no key, no caching: %d", h.calls.Load())
	}
	do2(api, "GET", "/things", "", "Idempotency-Key", "g")
	do2(api, "GET", "/things", "", "Idempotency-Key", "g")
	if h.calls.Load() != 4 {
		t.Fatalf("GET is not covered by default: %d", h.calls.Load())
	}
	do2(api, "PATCH", "/things", "x", "Idempotency-Key", "p")
	do2(api, "PATCH", "/things", "x", "Idempotency-Key", "p")
	if h.calls.Load() != 5 {
		t.Fatalf("PATCH is covered: %d", h.calls.Load())
	}
}

func TestIdempotencyDoesNotCacheServerErrors(t *testing.T) {
	h := &countingHandler{fail: func(n int32) (int, string) {
		if n == 1 {
			return http.StatusInternalServerError, "boom"
		}
		return http.StatusCreated, "made"
	}}
	api := idemAPI(h, middleware.IdempotencyConfig{})
	if rec := post(api, "k-3", "x"); rec.Code != 500 {
		t.Fatalf("first: %d", rec.Code)
	}
	rec := post(api, "k-3", "x")
	if rec.Code != 201 || h.calls.Load() != 2 {
		t.Fatalf("a 500 releases the key so the client can retry: %d calls=%d", rec.Code, h.calls.Load())
	}
	if post(api, "k-3", "x").Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("the successful retry is cached")
	}
}

func TestIdempotencyCachesClientErrors(t *testing.T) {
	h := &countingHandler{fail: func(n int32) (int, string) { return http.StatusConflict, "nope" }}
	api := idemAPI(h, middleware.IdempotencyConfig{})
	post(api, "k-4", "x")
	rec := post(api, "k-4", "x")
	if rec.Code != 409 || h.calls.Load() != 1 {
		t.Fatalf("a 4xx is the answer and is replayed: %d calls=%d", rec.Code, h.calls.Load())
	}
}

func TestIdempotencyConcurrentDuplicateGets409(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(201)
	})
	api := idemAPI(h, middleware.IdempotencyConfig{})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- post(api, "k-5", "x") }()
	<-entered
	rec := post(api, "k-5", "x")
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "still in progress") {
		t.Fatalf("a duplicate while the first runs: %d %s", rec.Code, rec.Body)
	}
	close(release)
	if first := <-done; first.Code != 201 {
		t.Fatalf("the first request completes: %d", first.Code)
	}
	if post(api, "k-5", "x").Code != 201 {
		t.Fatal("once finished, the key replays")
	}
}

func TestIdempotencyPanicReleasesTheKey(t *testing.T) {
	var n atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			panic("first attempt crashes")
		}
		w.WriteHeader(201)
	})
	api := idemAPI(h, middleware.IdempotencyConfig{})
	if rec := post(api, "k-6", "x"); rec.Code != 500 {
		t.Fatalf("the panic is a 500: %d", rec.Code)
	}
	if rec := post(api, "k-6", "x"); rec.Code != 201 {
		t.Fatalf("the key is free after a panic: %d", rec.Code)
	}
}

func TestIdempotencyExpiresAndLimitsKeysAndBodies(t *testing.T) {
	h := &countingHandler{}
	api := idemAPI(h, middleware.IdempotencyConfig{TTL: 50 * time.Millisecond, MaxBody: 10})
	post(api, "k-7", "x")
	time.Sleep(100 * time.Millisecond)
	post(api, "k-7", "x")
	if h.calls.Load() != 2 {
		t.Fatalf("an expired key runs again: %d", h.calls.Load())
	}
	if rec := post(api, strings.Repeat("k", 256), "x"); rec.Code != 400 {
		t.Fatalf("a key over 255 characters: %d", rec.Code)
	}
	if rec := post(api, "k-8", strings.Repeat("x", 11)); rec.Code != 413 {
		t.Fatalf("a body over MaxBody: %d", rec.Code)
	}
}

func TestIdempotencyCustomStore(t *testing.T) {
	store := middleware.NewMemoryIdempotency()
	h := &countingHandler{}
	api := idemAPI(h, middleware.IdempotencyConfig{Store: store})
	post(api, "k-9", "x")
	post(api, "k-9", "x")
	if h.calls.Load() != 1 {
		t.Fatalf("the configured store is used: %d", h.calls.Load())
	}
}

func do2(h http.Handler, method, url, body string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestIdempotencyReleasesWhenThePanicPassesThrough(t *testing.T) {
	var n atomic.Int32
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			panic("crash without a recoverer above")
		}
		w.WriteHeader(201)
	})
	h := middleware.Idempotency(middleware.IdempotencyConfig{})(inner)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic must reach the caller")
			}
		}()
		do2(h, "POST", "/things", "x", "Idempotency-Key", "k-p")
	}()
	if rec := do2(h, "POST", "/things", "x", "Idempotency-Key", "k-p"); rec.Code != 201 {
		t.Fatalf("the key is released when the panic passes through: %d", rec.Code)
	}
}

func TestIdempotencyDoesNotReplayCredentialFailures(t *testing.T) {
	h := &countingHandler{fail: func(n int32) (int, string) {
		if n == 1 {
			return http.StatusUnauthorized, "no"
		}
		return http.StatusCreated, "made"
	}}
	api := idemAPI(h, middleware.IdempotencyConfig{})
	if rec := do2(api, "POST", "/things", "x", "Idempotency-Key", "k-auth"); rec.Code != 401 {
		t.Fatalf("setup: %d", rec.Code)
	}
	if rec := do2(api, "POST", "/things", "x", "Idempotency-Key", "k-auth", "Authorization", "Bearer good"); rec.Code != 201 {
		t.Fatalf("a 401 is not cached, the retry with credentials runs: %d", rec.Code)
	}
}

func TestMemoryIdempotencyStoreContract(t *testing.T) {
	idempotencytest.Run(t, func(t *testing.T) middleware.IdempotencyStore { return middleware.NewMemoryIdempotency() })
}
