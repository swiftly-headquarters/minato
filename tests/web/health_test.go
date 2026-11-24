package web_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/swiftly-headquarters/minato/middleware"
	"github.com/swiftly-headquarters/minato/server"
	"github.com/swiftly-headquarters/minato/web"
)

type healthBody struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func readiness(t *testing.T, h http.Handler) (int, healthBody, *httptest.ResponseRecorder) {
	t.Helper()
	rec := get(h, "GET", "/readyz")
	var b healthBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	return rec.Code, b, rec
}

func TestLive(t *testing.T) {
	rec := get(web.Live(), "GET", "/healthz")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` ||
		rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("live: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
}

func TestReady(t *testing.T) {
	if code, b, _ := readiness(t, web.Ready(time.Second, nil)); code != 200 || b.Status != "ok" || b.Checks != nil {
		t.Errorf("no checks is ready: %d %+v", code, b)
	}

	ok := func(context.Context) error { return nil }
	code, b, _ := readiness(t, web.Ready(time.Second, map[string]web.Check{"db": ok, "cache": ok}))
	if code != 200 || b.Status != "ok" || b.Checks["db"] != "ok" || b.Checks["cache"] != "ok" {
		t.Errorf("all passing: %d %+v", code, b)
	}

	h := web.Ready(time.Second, map[string]web.Check{
		"db":    func(context.Context) error { return errors.New("dial tcp 10.1.2.3:5432: secret connection string") },
		"cache": ok,
		"boom":  func(context.Context) error { panic("kaboom") },
	})
	code, b, rec := readiness(t, h)
	if code != 503 || b.Status != "unavailable" || b.Checks["db"] != "failed" || b.Checks["cache"] != "ok" || b.Checks["boom"] != "failed" {
		t.Errorf("failing checks: %d %+v", code, b)
	}
	if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "10.1.2.3") || strings.Contains(rec.Body.String(), "kaboom") {
		t.Errorf("internal details must stay out of the response: %s", rec.Body)
	}
}

func TestReadyTimeoutAndConcurrency(t *testing.T) {
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	h := web.Ready(50*time.Millisecond, map[string]web.Check{
		"stuck": func(context.Context) error { <-stuck; return nil },
		"fine":  func(context.Context) error { return nil },
	})
	start := time.Now()
	code, b, _ := readiness(t, h)
	if code != 503 || b.Checks["stuck"] != "timeout" || b.Checks["fine"] != "ok" || time.Since(start) > 2*time.Second {
		t.Errorf("a check that ignores its context: %d %+v after %v", code, b, time.Since(start))
	}

	aStarted, bStarted := make(chan struct{}), make(chan struct{})
	barrier := web.Ready(5*time.Second, map[string]web.Check{
		"a": func(context.Context) error { close(aStarted); <-bStarted; return nil },
		"b": func(context.Context) error { close(bStarted); <-aStarted; return nil },
	})
	if code, b, _ := readiness(t, barrier); code != 200 || b.Checks["a"] != "ok" || b.Checks["b"] != "ok" {
		t.Errorf("checks must run concurrently, each waits for the other: %d %+v", code, b)
	}

	var deadlined bool
	web.Ready(time.Second, map[string]web.Check{"d": func(ctx context.Context) error {
		_, deadlined = ctx.Deadline()
		return nil
	}}).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/readyz", nil))
	if !deadlined {
		t.Error("checks must get a context with the timeout")
	}
}

func TestHealthMountedOnAPI(t *testing.T) {
	api := server.New("t", "1")
	server.Handle(api, "GET /healthz", web.Live())
	server.Handle(api, "GET /readyz", web.Ready(time.Second, nil))
	for _, p := range []string{"/healthz", "/readyz"} {
		if rec := get(api, "GET", p); rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if rec := get(api, "HEAD", "/healthz"); rec.Code != 200 {
		t.Errorf("HEAD /healthz: %d", rec.Code)
	}
}

func TestRequestIDReachesTheUpstream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.Header.Get("X-Request-ID")))
	}))
	defer up.Close()
	proxy, err := web.Proxy(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	api := server.New("t", "1")
	api.Use(middleware.RequestID(middleware.RequestIDConfig{}))
	server.Handle(api, "/", proxy)

	rec := get(api, "GET", "/anything")
	if id := rec.Header().Get("X-Request-ID"); id == "" || rec.Body.String() != id {
		t.Errorf("the proxied service must see the same id the client gets: header %q, upstream saw %q", id, rec.Body)
	}
	if rec := get(api, "GET", "/anything", "X-Request-ID", "trace-9"); rec.Body.String() != "trace-9" {
		t.Errorf("an incoming id is forwarded: %q", rec.Body)
	}
}
