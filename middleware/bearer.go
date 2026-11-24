package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/swiftly-headquarters/minato/server"
)

func Bearer(verify func(ctx context.Context, token string) (context.Context, error)) func(http.Handler) http.Handler {
	if verify == nil {
		panic("minato: Bearer needs a verify function")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
			token = strings.TrimSpace(token)
			if !strings.EqualFold(scheme, "Bearer") || token == "" {
				w.Header().Set("WWW-Authenticate", "Bearer")
				server.WriteError(w, server.Error401("missing bearer token"))
				return
			}
			ctx, err := verify(r.Context(), token)
			if err != nil {
				var em *server.ErrorModel
				if !errors.As(err, &em) {
					em = server.NewError(http.StatusUnauthorized, "invalid bearer token")
				}
				if em.Status == http.StatusUnauthorized {
					w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				}
				server.WriteError(w, em)
				return
			}
			if ctx == nil {
				ctx = r.Context()
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
