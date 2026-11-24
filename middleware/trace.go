package middleware

import (
	"context"
	"net/http"

	"github.com/swiftly-headquarters/minato/server"
)

type Tracer interface {
	Start(r *http.Request) (context.Context, Span)
}

type Span interface {
	End(SpanResult)
}

type SpanResult struct {
	Route  string
	Status int
}

type TracerFunc func(r *http.Request) (context.Context, Span)

func (f TracerFunc) Start(r *http.Request) (context.Context, Span) { return f(r) }

func Trace(t Tracer) func(http.Handler) http.Handler {
	if t == nil {
		panic("minato: Trace needs a Tracer")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, span := t.Start(r)
			if ctx == nil {
				ctx = r.Context()
			}
			sw := &statusWriter{ResponseWriter: w}
			completed := false
			r, route := server.CaptureRoute(r.WithContext(ctx))
			defer func() {
				if span == nil {
					return
				}
				status := sw.status
				if !completed && status == 0 {
					status = http.StatusInternalServerError
				}
				if status == 0 {
					status = http.StatusOK
				}
				span.End(SpanResult{Route: routeOrUnrouted(route()), Status: status})
			}()
			next.ServeHTTP(sw, r)
			completed = true
		})
	}
}

func routeOrUnrouted(pattern string) string {
	if pattern == "" {
		return "unrouted"
	}
	return pattern
}
