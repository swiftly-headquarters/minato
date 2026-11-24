package middleware

import (
	"net/http"
	"strconv"
	"time"
)

type SecureHeadersConfig struct {
	FrameOptions          string
	ReferrerPolicy        string
	ContentSecurityPolicy string
	HSTSMaxAge            time.Duration
	HSTSIncludeSubdomains bool
}

func SecureHeaders(c SecureHeadersConfig) func(http.Handler) http.Handler {
	frame := c.FrameOptions
	if frame == "" {
		frame = "DENY"
	}
	referrer := c.ReferrerPolicy
	if referrer == "" {
		referrer = "no-referrer"
	}
	var hsts string
	if c.HSTSMaxAge > 0 {
		hsts = "max-age=" + strconv.FormatInt(int64(c.HSTSMaxAge.Seconds()), 10)
		if c.HSTSIncludeSubdomains {
			hsts += "; includeSubDomains"
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", frame)
			h.Set("Referrer-Policy", referrer)
			if c.ContentSecurityPolicy != "" {
				h.Set("Content-Security-Policy", c.ContentSecurityPolicy)
			}
			if hsts != "" {
				h.Set("Strict-Transport-Security", hsts)
			}
			next.ServeHTTP(w, r)
		})
	}
}
