package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
)

type requestIDKey struct{}

type RequestIDConfig struct {
	Header         string
	Generate       func(r *http.Request) string
	IgnoreIncoming bool
}

func NewRequestID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func RequestID(c RequestIDConfig) func(http.Handler) http.Handler {
	if c.Header == "" {
		c.Header = "X-Request-ID"
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := ""
			if in := r.Header.Get(c.Header); !c.IgnoreIncoming && validRequestID(in) {
				id = in
			}
			if id == "" && c.Generate != nil {
				id = c.Generate(r)
			}
			if id == "" {
				id = NewRequestID()
			}
			w.Header().Set(c.Header, id)
			r.Header.Set(c.Header, id)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
		})
	}
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] <= ' ' || id[i] > '~' {
			return false
		}
	}
	return true
}

type contextHandler struct{ slog.Handler }

func SlogContext(h slog.Handler) slog.Handler { return contextHandler{h} }

func (h contextHandler) Handle(ctx context.Context, rec slog.Record) error {
	if id := RequestIDFromContext(ctx); id != "" {
		seen := false
		rec.Attrs(func(a slog.Attr) bool {
			seen = a.Key == "request_id"
			return !seen
		})
		if !seen {
			rec.AddAttrs(slog.String("request_id", id))
		}
	}
	return h.Handler.Handle(ctx, rec)
}

func (h contextHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(a)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}
