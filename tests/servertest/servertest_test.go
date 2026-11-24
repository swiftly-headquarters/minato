package servertest_test

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/swiftly-headquarters/minato/server"
	"github.com/swiftly-headquarters/minato/servertest"
)

type user struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type createIn struct {
	Body struct {
		Name string `json:"name" minLength:"2"`
	}
}

type userOut struct{ Body user }

func newAPI() *server.API {
	api := server.New("t", "1")
	server.Post(api, "/users", func(ctx context.Context, in *createIn) (*userOut, error) {
		return &userOut{user{1, in.Body.Name}}, nil
	}, func(o *server.Operation) { o.DefaultStatus = 201 })
	server.Get(api, "/users/{id}", func(ctx context.Context, in *struct {
		ID int `path:"id"`
	}) (*userOut, error) {
		if in.ID != 1 {
			return nil, server.Error404("user not found")
		}
		return &userOut{user{1, "Ada"}}, nil
	})
	server.Get(api, "/whoami", func(ctx context.Context, in *struct {
		Token string `header:"X-Token"`
	}) (*struct{ Body string }, error) {
		return &struct{ Body string }{in.Token}, nil
	})
	return api
}

type fakeTB struct {
	testing.TB
	msg string
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.msg = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func failure(fn func(tb testing.TB)) string {
	tb := &fakeTB{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(tb)
	}()
	<-done
	return tb.msg
}

func TestRoundTrip(t *testing.T) {
	c := servertest.New(t, newAPI())

	created := servertest.Decode[user](c.Post("/users", map[string]string{"name": "Ada"}).Status(201))
	if created != (user{1, "Ada"}) {
		t.Fatalf("created: %+v", created)
	}
	if got := servertest.Decode[user](c.Get("/users/1").Status(200)); got.Name != "Ada" {
		t.Fatalf("fetched: %+v", got)
	}
	c.Get("/users/2").Status(404)
	c.Post("/users", map[string]string{"name": "A"}).Status(422)

	for name, body := range map[string]any{
		"string": `{"name":"Bo"}`, "bytes": []byte(`{"name":"Bo"}`), "reader": strings.NewReader(`{"name":"Bo"}`),
	} {
		if got := servertest.Decode[user](c.Post("/users", body).Status(201)); got.Name != "Bo" {
			t.Errorf("%s body: %+v", name, got)
		}
	}
	if c.Do("PATCH", "/users/1", nil).Code != 405 {
		t.Error("Do must pass the method through")
	}
}

func TestHeaders(t *testing.T) {
	c := servertest.New(t, newAPI())
	whoami := func(hdr ...string) string {
		t.Helper()
		return servertest.Decode[string](c.Get("/whoami", hdr...).Status(200))
	}
	if whoami() != "" {
		t.Fatal("no header was set yet")
	}
	c.Header.Set("X-Token", "default")
	if got := whoami(); got != "default" {
		t.Errorf("client header: %q", got)
	}
	if got := whoami("X-Token", "override"); got != "override" {
		t.Errorf("per-call header must win: %q", got)
	}
	if got := whoami(); got != "default" || c.Header.Get("X-Token") != "default" {
		t.Errorf("a per-call header leaked into later requests: %q", got)
	}

	var seen string
	api := server.New("t", "1")
	server.Post(api, "/echo", func(ctx context.Context, in *struct {
		Type string `header:"Content-Type"`
	}) (*empty, error) {
		seen = in.Type
		return nil, nil
	})
	c = servertest.New(t, api)
	c.Post("/echo", map[string]int{"a": 1})
	if seen != "application/json" {
		t.Errorf("a struct body is sent as JSON, got %q", seen)
	}
	c.Post("/echo", "a=1", "Content-Type", "application/x-www-form-urlencoded")
	if seen != "application/x-www-form-urlencoded" {
		t.Errorf("an explicit content type wins, got %q", seen)
	}
}

type empty = struct{}

func TestFailuresAreReportedThroughT(t *testing.T) {
	c := func(tb testing.TB) *servertest.Client { return servertest.New(tb, newAPI()) }

	for name, tc := range map[string]struct {
		run  func(tb testing.TB)
		want []string
	}{
		"status mismatch": {func(tb testing.TB) { c(tb).Get("/users/2").Status(200) }, []string{"status 404, want 200", "user not found"}},
		"bad decode":      {func(tb testing.TB) { servertest.Decode[[]int](c(tb).Get("/users/1")) }, []string{"decoding", "[]int"}},
		"odd headers":     {func(tb testing.TB) { c(tb).Get("/users/1", "X-Only") }, []string{"key, value pairs"}},
		"unencodable":     {func(tb testing.TB) { c(tb).Post("/users", make(chan int)) }, []string{"encoding request body"}},
	} {
		msg := failure(tc.run)
		for _, w := range tc.want {
			if !strings.Contains(msg, w) {
				t.Errorf("%s: message %q lacks %q", name, msg, w)
			}
		}
	}
}
