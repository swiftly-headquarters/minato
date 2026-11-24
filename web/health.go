package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

type Check func(ctx context.Context) error

type health struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

func writeHealth(w http.ResponseWriter, code int, h health) {
	b, _ := json.Marshal(h)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	w.Write(b)
}

func Live() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHealth(w, http.StatusOK, health{Status: "ok"})
	})
}

func Ready(timeout time.Duration, checks map[string]Check) http.Handler {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		type result struct{ name, state string }
		results := make(chan result, len(checks))
		for name, check := range checks {
			go func() {
				state := "ok"
				defer func() {
					if v := recover(); v != nil {
						slog.ErrorContext(ctx, "minato: readiness check panicked", "check", name, "panic", v)
						state = "failed"
					}
					results <- result{name, state}
				}()
				if err := check(ctx); err != nil {
					slog.WarnContext(ctx, "minato: readiness check failed", "check", name, "err", err)
					state = "failed"
				}
			}()
		}
		out := health{Status: "ok"}
		if len(checks) > 0 {
			out.Checks = make(map[string]string, len(checks))
		}
		for len(out.Checks) < len(checks) {
			select {
			case res := <-results:
				out.Checks[res.name] = res.state
			case <-ctx.Done():
				for name := range checks {
					if _, done := out.Checks[name]; !done {
						out.Checks[name] = "timeout"
					}
				}
			}
		}
		code := http.StatusOK
		for _, state := range out.Checks {
			if state != "ok" {
				out.Status, code = "unavailable", http.StatusServiceUnavailable
			}
		}
		writeHealth(w, code, out)
	})
}
