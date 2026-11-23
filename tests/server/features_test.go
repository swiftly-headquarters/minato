package server_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/swiftly-headquarters/minato/server"
)

type empty = struct{}

func noop(ctx context.Context, _ *empty) (*empty, error) { return nil, nil }

func call(h http.Handler, method, url, body string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(c int) { s.code = c; s.ResponseWriter.WriteHeader(c) }

func TestPanicRecovery(t *testing.T) {
	api := server.New("t", "1")
	var seen int
	api.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sr := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(sr, r)
			seen = sr.code
		})
	})
	server.Get(api, "/boom", func(ctx context.Context, _ *empty) (*empty, error) { panic("secret internals") })
	server.Get(api, "/abort", func(ctx context.Context, _ *empty) (*empty, error) { panic(http.ErrAbortHandler) })
	server.Get(api, "/ok", noop)

	rec := call(api, "GET", "/boom", "")
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "secret") || rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if seen != 500 {
		t.Fatalf("middleware saw status %d, want 500", seen)
	}
	if rec := call(api, "GET", "/ok", ""); rec.Code != 204 {
		t.Fatalf("API unusable after a panic: %d", rec.Code)
	}
	func() {
		defer func() {
			if recover() != http.ErrAbortHandler {
				t.Fatal("http.ErrAbortHandler must propagate")
			}
		}()
		call(api, "GET", "/abort", "")
	}()

	bad := server.New("t", "1")
	bad.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("in middleware") })
	})
	if rec := call(bad, "GET", "/docs", ""); rec.Code != 500 {
		t.Fatalf("panic in middleware: got %d", rec.Code)
	}
}

type fmtBody struct {
	Email string `json:"email" format:"email"`
	ID    string `json:"id" format:"uuid"`
	When  string `json:"when" format:"date-time"`
	Day   string `json:"day" format:"date"`
	Link  string `json:"link" format:"uri"`
	V4    string `json:"v4" format:"ipv4"`
	V6    string `json:"v6" format:"ipv6"`
	Data  []byte `json:"data"`
}

func TestFormatValidation(t *testing.T) {
	api := server.New("t", "1")
	server.Post(api, "/f", func(ctx context.Context, in *struct{ Body fmtBody }) (*empty, error) { return nil, nil })

	valid := map[string]string{
		"email": "a@example.com", "id": "123e4567-e89b-12d3-a456-426614174000", "when": "2026-11-22T10:00:00Z",
		"day": "2026-11-22", "link": "https://example.com/x", "v4": "10.0.0.1", "v6": "::1", "data": "aGk=",
	}
	send := func(field, value string) *httptest.ResponseRecorder {
		m := map[string]string{}
		for k, v := range valid {
			m[k] = v
		}
		if field != "" {
			m[field] = value
		}
		b, _ := json.Marshal(m)
		return call(api, "POST", "/f", string(b))
	}
	if rec := send("", ""); rec.Code != 204 {
		t.Fatalf("valid body rejected: %d %s", rec.Code, rec.Body)
	}
	for _, c := range [][2]string{
		{"email", "not an email"}, {"email", "Ada <a@example.com>"}, {"id", "123"}, {"when", "yesterday"},
		{"day", "2026-13-40"}, {"link", "example.com"}, {"v4", "::1"}, {"v6", "10.0.0.1"}, {"data", "!!"},
	} {
		rec := send(c[0], c[1])
		var em server.ErrorModel
		json.Unmarshal(rec.Body.Bytes(), &em)
		if rec.Code != 422 || len(em.Errors) != 1 || em.Errors[0].Location != "body."+c[0] {
			t.Errorf("%s=%q: got %d %s", c[0], c[1], rec.Code, rec.Body)
		}
	}
}

func TestGroupsAndMiddleware(t *testing.T) {
	api := server.New("t", "1")
	api.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Global", "1")
			next.ServeHTTP(w, r)
		})
	})
	var order []string
	mark := func(name string) server.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer ok" {
				server.WriteError(w, server.Error401("no token"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	hits := 0
	h := func(ctx context.Context, _ *empty) (*empty, error) { hits++; return nil, nil }

	v1 := api.Group("/v1", mark("v1"))
	v1.Tags = []string{"v1"}
	server.Get(v1, "/", h)
	server.Get(v1, "/things", h, func(o *server.Operation) { o.Tags = []string{"things"} })
	admin := v1.Group("/admin", mark("admin"), auth)
	server.Get(admin, "/stats", h)

	if rec := call(api, "GET", "/v1/things", ""); rec.Code != 204 || !slices.Equal(order, []string{"v1"}) || rec.Header().Get("X-Global") != "1" {
		t.Fatalf("got %d order=%v hdr=%v", rec.Code, order, rec.Header())
	}
	if call(api, "GET", "/v1/", "").Code != 204 || call(api, "GET", "/v1/nope", "").Code != 404 {
		t.Fatal(`group root "/" must match exactly /v1/`)
	}

	order, hits = nil, 0
	rec := call(api, "GET", "/v1/admin/stats", "")
	if rec.Code != 401 || hits != 0 || !strings.Contains(rec.Body.String(), "no token") || !slices.Equal(order, []string{"v1", "admin"}) {
		t.Fatalf("unauthenticated: %d hits=%d order=%v %s", rec.Code, hits, order, rec.Body)
	}
	if rec := call(api, "GET", "/v1/admin/stats", "", "Authorization", "Bearer ok"); rec.Code != 204 || hits != 1 {
		t.Fatalf("authenticated: %d hits=%d", rec.Code, hits)
	}

	if g := call(api, "GET", "/openapi.json", ""); g.Header().Get("X-Global") != "1" {
		t.Fatal("API.Use must wrap the docs routes too")
	}
	for path, tags := range map[string][]string{"/v1/": {"v1"}, "/v1/things": {"v1", "things"}, "/v1/admin/stats": {"v1"}} {
		if op := api.Spec.Paths[path]["get"]; op == nil || !slices.Equal(op.Tags, tags) {
			t.Errorf("%s: %+v, want tags %v", path, op, tags)
		}
	}
}

func TestSecurityAndErrorDocs(t *testing.T) {
	api := server.New("t", "1")
	api.BearerAuth("bearer")
	g := api.Group("/secure")
	g.Security = server.Secured("bearer", "read")
	server.Get(g, "/a", noop, func(o *server.Operation) { o.Errors = []int{404, 409} })
	server.Get(g, "/b", noop, func(o *server.Operation) { o.Security = server.Secured("other") })
	server.Get(api, "/c", noop)

	sec := func(path string) string {
		b, _ := json.Marshal(api.Spec.Paths[path]["get"].Security)
		return string(b)
	}
	if sec("/secure/a") != `[{"bearer":["read"]}]` || sec("/secure/b") != `[{"other":[]}]` || sec("/c") != "null" {
		t.Fatalf("security: %s %s %s", sec("/secure/a"), sec("/secure/b"), sec("/c"))
	}
	resp := api.Spec.Paths["/secure/a"]["get"].Responses
	for _, code := range []string{"204", "404", "409", "default"} {
		if resp[code] == nil {
			t.Errorf("missing response %s", code)
		}
	}
	if r := resp["404"]; r == nil || r.Description != "Not Found" || r.Content["application/problem+json"] == nil {
		t.Errorf("bad 404 response: %+v", r)
	}
	if s, _ := api.Spec.Components.SecuritySchemes["bearer"].(map[string]any); s["scheme"] != "bearer" || s["type"] != "http" {
		t.Errorf("bad scheme: %v", api.Spec.Components.SecuritySchemes)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("a non-error status in Operation.Errors must panic")
		}
	}()
	server.Get(api, "/d", noop, func(o *server.Operation) { o.Errors = []int{200} })
}

func TestRunGracefulShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	api := server.New("t", "1")
	started, release := make(chan struct{}), make(chan struct{})
	server.Get(api, "/slow", func(ctx context.Context, _ *empty) (*empty, error) {
		close(started)
		<-release
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- api.Run(ctx, addr) }()

	status := make(chan int, 1)
	go func() {
		for {
			resp, err := http.Get("http://" + addr + "/slow")
			if err == nil {
				resp.Body.Close()
				status <- resp.StatusCode
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("server never started")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("Run returned (%v) while a request was in flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if code := <-status; code != 204 {
		t.Fatalf("in-flight request got %d", code)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}
