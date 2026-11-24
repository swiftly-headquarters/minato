package server_test

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/swiftly-headquarters/minato/server"
)

type etagBody struct {
	N    int    `json:"n" xml:"n"`
	Name string `json:"name" xml:"name"`
}

func etagAPI(enabled bool) (*server.API, *int) {
	api := server.New("t", "1")
	api.ETag = enabled
	api.Formats["application/xml"] = xml.Marshal
	n := 1
	server.Get(api, "/thing", func(ctx context.Context, _ *empty) (*struct{ Body etagBody }, error) {
		return &struct{ Body etagBody }{etagBody{n, "x"}}, nil
	})
	server.Post(api, "/thing", func(ctx context.Context, _ *empty) (*struct{ Body etagBody }, error) {
		return &struct{ Body etagBody }{etagBody{n, "x"}}, nil
	})
	server.Get(api, "/created", func(ctx context.Context, _ *empty) (*struct {
		Status int
		Body   etagBody
	}, error) {
		return &struct {
			Status int
			Body   etagBody
		}{Status: 201, Body: etagBody{1, "x"}}, nil
	})
	server.Get(api, "/none", noop)
	server.Get(api, "/missing", func(ctx context.Context, _ *empty) (*struct{ Body etagBody }, error) {
		return nil, server.Error404("nope")
	})
	server.Get(api, "/manual", func(ctx context.Context, _ *empty) (*struct {
		ETag string `header:"ETag"`
		Body etagBody
	}, error) {
		return &struct {
			ETag string `header:"ETag"`
			Body etagBody
		}{`"v7"`, etagBody{7, "m"}}, nil
	})
	return api, &n
}

func get(api http.Handler, method, url string, hdr ...string) *httptest.ResponseRecorder {
	return call(api, method, url, "", hdr...)
}

func TestETag(t *testing.T) {
	api, n := etagAPI(true)

	first := get(api, "GET", "/thing")
	tag := first.Header().Get("ETag")
	if first.Code != 200 || !regexp.MustCompile(`^W/"[0-9a-f]{32}"$`).MatchString(tag) || first.Body.Len() == 0 {
		t.Fatalf("first response: %d %q %s", first.Code, tag, first.Body)
	}
	if get(api, "GET", "/thing").Header().Get("ETag") != tag {
		t.Error("the same body must give the same tag")
	}

	for name, header := range map[string]string{
		"exact": tag, "strong form": strings.TrimPrefix(tag, "W/"), "star": "*", "in a list": `"other", ` + tag + `, "x"`,
	} {
		rec := get(api, "GET", "/thing", "If-None-Match", header)
		if rec.Code != 304 || rec.Body.Len() != 0 || rec.Header().Get("ETag") != tag {
			t.Errorf("%s: %d %q %v", name, rec.Code, rec.Body, rec.Header())
		}
	}
	for name, header := range map[string]string{"stale": `W/"0000"`, "empty": "", "garbage": "nonsense"} {
		if rec := get(api, "GET", "/thing", "If-None-Match", header); rec.Code != 200 || rec.Body.Len() == 0 {
			t.Errorf("%s must not match: %d", name, rec.Code)
		}
	}
	if rec := get(api, "HEAD", "/thing", "If-None-Match", tag); rec.Code != 304 {
		t.Errorf("HEAD revalidation: %d", rec.Code)
	}

	*n = 2
	changed := get(api, "GET", "/thing", "If-None-Match", tag)
	if changed.Code != 200 || changed.Header().Get("ETag") == tag {
		t.Errorf("a changed body must get a new tag and a full response: %d %q", changed.Code, changed.Header().Get("ETag"))
	}

	xmlRec := get(api, "GET", "/thing", "Accept", "application/xml")
	if xmlRec.Header().Get("ETag") == changed.Header().Get("ETag") || xmlRec.Header().Get("ETag") == "" {
		t.Error("each representation needs its own tag")
	}
}

func TestETagOnlyWhereItMakesSense(t *testing.T) {
	api, _ := etagAPI(true)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"POST":        get(api, "POST", "/thing", "If-None-Match", "*"),
		"201":         get(api, "GET", "/created", "If-None-Match", "*"),
		"no body":     get(api, "GET", "/none", "If-None-Match", "*"),
		"error":       get(api, "GET", "/missing", "If-None-Match", "*"),
		"docs":        get(api, "GET", "/docs"),
		"unknown url": get(api, "GET", "/nowhere", "If-None-Match", "*"),
	} {
		if rec.Code == 304 || rec.Header().Get("ETag") != "" {
			t.Errorf("%s: %d %q", name, rec.Code, rec.Header().Get("ETag"))
		}
	}
	if rec := get(api, "POST", "/thing", "If-None-Match", "*"); rec.Code != 200 {
		t.Errorf("a POST with If-None-Match is not a revalidation: %d", rec.Code)
	}
}

func TestETagOffAndManual(t *testing.T) {
	off, _ := etagAPI(false)
	if rec := get(off, "GET", "/thing", "If-None-Match", "*"); rec.Code != 200 || rec.Header().Get("ETag") != "" {
		t.Errorf("api.ETag is off by default: %d %q", rec.Code, rec.Header().Get("ETag"))
	}

	for _, api := range []*server.API{off, func() *server.API { a, _ := etagAPI(true); return a }()} {
		rec := get(api, "GET", "/manual")
		if rec.Code != 200 || rec.Header().Get("ETag") != `"v7"` || rec.Body.Len() == 0 {
			t.Fatalf("a handler-chosen tag is kept as is: %d %q", rec.Code, rec.Header().Get("ETag"))
		}
		if rec := get(api, "GET", "/manual", "If-None-Match", `"v7"`); rec.Code != 304 || rec.Body.Len() != 0 {
			t.Errorf("and honoured, even with api.ETag off: %d %q", rec.Code, rec.Body)
		}
		if rec := get(api, "GET", "/manual", "If-None-Match", `"v6"`); rec.Code != 200 {
			t.Errorf("another version: %d", rec.Code)
		}
	}

	api, _ := etagAPI(false)
	api.ETag = true
	if get(api, "GET", "/thing").Header().Get("ETag") == "" {
		t.Error("api.ETag can be switched on after registration")
	}
}

func TestCaptureRoute(t *testing.T) {
	api := server.New("t", "1")
	server.Get(api, "/things/{id}", func(ctx context.Context, in *struct {
		ID int `path:"id"`
	}) (*empty, error) {
		return nil, nil
	})
	server.Get(api, "/", noop)
	g := api.Group("/v1", func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { server.WriteError(w, server.Error401("no")) })
	})
	server.Get(g, "/secret", noop)
	server.Handle(api, "/files/", http.NotFoundHandler())
	server.Handle(g, "GET /static/", http.NotFoundHandler())

	for url, want := range map[string]string{
		"/things/42":     "GET /things/{id}",
		"/":              "GET /",
		"/v1/secret":     "GET /v1/secret",
		"/files/a/b.txt": "/files/",
		"/v1/static/x":   "GET /v1/static/",
		"/nowhere":       "",
		"/things/abc":    "GET /things/{id}",
	} {
		req, route := server.CaptureRoute(httptest.NewRequest("GET", url, nil))
		api.ServeHTTP(httptest.NewRecorder(), req)
		if got := route(); got != want {
			t.Errorf("%s: route %q, want %q", url, got, want)
		}
	}

	req, route := server.CaptureRoute(httptest.NewRequest("GET", "/things/1", nil))
	api.ServeHTTP(httptest.NewRecorder(), req.WithContext(context.WithValue(req.Context(), struct{}{}, 1)))
	if route() != "GET /things/{id}" {
		t.Error("the capture must survive a middleware that copies the request")
	}
}
