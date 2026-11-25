package middleware_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swiftly-headquarters/minato/middleware"
	"github.com/swiftly-headquarters/minato/server"
)

type traceKey struct{}

type recordingTracer struct {
	mu      sync.Mutex
	starts  int
	results []middleware.SpanResult
	nilSpan bool
	nilCtx  bool
}

type recordingSpan struct{ t *recordingTracer }

func (s recordingSpan) End(r middleware.SpanResult) {
	s.t.mu.Lock()
	s.t.results = append(s.t.results, r)
	s.t.mu.Unlock()
}

func (t *recordingTracer) Start(r *http.Request) (context.Context, middleware.Span) {
	t.mu.Lock()
	t.starts++
	t.mu.Unlock()
	ctx := context.WithValue(r.Context(), traceKey{}, "span-1")
	if t.nilCtx {
		ctx = nil
	}
	if t.nilSpan {
		return ctx, nil
	}
	return ctx, recordingSpan{t}
}

func (t *recordingTracer) last() middleware.SpanResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.results[len(t.results)-1]
}

func TestTraceStartsAndEndsSpans(t *testing.T) {
	tr := &recordingTracer{}
	api := server.New("t", "1")
	api.Use(middleware.Trace(tr))
	server.Post(api, "/things", func(ctx context.Context, _ *empty) (*struct{ Body string }, error) {
		return &struct{ Body string }{ctx.Value(traceKey{}).(string)}, nil
	}, func(o *server.Operation) { o.DefaultStatus = 201 })
	rec := do(api, "POST", "/things")
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), "span-1") {
		t.Fatalf("the handler sees the span context: %d %s", rec.Code, rec.Body)
	}
	if got := tr.last(); got != (middleware.SpanResult{Route: "POST /things", Status: 201}) || tr.starts != 1 {
		t.Fatalf("span end: %+v starts=%d", got, tr.starts)
	}
	do(api, "GET", "/missing")
	if got := tr.last(); got != (middleware.SpanResult{Route: "unrouted", Status: 404}) {
		t.Fatalf("unmatched requests: %+v", got)
	}
}

func TestTraceRecordsPanicsAs500(t *testing.T) {
	tr := &recordingTracer{}
	api := server.New("t", "1")
	api.Use(middleware.Trace(tr))
	server.Get(api, "/boom", func(ctx context.Context, _ *empty) (*empty, error) { panic("x") })
	rec := do(api, "GET", "/boom")
	if got := tr.last(); rec.Code != 500 || got.Status != 500 || got.Route != "GET /boom" {
		t.Fatalf("a panic ends the span as a 500: %d %+v", rec.Code, got)
	}
}

type spanFunc func(middleware.SpanResult)

func (f spanFunc) End(r middleware.SpanResult) { f(r) }

func TestTracerFuncAdaptsAFunction(t *testing.T) {
	var got middleware.SpanResult
	api := server.New("t", "1")
	api.Use(middleware.Trace(middleware.TracerFunc(func(r *http.Request) (context.Context, middleware.Span) {
		return r.Context(), spanFunc(func(res middleware.SpanResult) { got = res })
	})))
	server.Get(api, "/x", okHandler)
	do(api, "GET", "/x")
	if got.Route != "GET /x" || got.Status != 204 {
		t.Fatalf("%+v", got)
	}
}

func TestTraceNeedsATracer(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Trace(nil) must panic at setup")
		}
	}()
	middleware.Trace(nil)
}

type failingStore struct {
	middleware.IdempotencyStore
	beginErr, finishErr, releaseErr error
	finishes, releases              atomic.Int32
}

func (f *failingStore) Begin(ctx context.Context, key, fp string, ttl time.Duration) (*middleware.IdempotencyEntry, string, error) {
	if f.beginErr != nil {
		return nil, "", f.beginErr
	}
	return f.IdempotencyStore.Begin(ctx, key, fp, ttl)
}

func (f *failingStore) Finish(ctx context.Context, key, claim string, e middleware.IdempotencyEntry, ttl time.Duration) error {
	f.finishes.Add(1)
	if f.finishErr != nil {
		return f.finishErr
	}
	return f.IdempotencyStore.Finish(ctx, key, claim, e, ttl)
}

func (f *failingStore) Release(ctx context.Context, key, claim string) error {
	f.releases.Add(1)
	if f.releaseErr != nil {
		return f.releaseErr
	}
	return f.IdempotencyStore.Release(ctx, key, claim)
}

func TestIdempotencyStoreFailureOnBeginIsA503(t *testing.T) {
	h := &countingHandler{}
	store := &failingStore{IdempotencyStore: middleware.NewMemoryIdempotency(), beginErr: errors.New("redis: connection refused 10.0.0.5:6379")}
	rec := post(idemAPI(h, middleware.IdempotencyConfig{Store: store}), "k", "x")
	if rec.Code != 503 || h.calls.Load() != 0 || strings.Contains(rec.Body.String(), "redis") || strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatalf("an unreachable store must not run the handler (it could duplicate the work) nor leak details: %d calls=%d %s", rec.Code, h.calls.Load(), rec.Body)
	}
}

func TestIdempotencyStoreFailureOnFinishStillAnswers(t *testing.T) {
	h := &countingHandler{}
	store := &failingStore{IdempotencyStore: middleware.NewMemoryIdempotency(), finishErr: errors.New("write failed")}
	rec := post(idemAPI(h, middleware.IdempotencyConfig{Store: store}), "k", "x")
	if rec.Code != 201 || rec.Body.String() != "made" || store.finishes.Load() != 1 {
		t.Fatalf("the client still gets the real response: %d %q", rec.Code, rec.Body)
	}
}

func TestIdempotencyStoreFailureOnReleaseStillAnswers(t *testing.T) {
	h := &countingHandler{fail: func(int32) (int, string) { return 500, "boom" }}
	store := &failingStore{IdempotencyStore: middleware.NewMemoryIdempotency(), releaseErr: errors.New("write failed")}
	if rec := post(idemAPI(h, middleware.IdempotencyConfig{Store: store}), "k", "x"); rec.Code != 500 || store.releases.Load() != 1 {
		t.Fatalf("%d releases=%d", rec.Code, store.releases.Load())
	}
}

type ctxSpyStore struct {
	middleware.IdempotencyStore
	finishCtxErr atomic.Value
}

func (s *ctxSpyStore) Finish(ctx context.Context, key, claim string, e middleware.IdempotencyEntry, ttl time.Duration) error {
	s.finishCtxErr.Store(fmt.Sprint(ctx.Err()))
	return s.IdempotencyStore.Finish(ctx, key, claim, e, ttl)
}

func TestIdempotencyFinishesEvenWhenTheClientHungUp(t *testing.T) {
	store := &ctxSpyStore{IdempotencyStore: middleware.NewMemoryIdempotency()}
	ctx, cancel := context.WithCancel(context.Background())
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		w.WriteHeader(201)
	})
	mw := middleware.Idempotency(middleware.IdempotencyConfig{Store: store})(h)
	req := httptest.NewRequest("POST", "/things", strings.NewReader("x")).WithContext(ctx)
	req.Header.Set("Idempotency-Key", "k-gone")
	mw.ServeHTTP(httptest.NewRecorder(), req)
	if got := store.finishCtxErr.Load(); got != "<nil>" {
		t.Fatalf("the result must be stored even when the request was cancelled, but Finish got ctx.Err()=%v", got)
	}
}

type ttlSpyStore struct {
	middleware.IdempotencyStore
	begin *atomic.Int64
}

func (s *ttlSpyStore) Begin(ctx context.Context, key, fp string, ttl time.Duration) (*middleware.IdempotencyEntry, string, error) {
	s.begin.Store(int64(ttl))
	return s.IdempotencyStore.Begin(ctx, key, fp, ttl)
}

func TestIdempotencyLockTTLIsSeparateFromTheResponseTTL(t *testing.T) {
	var seen atomic.Int64
	store := &ttlSpyStore{IdempotencyStore: middleware.NewMemoryIdempotency(), begin: &seen}
	post(idemAPI(&countingHandler{}, middleware.IdempotencyConfig{Store: store, TTL: 24 * time.Hour}), "k", "x")
	if got := time.Duration(seen.Load()); got != 5*time.Minute {
		t.Fatalf("the in-progress claim must not live as long as stored responses, a crash would block the key: %v", got)
	}
	post(idemAPI(&countingHandler{}, middleware.IdempotencyConfig{Store: store, LockTTL: 7 * time.Second}), "k2", "x")
	if got := time.Duration(seen.Load()); got != 7*time.Second {
		t.Fatalf("LockTTL is configurable: %v", got)
	}
}

func TestTraceToleratesNilSpanAndContext(t *testing.T) {
	tr := &recordingTracer{nilSpan: true, nilCtx: true}
	var sawCtx context.Context
	h := middleware.Trace(tr)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawCtx = r.Context()
		w.WriteHeader(204)
	}))
	rec := do(h, "GET", "/x")
	if rec.Code != 204 || sawCtx == nil || len(tr.results) != 0 {
		t.Fatalf("a nil span and a nil context are allowed, with no recoverer to hide a panic: %d ctx=%v results=%v", rec.Code, sawCtx, tr.results)
	}
}

func TestIdempotencyKeysAreScopedPerCaller(t *testing.T) {
	h := &countingHandler{}
	api := idemAPI(h, middleware.IdempotencyConfig{})
	first := do2(api, "POST", "/things", "x", "Idempotency-Key", "k", "Authorization", "Bearer alice")
	bob := do2(api, "POST", "/things", "x", "Idempotency-Key", "k", "Authorization", "Bearer bob")
	if bob.Code != 201 || bob.Header().Get("Idempotent-Replayed") != "" || h.calls.Load() != 2 {
		t.Fatalf("another credential gets its own execution, never alice's response: %d %v calls=%d", bob.Code, bob.Header(), h.calls.Load())
	}
	again := do2(api, "POST", "/things", "x", "Idempotency-Key", "k", "Authorization", "Bearer alice")
	if again.Header().Get("Idempotent-Replayed") != "true" || again.Body.String() != first.Body.String() || h.calls.Load() != 2 {
		t.Fatalf("the same caller still replays: %v calls=%d", again.Header(), h.calls.Load())
	}
	do2(api, "POST", "/things", "x", "Idempotency-Key", "c", "Cookie", "sid=a")
	if rec := do2(api, "POST", "/things", "x", "Idempotency-Key", "c", "Cookie", "sid=b"); rec.Header().Get("Idempotent-Replayed") != "" || h.calls.Load() != 4 {
		t.Fatalf("another session must not get a replay: %v calls=%d", rec.Header(), h.calls.Load())
	}
}

func TestIdempotencyKeySquattingDoesNotBlockAnotherCaller(t *testing.T) {
	h := &countingHandler{}
	api := idemAPI(h, middleware.IdempotencyConfig{})
	do2(api, "POST", "/things", "junk", "Idempotency-Key", "victim", "Authorization", "Bearer mallory")
	if rec := do2(api, "POST", "/things", "real", "Idempotency-Key", "victim", "Authorization", "Bearer alice"); rec.Code != 201 {
		t.Fatalf("alice is blocked by mallory using her key first: %d %s", rec.Code, rec.Body)
	}
}

func TestIdempotencyCustomScope(t *testing.T) {
	h := &countingHandler{}
	cfg := middleware.IdempotencyConfig{Scope: func(r *http.Request) string { return r.Header.Get("X-Tenant") }}
	api := idemAPI(h, cfg)
	do2(api, "POST", "/things", "x", "Idempotency-Key", "k", "X-Tenant", "a", "Authorization", "Bearer one")
	if rec := do2(api, "POST", "/things", "x", "Idempotency-Key", "k", "X-Tenant", "a", "Authorization", "Bearer two"); rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("a custom scope replaces the default: %v", rec.Header())
	}
	if rec := do2(api, "POST", "/things", "x", "Idempotency-Key", "k", "X-Tenant", "b", "Authorization", "Bearer one"); rec.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("another tenant: %v", rec.Header())
	}
}

func TestIdempotencyFingerprintFieldsCannotBeShifted(t *testing.T) {
	h := &countingHandler{}
	api := idemAPI(h, middleware.IdempotencyConfig{})
	do2(api, "POST", "/things", "bc", "Idempotency-Key", "k", "Accept-Encoding", "a")
	if rec := do2(api, "POST", "/things", "c", "Idempotency-Key", "k", "Accept-Encoding", "ab"); rec.Code != 422 || h.calls.Load() != 1 {
		t.Fatalf("moving a byte from one field to the next must change the fingerprint: %d calls=%d", rec.Code, h.calls.Load())
	}
}

func TestIdempotencyReplayNeverCopiesCallerSpecificHeaders(t *testing.T) {
	var calls atomic.Int32
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.SetCookie(w, &http.Cookie{Name: "csrf", Value: "first"})
		w.Header().Set("Location", "/things/1")
		w.Header().Set("X-Request-Id", "inner-id")
		w.WriteHeader(201)
	})
	idem := middleware.Idempotency(middleware.IdempotencyConfig{})(inner)
	cors := middleware.CORS(middleware.CORSConfig{AllowOrigins: []string{"https://a.example", "https://b.example"}, AllowCredentials: true})
	h := middleware.RequestID(middleware.RequestIDConfig{})(cors(idem))
	do(h, "POST", "/x", "Idempotency-Key", "k", "Origin", "https://a.example", "X-Request-ID", "req-first")
	second := do(h, "POST", "/x", "Idempotency-Key", "k", "Origin", "https://b.example", "X-Request-ID", "req-second")
	if second.Header().Get("Idempotent-Replayed") != "true" || second.Header().Get("Location") != "/things/1" {
		t.Fatalf("setup: %v", second.Header())
	}
	if got := second.Header().Get("X-Request-ID"); got != "req-second" {
		t.Fatalf("the replay keeps its own request id: %q", got)
	}
	if got := second.Header().Get("Access-Control-Allow-Origin"); got != "https://b.example" {
		t.Fatalf("the replay keeps its own CORS grant: %q", got)
	}
	if got := second.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Fatalf("a cookie from the first response is never replayed: %v", got)
	}
}

func TestIdempotencyReplayDoesNotSendGzipToAClientThatDidNotAskForIt(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(201)
		fmt.Fprint(w, strings.Repeat("hello ", 50))
	})
	h := middleware.Idempotency(middleware.IdempotencyConfig{})(middleware.Gzip()(inner))
	do(h, "POST", "/x", "Idempotency-Key", "k", "Accept-Encoding", "gzip")
	rec := do(h, "POST", "/x", "Idempotency-Key", "k")
	if rec.Header().Get("Content-Encoding") != "" || strings.HasPrefix(rec.Body.String(), "\x1f\x8b") {
		t.Fatalf("a client without Accept-Encoding must not get a gzip body: %v", rec.Header())
	}
	if rec.Header().Get("Idempotent-Replayed") == "true" {
		t.Fatalf("a different Accept-Encoding is a different request: %v", rec.Header())
	}
}

type nilEntryStore struct{ middleware.IdempotencyStore }

func (nilEntryStore) Begin(context.Context, string, string, time.Duration) (*middleware.IdempotencyEntry, string, error) {
	return nil, "", nil
}

func TestIdempotencyStoreReturningNothingIsA503(t *testing.T) {
	h := &countingHandler{}
	rec := post(idemAPI(h, middleware.IdempotencyConfig{Store: nilEntryStore{}}), "k", "x")
	if rec.Code != 503 || h.calls.Load() != 0 {
		t.Fatalf("%d calls=%d", rec.Code, h.calls.Load())
	}
}
