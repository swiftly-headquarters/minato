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

func (f *failingStore) Begin(ctx context.Context, key, fp string, ttl time.Duration) (*middleware.IdempotencyEntry, bool, error) {
	if f.beginErr != nil {
		return nil, false, f.beginErr
	}
	return f.IdempotencyStore.Begin(ctx, key, fp, ttl)
}

func (f *failingStore) Finish(ctx context.Context, key string, e middleware.IdempotencyEntry, ttl time.Duration) error {
	f.finishes.Add(1)
	if f.finishErr != nil {
		return f.finishErr
	}
	return f.IdempotencyStore.Finish(ctx, key, e, ttl)
}

func (f *failingStore) Release(ctx context.Context, key string) error {
	f.releases.Add(1)
	if f.releaseErr != nil {
		return f.releaseErr
	}
	return f.IdempotencyStore.Release(ctx, key)
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

func (s *ctxSpyStore) Finish(ctx context.Context, key string, e middleware.IdempotencyEntry, ttl time.Duration) error {
	s.finishCtxErr.Store(fmt.Sprint(ctx.Err()))
	return s.IdempotencyStore.Finish(ctx, key, e, ttl)
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

func (s *ttlSpyStore) Begin(ctx context.Context, key, fp string, ttl time.Duration) (*middleware.IdempotencyEntry, bool, error) {
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
