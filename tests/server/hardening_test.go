package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/swiftly-headquarters/minato/server"
)

type ctxUser struct{}

func keyScheme(api *server.API) {
	api.AddScheme("key", server.Scheme{
		Spec:      map[string]any{"type": "apiKey", "in": "header", "name": "X-Key"},
		Challenge: `ApiKey realm="test"`,
		Verify: func(ctx context.Context, r *http.Request, scopes []string) (context.Context, error) {
			if r.Header.Get("X-Key") != "k1" {
				return nil, fmt.Errorf("bad key")
			}
			if slicesContain(scopes, "admin") && r.Header.Get("X-Role") != "admin" {
				return nil, server.Error403("admin scope required")
			}
			return context.WithValue(ctx, ctxUser{}, "ada"), nil
		},
	})
}

func slicesContain(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func secureAPI() *server.API {
	api := server.New("t", "1")
	keyScheme(api)
	api.BearerAuth("docs-only")
	g := api.Group("/v1", func(next http.Handler) http.Handler { return next })
	g.Security = server.Secured("key")
	server.Get(g, "/me", func(ctx context.Context, _ *empty) (*struct{ Body string }, error) {
		u, _ := ctx.Value(ctxUser{}).(string)
		return &struct{ Body string }{u}, nil
	})
	server.Get(api, "/admin", func(ctx context.Context, _ *empty) (*empty, error) { return nil, nil },
		func(o *server.Operation) { o.Security = server.Secured("key", "admin") })
	server.Get(api, "/either", func(ctx context.Context, _ *empty) (*empty, error) { return nil, nil },
		func(o *server.Operation) {
			o.Security = []map[string][]string{{"docs-only": nil}, {"key": nil}}
		})
	server.Get(api, "/open-docs-only", func(ctx context.Context, _ *empty) (*empty, error) { return nil, nil },
		func(o *server.Operation) { o.Security = server.Secured("docs-only") })
	return api
}

func TestSchemeEnforcesOnRegisteredRoutes(t *testing.T) {
	api := secureAPI()
	rec := call(api, "GET", "/v1/me", "")
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != `ApiKey realm="test"` {
		t.Fatalf("no key: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	rec = call(api, "GET", "/v1/me", "", "X-Key", "wrong")
	if rec.Code != 401 || strings.Contains(rec.Body.String(), "bad key") {
		t.Fatalf("wrong key must be a generic 401: %d %s", rec.Code, rec.Body)
	}
	rec = call(api, "GET", "/v1/me", "", "X-Key", "k1")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `"ada"` {
		t.Fatalf("valid key reaches the handler with the verified context: %d %s", rec.Code, rec.Body)
	}
}

func TestSchemeScopesAndForbidden(t *testing.T) {
	api := secureAPI()
	rec := call(api, "GET", "/admin", "", "X-Key", "k1")
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "admin scope required") {
		t.Fatalf("valid key without the scope: %d %s", rec.Code, rec.Body)
	}
	if rec := call(api, "GET", "/admin", "", "X-Key", "k1", "X-Role", "admin"); rec.Code != 204 {
		t.Fatalf("with the scope: %d", rec.Code)
	}
	if rec := call(api, "GET", "/admin", ""); rec.Code != 401 {
		t.Fatalf("no key on a scoped route: %d", rec.Code)
	}
}

func TestSchemeAlternativesAndDocOnlySchemes(t *testing.T) {
	api := secureAPI()
	if rec := call(api, "GET", "/either", ""); rec.Code != 204 {
		t.Fatalf("a documentation-only alternative passes without enforcement: %d", rec.Code)
	}
	if rec := call(api, "GET", "/open-docs-only", ""); rec.Code != 204 {
		t.Fatalf("a documentation-only scheme does not enforce: %d", rec.Code)
	}
	if rec := call(api, "GET", "/v1/me", "", "X-Key", "k1"); rec.Code != 200 {
		t.Fatalf("setup: %d", rec.Code)
	}
}

func TestSecuredRoutesDocument401(t *testing.T) {
	api := secureAPI()
	spec := specJSON(t, api)
	if r := dig(spec, "paths", "/v1/me", "get", "responses", "401"); r == nil {
		t.Fatal("a secured route must document 401")
	}
	if r := dig(spec, "paths", "/open-docs-only", "get", "responses", "401"); r == nil {
		t.Fatal("any security requirement documents 401")
	}
}

func TestUnknownSchemeIsAStartupPanic(t *testing.T) {
	api := server.New("t", "1")
	defer func() {
		if recover() == nil {
			t.Fatal("a typo in a security scheme name must panic at registration")
		}
	}()
	server.Get(api, "/x", noop, func(o *server.Operation) { o.Security = server.Secured("typo") })
}

func TestBearerTokenHelper(t *testing.T) {
	for header, want := range map[string]struct {
		tok string
		ok  bool
	}{"Bearer abc": {"abc", true}, "bearer   xyz  ": {"xyz", true}, "Bearer ": {"", false}, "Basic abc": {"", false}, "": {"", false}} {
		r := httptest.NewRequest("GET", "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		tok, ok := server.BearerToken(r)
		if tok != want.tok || ok != want.ok {
			t.Errorf("%q: got %q %v", header, tok, ok)
		}
	}
}

func TestReadHeaderTimeoutDropsSlowClients(t *testing.T) {
	api := server.New("t", "1")
	api.ReadHeaderTimeout = 200 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- api.Serve(ctx, ln) }()
	defer func() {
		cancel()
		<-done
	}()

	nc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	nc.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n"))
	nc.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	_, err = io.ReadAll(nc)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("the server never closed a client that stalled its headers")
	}
	if time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("closed after %v, expected about the header timeout", time.Since(start))
	}
}

func TestDefaultHTTPTimeouts(t *testing.T) {
	api := server.New("t", "1")
	if api.ReadHeaderTimeout != 10*time.Second || api.ReadTimeout != time.Minute || api.IdleTimeout != 2*time.Minute || api.WriteTimeout != 0 {
		t.Fatalf("defaults: header=%v read=%v idle=%v write=%v", api.ReadHeaderTimeout, api.ReadTimeout, api.IdleTimeout, api.WriteTimeout)
	}
	if api.Draining() {
		t.Fatal("a new API is not draining")
	}
}

func TestPaginateAndCursor(t *testing.T) {
	type row struct{ ID int }
	rows := []row{{1}, {2}, {3}}
	page := server.Paginate(rows, 2, func(r row) any { return r.ID })
	if len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("a full page: %+v", page)
	}
	var last int
	if err := server.DecodeCursor(page.NextCursor, &last); err != nil || last != 2 {
		t.Fatalf("the cursor names the last item returned: %d %v", last, err)
	}
	exact := server.Paginate([]row{{1}, {2}}, 2, func(r row) any { return r.ID })
	if exact.NextCursor != "" || len(exact.Items) != 2 {
		t.Fatalf("a page that exactly fills the limit has no next page: %+v", exact)
	}
	final := server.Paginate([]row{{3}}, 2, func(r row) any { return r.ID })
	if final.NextCursor != "" || final.Items[0].ID != 3 {
		t.Fatalf("the last page has no cursor: %+v", final)
	}
	empty := server.Paginate[row](nil, 2, func(r row) any { return r.ID })
	b, _ := json.Marshal(empty)
	if string(b) != `{"items":[]}` {
		t.Fatalf("an empty page encodes as an empty array: %s", b)
	}
	if err := server.DecodeCursor("!!!", &last); err == nil {
		t.Fatal("garbage is not a cursor")
	}
}

type pageItem struct {
	ID int `json:"id"`
}

func TestPagedRouteWithCursorValidation(t *testing.T) {
	api := server.New("t", "1")
	all := []pageItem{{1}, {2}, {3}, {4}, {5}}
	server.Get(api, "/items", func(ctx context.Context, in *struct {
		server.Page
	}) (*struct{ Body server.PageBody[pageItem] }, error) {
		start := 0
		if in.Cursor != "" {
			var after int
			if err := server.DecodeCursor(in.Cursor, &after); err != nil {
				return nil, server.InvalidCursor(err)
			}
			start = after
		}
		var rest []pageItem
		for _, it := range all {
			if it.ID > start {
				rest = append(rest, it)
			}
		}
		if len(rest) > in.Limit+1 {
			rest = rest[:in.Limit+1]
		}
		out := &struct{ Body server.PageBody[pageItem] }{}
		out.Body = server.Paginate(rest, in.Limit, func(p pageItem) any { return p.ID })
		return out, nil
	})

	var seen []int
	cursor := ""
	for range 5 {
		url := "/items?limit=2"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		rec := call(api, "GET", url, "")
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", url, rec.Code, rec.Body)
		}
		var body server.PageBody[pageItem]
		json.Unmarshal(rec.Body.Bytes(), &body)
		for _, it := range body.Items {
			seen = append(seen, it.ID)
		}
		if body.NextCursor == "" {
			break
		}
		cursor = body.NextCursor
	}
	if fmt.Sprint(seen) != "[1 2 3 4 5]" {
		t.Fatalf("walking every page returns every item once: %v", seen)
	}

	rec := call(api, "GET", "/items?cursor=%25%25", "")
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"query.cursor"`) {
		t.Fatalf("a bad cursor is a 422 on query.cursor: %d %s", rec.Code, rec.Body)
	}
	if rec := call(api, "GET", "/items?limit=500", ""); rec.Code != 422 {
		t.Fatalf("the limit maximum still applies: %d", rec.Code)
	}

	spec := specJSON(t, api)
	if name := dig(spec, "paths", "/items", "get", "responses", "200", "content", "application/json", "schema", "$ref"); name != "#/components/schemas/PageBodypageItem" {
		t.Fatalf("generic response schema name: %v", name)
	}
	if dig(spec, "components", "schemas", "PageBodypageItem") == nil {
		t.Fatal("the generic instance is a named component")
	}
}
