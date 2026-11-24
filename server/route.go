package server

import (
	"context"
	"net/http"
)

type routeKey struct{}

type routeCapture struct{ pattern string }

func CaptureRoute(r *http.Request) (*http.Request, func() string) {
	rc := &routeCapture{}
	return r.WithContext(context.WithValue(r.Context(), routeKey{}, rc)), func() string { return rc.pattern }
}

func routed(label string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rc, _ := r.Context().Value(routeKey{}).(*routeCapture); rc != nil {
			rc.pattern = label
		}
		next.ServeHTTP(w, r)
	})
}
