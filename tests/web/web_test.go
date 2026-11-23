package web_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/swiftly-headquarters/minato/middleware"
	"github.com/swiftly-headquarters/minato/server"
	"github.com/swiftly-headquarters/minato/web"
)

func files() fstest.MapFS {
	return fstest.MapFS{
		"index.html":        {Data: []byte("<html>app</html>")},
		"assets/app.js":     {Data: []byte("console.log(1)")},
		"docs/index.html":   {Data: []byte("docs")},
		"empty/x.txt":       {Data: []byte("x")},
		"big.txt":           {Data: []byte("0123456789")},
		".env":              {Data: []byte("SECRET")},
		".git/config":       {Data: []byte("SECRET")},
		".well-known/a.txt": {Data: []byte("wk")},
	}
}

func get(h http.Handler, method, url string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestStatic(t *testing.T) {
	h := web.Static(files(), web.Config{Immutable: "/assets/"})

	rec := get(h, "GET", "/")
	if rec.Code != 200 || rec.Body.String() != "<html>app</html>" || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("index: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	rec = get(h, "GET", "/assets/app.js")
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "javascript") || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d %v", rec.Code, rec.Header())
	}
	if rec := get(h, "GET", "/big.txt"); rec.Header().Get("Cache-Control") != "" {
		t.Errorf("only the Immutable prefix is cached: %v", rec.Header())
	}
	rec = get(h, "GET", "/missing.js")
	if rec.Code != 404 || rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("missing: %d %v", rec.Code, rec.Header())
	}

	if rec := get(h, "GET", "/docs/"); rec.Code != 200 || rec.Body.String() != "docs" {
		t.Fatalf("directory index: %d %q", rec.Code, rec.Body)
	}
	rec = get(h, "GET", "/docs?a=1")
	if rec.Code != 301 || rec.Header().Get("Location") != "docs/?a=1" {
		t.Fatalf("directory without slash: %d %v", rec.Code, rec.Header())
	}
	if rec := get(h, "GET", "/empty/"); rec.Code != 404 || strings.Contains(rec.Body.String(), "x.txt") {
		t.Fatalf("directories must not be listed: %d %s", rec.Code, rec.Body)
	}

	for _, p := range []string{"/.env", "/.git/config", "/%2e%2e/%2e%2e/.env", "/docs/../.env", "/.git/"} {
		if rec := get(h, "GET", p); rec.Code != 404 || strings.Contains(rec.Body.String(), "SECRET") {
			t.Errorf("%s must be hidden: %d %s", p, rec.Code, rec.Body)
		}
	}
	if rec := get(h, "GET", "/.well-known/a.txt"); rec.Code != 200 {
		t.Errorf(".well-known must be served: %d", rec.Code)
	}

	rec = get(h, "GET", "/big.txt", "Range", "bytes=0-3")
	if rec.Code != 206 || rec.Body.String() != "0123" {
		t.Fatalf("range: %d %q", rec.Code, rec.Body)
	}
	if rec := get(h, "HEAD", "/big.txt"); rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %q", rec.Code, rec.Body)
	}
	if rec := get(h, "POST", "/big.txt"); rec.Code != 405 || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST: %d %v", rec.Code, rec.Header())
	}
	if rec := get(h, "GET", "/users/42", "Accept", "text/html"); rec.Code != 404 {
		t.Fatalf("without SPA an unknown page is a 404: %d", rec.Code)
	}
}

func TestSPA(t *testing.T) {
	h := web.Static(files(), web.Config{SPA: true})
	const html = "text/html,application/xhtml+xml,*/*;q=0.8"

	for _, p := range []string{"/users/42", "/settings", "/empty/nope"} {
		rec := get(h, "GET", p, "Accept", html)
		if rec.Code != 200 || rec.Body.String() != "<html>app</html>" || rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: %d %q", p, rec.Code, rec.Body)
		}
	}
	for name, c := range map[string]struct{ path, accept string }{
		"fetch call":    {"/api/users", "application/json"},
		"no accept":     {"/users/42", ""},
		"missing asset": {"/assets/old.js", html},
		"missing image": {"/logo.png", html},
	} {
		if rec := get(h, "GET", c.path, "Accept", c.accept); rec.Code != 404 {
			t.Errorf("%s: %d, want 404", name, rec.Code)
		}
	}
	if rec := get(h, "POST", "/users/42", "Accept", html); rec.Code != 405 {
		t.Errorf("POST must not fall back: %d", rec.Code)
	}
	if rec := get(h, "GET", "/big.txt", "Accept", html); rec.Body.String() != "0123456789" {
		t.Error("real files win over the fallback")
	}
}

func TestMountedOnAPI(t *testing.T) {
	api := server.New("t", "1")
	server.Get(api, "/api/ping", func(ctx context.Context, _ *struct{}) (*struct{ Body string }, error) {
		return &struct{ Body string }{"pong"}, nil
	})
	server.Handle(api, "/", web.Static(files(), web.Config{SPA: true}))
	v1 := api.Group("/v1", func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Group", "1")
			next.ServeHTTP(w, r)
		})
	})
	server.Handle(v1, "GET /files/", http.StripPrefix("/v1/files", web.Static(files(), web.Config{})))

	if rec := get(api, "GET", "/api/ping"); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `"pong"` {
		t.Fatalf("typed route must win over the catch-all: %d %s", rec.Code, rec.Body)
	}
	if rec := get(api, "GET", "/openapi.json"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"openapi"`) {
		t.Fatalf("spec must win over the catch-all: %d", rec.Code)
	}
	if rec := get(api, "GET", "/docs"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "elements-api") {
		t.Fatalf("docs must win over the catch-all: %d", rec.Code)
	}
	if rec := get(api, "GET", "/users/42", "Accept", "text/html"); rec.Code != 200 || rec.Body.String() != "<html>app</html>" {
		t.Fatalf("catch-all: %d %q", rec.Code, rec.Body)
	}
	rec := get(api, "GET", "/v1/files/big.txt")
	if rec.Code != 200 || rec.Body.String() != "0123456789" || rec.Header().Get("X-Group") != "1" {
		t.Fatalf("group mount: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	if rec := get(api, "GET", "/v1/files/docs?x=1"); rec.Header().Get("Location") != "docs/?x=1" {
		t.Errorf("redirect must stay relative behind StripPrefix: %v", rec.Header())
	}
	if _, ok := api.Spec.Paths["/"]; ok {
		t.Error("mounted handlers must not appear in the spec")
	}
}

func TestProxy(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s?%s xff=%s host=%s", r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Forwarded-For"), r.Host)
	}))
	defer up.Close()

	for _, bad := range []string{"", "localhost:3000", "ftp://x", "http://"} {
		if _, err := web.Proxy(bad); err == nil {
			t.Errorf("Proxy(%q) must fail", bad)
		}
	}
	h, err := web.Proxy(up.URL + "/base")
	if err != nil {
		t.Fatal(err)
	}
	api := server.New("t", "1")
	server.Get(api, "/api/ping", func(ctx context.Context, _ *struct{}) (*struct{ Body string }, error) {
		return &struct{ Body string }{"pong"}, nil
	})
	server.Handle(api, "/", h)

	if rec := get(api, "GET", "/api/ping"); !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("typed route must not be proxied: %s", rec.Body)
	}
	rec := get(api, "GET", "/app/page?x=1", "X-Forwarded-For", "6.6.6.6")
	want := "/base/app/page?x=1 xff=192.0.2.1 host=" + strings.TrimPrefix(up.URL, "http://")
	if rec.Code != 200 || rec.Body.String() != want {
		t.Fatalf("proxied:\n got %d %q\nwant %q", rec.Code, rec.Body, want)
	}

	dead, _ := web.Proxy("http://127.0.0.1:1")
	rec = get(dead, "GET", "/")
	if rec.Code != 502 || rec.Header().Get("Content-Type") != "application/problem+json" || strings.Contains(rec.Body.String(), "127.0.0.1") {
		t.Fatalf("dead upstream: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
}

func TestProxyWebSocketThroughMiddleware(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("upstream hijack: %v", err)
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		line, _ := rw.ReadString('\n')
		rw.WriteString("echo:" + line)
		rw.Flush()
	}))
	defer up.Close()

	h, _ := web.Proxy(up.URL)
	api := server.New("t", "1")
	api.Use(middleware.Logger(nil), middleware.Gzip())
	server.Handle(api, "/", h)
	srv := httptest.NewServer(api)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rw, ok := resp.Body.(io.ReadWriteCloser)
	if resp.StatusCode != 101 || !ok {
		t.Fatalf("upgrade failed: %d writable=%v", resp.StatusCode, ok)
	}
	io.WriteString(rw, "hi\n")
	if got, _ := io.ReadAll(rw); string(got) != "echo:hi\n" {
		t.Fatalf("tunnel: %q", got)
	}
}
