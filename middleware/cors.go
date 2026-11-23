package middleware

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

type CORSConfig struct {
	AllowOrigins     []string
	AllowMethods     []string
	AllowHeaders     []string
	ExposeHeaders    []string
	AllowCredentials bool
	MaxAge           time.Duration
}

func CORS(c CORSConfig) func(http.Handler) http.Handler {
	if len(c.AllowOrigins) == 0 && !c.AllowCredentials {
		c.AllowOrigins = []string{"*"}
	}
	wildcard := slices.Contains(c.AllowOrigins, "*")
	if c.AllowCredentials && (wildcard || len(c.AllowOrigins) == 0) {
		panic("minato: CORS AllowCredentials requires explicit AllowOrigins, not \"*\"")
	}
	if len(c.AllowMethods) == 0 {
		c.AllowMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"}
	}
	methods, headers, expose := strings.Join(c.AllowMethods, ", "), strings.Join(c.AllowHeaders, ", "), strings.Join(c.ExposeHeaders, ", ")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			if !wildcard {
				h.Add("Vary", "Origin")
			}
			if !wildcard && !slices.Contains(c.AllowOrigins, origin) {
				next.ServeHTTP(w, r)
				return
			}
			if wildcard {
				h.Set("Access-Control-Allow-Origin", "*")
			} else {
				h.Set("Access-Control-Allow-Origin", origin)
			}
			if c.AllowCredentials {
				h.Set("Access-Control-Allow-Credentials", "true")
			}
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", methods)
				if headers != "" {
					h.Set("Access-Control-Allow-Headers", headers)
				} else if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
					h.Set("Access-Control-Allow-Headers", req)
					h.Add("Vary", "Access-Control-Request-Headers")
				}
				if c.MaxAge > 0 {
					h.Set("Access-Control-Max-Age", strconv.Itoa(int(c.MaxAge.Seconds())))
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if expose != "" {
				h.Set("Access-Control-Expose-Headers", expose)
			}
			next.ServeHTTP(w, r)
		})
	}
}
