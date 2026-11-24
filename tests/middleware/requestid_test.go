package middleware_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/swiftly-headquarters/minato/middleware"
	"github.com/swiftly-headquarters/minato/server"
)

var hexID = regexp.MustCompile(`^[0-9a-f]{32}$`)

type idIn struct {
	Header string `header:"X-Request-ID"`
	Other  string `header:"X-Correlation-ID"`
}

func idAPI(mw server.Middleware) *server.API {
	api := server.New("t", "1")
	api.Use(mw)
	server.Get(api, "/id", func(ctx context.Context, in *idIn) (*struct{ Body []string }, error) {
		return &struct{ Body []string }{[]string{middleware.RequestIDFromContext(ctx), in.Header, in.Other}}, nil
	})
	return api
}

func seen(t *testing.T, rec *httptest.ResponseRecorder) (ctxID, reqHeader, other string) {
	t.Helper()
	var got []string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != 3 {
		t.Fatalf("bad response %d %s", rec.Code, rec.Body)
	}
	return got[0], got[1], got[2]
}

func TestRequestIDDefault(t *testing.T) {
	api := idAPI(middleware.RequestID(middleware.RequestIDConfig{}))

	rec := do(api, "GET", "/id")
	ctxID, reqHeader, _ := seen(t, rec)
	if !hexID.MatchString(ctxID) || rec.Header().Get("X-Request-ID") != ctxID || reqHeader != ctxID {
		t.Fatalf("response header, context and request header must agree on a generated id: %q %q %q", rec.Header().Get("X-Request-ID"), ctxID, reqHeader)
	}
	if again, _, _ := seen(t, do(api, "GET", "/id")); again == ctxID {
		t.Error("each request needs its own id")
	}
	if middleware.RequestIDFromContext(context.Background()) != "" {
		t.Error("no id outside a request")
	}
}

func TestRequestIDIncoming(t *testing.T) {
	api := idAPI(middleware.RequestID(middleware.RequestIDConfig{}))

	rec := do(api, "GET", "/id", "X-Request-ID", "upstream-42.a:b/c")
	if id, hdr, _ := seen(t, rec); id != "upstream-42.a:b/c" || hdr != id || rec.Header().Get("X-Request-ID") != id {
		t.Errorf("a well-formed incoming id must be kept: %q", id)
	}
	for name, bad := range map[string]string{
		"spaces": "a b", "control": "a\x01b", "too long": strings.Repeat("x", 129), "non-ascii": "idé",
	} {
		rec := do(api, "GET", "/id", "X-Request-ID", bad)
		if id, _, _ := seen(t, rec); !hexID.MatchString(id) {
			t.Errorf("%s: a malformed incoming id must be replaced, got %q", name, id)
		}
	}
	if id, _, _ := seen(t, do(api, "GET", "/id", "X-Request-ID", strings.Repeat("x", 128))); len(id) != 128 {
		t.Error("128 characters is allowed")
	}

	ignoring := idAPI(middleware.RequestID(middleware.RequestIDConfig{IgnoreIncoming: true}))
	if id, _, _ := seen(t, do(ignoring, "GET", "/id", "X-Request-ID", "client-chosen")); !hexID.MatchString(id) {
		t.Errorf("IgnoreIncoming must always generate, got %q", id)
	}
}

func TestRequestIDCustomGenerator(t *testing.T) {
	var gotPath string
	api := idAPI(middleware.RequestID(middleware.RequestIDConfig{
		Generate: func(r *http.Request) string {
			gotPath = r.URL.Path
			return "api-" + middleware.NewRequestID()
		},
	}))
	rec := do(api, "GET", "/id")
	if id, _, _ := seen(t, rec); !regexp.MustCompile(`^api-[0-9a-f]{32}$`).MatchString(id) || gotPath != "/id" || rec.Header().Get("X-Request-ID") != id {
		t.Errorf("custom generator: %q path=%q", id, gotPath)
	}

	if id, _, _ := seen(t, do(api, "GET", "/id", "X-Request-ID", "from-client")); id != "from-client" {
		t.Errorf("a valid incoming id wins over the generator: %q", id)
	}

	n := 0
	counter := idAPI(middleware.RequestID(middleware.RequestIDConfig{
		Generate:       func(*http.Request) string { n++; return "req-" + string(rune('0'+n)) },
		IgnoreIncoming: true,
	}))
	for _, want := range []string{"req-1", "req-2"} {
		if id, _, _ := seen(t, do(counter, "GET", "/id", "X-Request-ID", "ignored")); id != want {
			t.Errorf("sequential generator: got %q, want %q", id, want)
		}
	}

	empty := idAPI(middleware.RequestID(middleware.RequestIDConfig{Generate: func(*http.Request) string { return "" }}))
	if id, _, _ := seen(t, do(empty, "GET", "/id")); !hexID.MatchString(id) {
		t.Errorf("a generator that returns nothing falls back to the default, got %q", id)
	}
}

func TestRequestIDCustomHeader(t *testing.T) {
	api := idAPI(middleware.RequestID(middleware.RequestIDConfig{Header: "X-Correlation-ID"}))
	rec := do(api, "GET", "/id", "X-Correlation-ID", "corr-1", "X-Request-ID", "ignored-header")
	id, reqHeader, other := seen(t, rec)
	if id != "corr-1" || other != "corr-1" || reqHeader != "ignored-header" || rec.Header().Get("X-Correlation-ID") != "corr-1" || rec.Header().Get("X-Request-ID") != "" {
		t.Errorf("custom header: ctx=%q other=%q default-header=%q resp=%v", id, other, reqHeader, rec.Header())
	}
}

func lineWith(log, needle string) string {
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, needle) {
			return l
		}
	}
	return ""
}

func TestRequestIDInLogs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(middleware.SlogContext(slog.NewTextHandler(&buf, nil)))
	prev := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })

	api := server.New("t", "1")
	api.Use(middleware.RequestID(middleware.RequestIDConfig{}), middleware.Logger(logger))
	server.Get(api, "/fail", func(ctx context.Context, _ *empty) (*empty, error) { return nil, errors.New("boom") })
	server.Get(api, "/panic", func(ctx context.Context, _ *empty) (*empty, error) { panic("kaboom") })
	server.Get(api, "/note", func(ctx context.Context, _ *empty) (*empty, error) {
		logger.InfoContext(ctx, "from the handler")
		return nil, nil
	})

	for path, id := range map[string]string{"/fail": "req-fail", "/panic": "req-panic", "/note": "req-note"} {
		if rec := do(api, "GET", path, "X-Request-ID", id); rec.Header().Get("X-Request-ID") != id {
			t.Fatalf("%s: %v", path, rec.Header())
		}
	}
	log := buf.String()
	for needle, id := range map[string]string{"unhandled error": "req-fail", "minato: panic": "req-panic", "from the handler": "req-note"} {
		if line := lineWith(log, needle); !strings.Contains(line, "request_id="+id) {
			t.Errorf("%q line lacks request_id=%s:\n%s", needle, id, log)
		}
	}
	line := lineWith(log, "path=/fail")
	if strings.Count(line, "request_id=") != 1 || !strings.Contains(line, "request_id=req-fail") {
		t.Errorf("the request line must carry the id exactly once even through SlogContext: %s", line)
	}
}

func TestRequestIDLoggerWithoutWrapper(t *testing.T) {
	var buf bytes.Buffer
	plain := slog.New(slog.NewTextHandler(&buf, nil))

	api := server.New("t", "1")
	api.Use(middleware.RequestID(middleware.RequestIDConfig{}), middleware.Logger(plain))
	server.Get(api, "/x", okHandler)
	do(api, "GET", "/x", "X-Request-ID", "abc")
	if !strings.Contains(buf.String(), "request_id=abc") {
		t.Errorf("Logger adds the id itself, wrapper or not: %s", buf.String())
	}

	buf.Reset()
	bare := server.New("t", "1")
	bare.Use(middleware.Logger(slog.New(middleware.SlogContext(slog.NewTextHandler(&buf, nil)))))
	server.Get(bare, "/x", okHandler)
	do(bare, "GET", "/x")
	if strings.Contains(buf.String(), "request_id") {
		t.Errorf("no id, no attribute: %s", buf.String())
	}
}
