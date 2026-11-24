package middleware_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/swiftly-headquarters/minato/middleware"
	"github.com/swiftly-headquarters/minato/server"
)

type userKey struct{}

func authAPI(verify func(context.Context, string) (context.Context, error)) (*server.API, *atomic.Int32) {
	var hits atomic.Int32
	api := server.New("t", "1")
	g := api.Group("/p", middleware.Bearer(verify))
	server.Get(g, "/who", func(ctx context.Context, _ *empty) (*struct{ Body string }, error) {
		hits.Add(1)
		user, _ := ctx.Value(userKey{}).(string)
		return &struct{ Body string }{user}, nil
	})
	server.Get(api, "/open", okHandler)
	return api, &hits
}

func TestBearer(t *testing.T) {
	var verified []string
	api, hits := authAPI(func(ctx context.Context, token string) (context.Context, error) {
		verified = append(verified, token)
		if token == "good" {
			return context.WithValue(ctx, userKey{}, "ada"), nil
		}
		return ctx, errors.New("lookup failed: secret db detail")
	})

	rec := do(api, "GET", "/p/who", "Authorization", "Bearer good")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `"ada"` || hits.Load() != 1 {
		t.Fatalf("valid token: %d %s", rec.Code, rec.Body)
	}
	if rec := do(api, "GET", "/p/who", "Authorization", "bearer good"); rec.Code != 200 {
		t.Errorf("the scheme is case-insensitive: %d", rec.Code)
	}

	hits.Store(0)
	verified = nil
	for name, h := range map[string][]string{
		"no header": nil, "wrong scheme": {"Authorization", "Basic Z29vZA=="}, "scheme only": {"Authorization", "Bearer"},
		"empty token": {"Authorization", "Bearer   "},
	} {
		rec := do(api, "GET", "/p/who", h...)
		if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "Bearer" || rec.Header().Get("Content-Type") != "application/problem+json" {
			t.Errorf("%s: %d %v %s", name, rec.Code, rec.Header(), rec.Body)
		}
	}
	if len(verified) != 0 || hits.Load() != 0 {
		t.Errorf("verify and the handler must not run without a token: %v %d", verified, hits.Load())
	}

	rec = do(api, "GET", "/p/who", "Authorization", "Bearer bad")
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != `Bearer error="invalid_token"` || strings.Contains(rec.Body.String(), "secret") || hits.Load() != 0 {
		t.Errorf("invalid token: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if rec := do(api, "GET", "/open"); rec.Code != 204 {
		t.Errorf("routes outside the group stay open: %d", rec.Code)
	}
}

func TestBearerVerifyChoosesTheResponse(t *testing.T) {
	api, _ := authAPI(func(ctx context.Context, token string) (context.Context, error) {
		switch token {
		case "forbidden":
			return nil, server.Error403("token lacks the admin role")
		case "expired":
			return nil, server.Error401("token expired")
		case "down":
			return nil, server.NewError(http.StatusServiceUnavailable, "auth backend down")
		}
		return nil, nil
	})
	for token, want := range map[string]struct {
		code      int
		challenge string
		detail    string
	}{
		"forbidden": {403, "", "admin role"},
		"expired":   {401, `Bearer error="invalid_token"`, "token expired"},
		"down":      {503, "", "backend down"},
	} {
		rec := do(api, "GET", "/p/who", "Authorization", "Bearer "+token)
		if rec.Code != want.code || rec.Header().Get("WWW-Authenticate") != want.challenge || !strings.Contains(rec.Body.String(), want.detail) {
			t.Errorf("%s: %d %q %s", token, rec.Code, rec.Header().Get("WWW-Authenticate"), rec.Body)
		}
	}
	if rec := do(api, "GET", "/p/who", "Authorization", "Bearer anything"); rec.Code != 200 {
		t.Errorf("a nil context from verify keeps the request context: %d %s", rec.Code, rec.Body)
	}
}

func TestBearerNeedsVerify(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Bearer(nil) must panic at setup")
		}
	}()
	middleware.Bearer(nil)
}
