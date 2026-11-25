package middleware_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/swiftly-headquarters/minato/middleware"
	"github.com/swiftly-headquarters/minato/server"
)

type empty = struct{}

type textOut struct {
	Body struct {
		Text string `json:"text"`
	}
}

func newAPI(mws ...server.Middleware) *server.API {
	api := server.New("t", "1")
	api.Use(mws...)
	server.Get(api, "/text", func(ctx context.Context, _ *empty) (*textOut, error) {
		out := &textOut{}
		out.Body.Text = strings.Repeat("hello ", 100)
		return out, nil
	})
	server.Get(api, "/nothing", func(ctx context.Context, _ *empty) (*empty, error) { return nil, nil })
	server.Get(api, "/missing", func(ctx context.Context, _ *empty) (*empty, error) { return nil, server.Error404("nope") })
	return api
}

func do(h http.Handler, method, url string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLogger(t *testing.T) {
	var buf bytes.Buffer
	api := newAPI(middleware.Logger(slog.New(slog.NewTextHandler(&buf, nil))))
	do(api, "GET", "/missing?token=hunter2")
	do(api, "GET", "/text")
	log := buf.String()
	for _, want := range []string{"method=GET", "path=/missing", "status=404", "status=200", "duration="} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "hunter2") {
		t.Errorf("query string leaked into the log:\n%s", log)
	}
	if !strings.Contains(log, "bytes=") || strings.Count(log, "msg=request") != 2 {
		t.Errorf("expected one line per request with bytes:\n%s", log)
	}
}

func TestCORS(t *testing.T) {
	cfg := middleware.CORSConfig{
		AllowOrigins: []string{"https://app.example.com"}, AllowCredentials: true,
		ExposeHeaders: []string{"X-Total"}, MaxAge: time.Hour,
	}
	api := newAPI(middleware.CORS(cfg))

	rec := do(api, "OPTIONS", "/text", "Origin", "https://app.example.com",
		"Access-Control-Request-Method", "PUT", "Access-Control-Request-Headers", "X-Custom")
	h := rec.Header()
	if rec.Code != 204 || h.Get("Access-Control-Allow-Origin") != "https://app.example.com" ||
		h.Get("Access-Control-Allow-Credentials") != "true" || h.Get("Access-Control-Allow-Headers") != "X-Custom" ||
		!strings.Contains(h.Get("Access-Control-Allow-Methods"), "PUT") || h.Get("Access-Control-Max-Age") != "3600" {
		t.Fatalf("preflight: %d %v", rec.Code, h)
	}

	rec = do(api, "GET", "/text", "Origin", "https://app.example.com")
	if rec.Code != 200 || rec.Header().Get("Access-Control-Expose-Headers") != "X-Total" || rec.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Fatalf("simple request: %d %v", rec.Code, rec.Header())
	}

	rec = do(api, "GET", "/text", "Origin", "https://evil.example.com")
	if rec.Code != 200 || rec.Header().Get("Access-Control-Allow-Origin") != "" || !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Fatalf("foreign origin must get no CORS headers but Vary: Origin: %v", rec.Header())
	}
	rec = do(api, "OPTIONS", "/text", "Origin", "https://evil.example.com", "Access-Control-Request-Method", "GET")
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("preflight from a foreign origin must not be approved")
	}
	if rec := do(api, "GET", "/text"); rec.Code != 200 || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("requests without Origin are untouched")
	}

	wild := newAPI(middleware.CORS(middleware.CORSConfig{}))
	rec = do(wild, "GET", "/text", "Origin", "https://any.example.com")
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" || rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("wildcard: %v", rec.Header())
	}

	for name, c := range map[string]middleware.CORSConfig{
		"wildcard with credentials": {AllowOrigins: []string{"*"}, AllowCredentials: true},
		"credentials with no list":  {AllowCredentials: true},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s must panic", name)
				}
			}()
			middleware.CORS(c)
		}()
	}
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestGzip(t *testing.T) {
	api := newAPI(middleware.Gzip())
	plain := do(api, "GET", "/text").Body.String()

	rec := do(api, "GET", "/text", "Accept-Encoding", "gzip, br")
	if rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("Content-Type") != "application/json" ||
		!strings.Contains(strings.Join(rec.Header().Values("Vary"), ","), "Accept-Encoding") || gunzip(t, rec.Body.Bytes()) != plain {
		t.Fatalf("compressed response wrong: %v", rec.Header())
	}
	if rec.Body.Len() >= len(plain) {
		t.Errorf("not smaller: %d >= %d", rec.Body.Len(), len(plain))
	}
	if rec := do(api, "GET", "/text", "Accept-Encoding", "gzip;q=0"); rec.Header().Get("Content-Encoding") != "" {
		t.Error("gzip;q=0 must not be compressed")
	}
	if rec := do(api, "GET", "/text", "Accept-Encoding", "identity"); rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != plain {
		t.Error("clients without gzip get the plain body")
	}
	if rec := do(api, "GET", "/nothing", "Accept-Encoding", "gzip"); rec.Code != 204 || rec.Header().Get("Content-Encoding") != "" || rec.Body.Len() != 0 {
		t.Errorf("204 must pass through: %d %v", rec.Code, rec.Header())
	}
	if rec := do(api, "HEAD", "/text", "Accept-Encoding", "gzip"); rec.Header().Get("Content-Encoding") != "" {
		t.Error("HEAD must pass through")
	}
	if rec := do(api, "GET", "/missing", "Accept-Encoding", "gzip"); rec.Code != 404 || !strings.Contains(gunzip(t, rec.Body.Bytes()), "nope") {
		t.Errorf("error responses are compressed too: %d", rec.Code)
	}
}

func TestStreamingThroughMiddleware(t *testing.T) {
	var buf bytes.Buffer
	api := server.New("t", "1")
	api.Use(middleware.Logger(slog.New(slog.NewTextHandler(&buf, nil))), middleware.Gzip())
	release := make(chan struct{})
	server.Get(api, "/events", func(ctx context.Context, _ *empty) (*struct{ Body server.SSE }, error) {
		return &struct{ Body server.SSE }{func(ctx context.Context, send func(server.SSEEvent) error) error {
			if err := send(server.SSEEvent{Data: "one"}); err != nil {
				return err
			}
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			return send(server.SSEEvent{Data: "two"})
		}}, nil
	})
	srv := httptest.NewServer(api)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !resp.Uncompressed {
		t.Fatal("response was not gzip-encoded")
	}
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil || line != "data: one\n" {
		t.Fatalf("first event must arrive while the handler is still running: %q %v", line, err)
	}
	close(release)
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "data: two") {
		t.Fatalf("second event missing: %q", rest)
	}
	if !strings.Contains(buf.String(), "status=200") {
		t.Errorf("logger lost the status through streaming:\n%s", buf.String())
	}
}

func TestTimeout(t *testing.T) {
	api := server.New("t", "1")
	api.Use(middleware.Timeout(50 * time.Millisecond))
	server.Get(api, "/slow", func(ctx context.Context, _ *empty) (*empty, error) {
		select {
		case <-ctx.Done():
			return nil, errors.Join(errors.New("db"), ctx.Err())
		case <-time.After(5 * time.Second):
			return nil, nil
		}
	})
	server.Get(api, "/fast", func(ctx context.Context, _ *empty) (*empty, error) { return nil, nil })

	rec := do(api, "GET", "/slow")
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "timed out") || strings.Contains(rec.Body.String(), "db") {
		t.Fatalf("slow: %d %s", rec.Code, rec.Body)
	}
	if rec := do(api, "GET", "/fast"); rec.Code != 204 {
		t.Fatalf("fast: %d", rec.Code)
	}
}
