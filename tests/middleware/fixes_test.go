package middleware_test

import (
	"context"
	"math"
	"net/http"
	"testing"

	"github.com/swiftly-headquarters/minato/middleware"
	"github.com/swiftly-headquarters/minato/server"
)

func TestBearerTypedNilErrorIsA401(t *testing.T) {
	h := middleware.Bearer(func(ctx context.Context, tok string) (context.Context, error) {
		var em *server.ErrorModel
		return nil, em
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if rec := do(h, "GET", "/", "Authorization", "Bearer x"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a typed-nil error must not crash the middleware: %d", rec.Code)
	}
}

func TestRateLimitRejectsAnInfiniteRate(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("an infinite rate must panic at setup")
		}
	}()
	middleware.RateLimit(middleware.RateLimitConfig{Rate: math.Inf(1)})
}
