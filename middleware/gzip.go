package middleware

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
)

func Gzip() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !acceptsGzip(r.Header.Get("Accept-Encoding")) || r.Header.Get("Upgrade") != "" {
				next.ServeHTTP(w, r)
				return
			}
			gw := &gzipWriter{ResponseWriter: w, head: r.Method == http.MethodHead}
			next.ServeHTTP(gw, r)
			if gw.gz != nil {
				gw.gz.Close()
			}
		})
	}
}

func acceptsGzip(h string) bool {
	for part := range strings.SplitSeq(h, ",") {
		coding, params, _ := strings.Cut(part, ";")
		if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
			continue
		}
		if _, q, ok := strings.Cut(params, "q="); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(q), 64)
			return err != nil || f > 0
		}
		return true
	}
	return false
}

type gzipWriter struct {
	http.ResponseWriter
	head    bool
	gz      *gzip.Writer
	decided bool
}

func (g *gzipWriter) WriteHeader(code int) {
	if code < 200 && code != http.StatusSwitchingProtocols {
		g.ResponseWriter.WriteHeader(code)
		return
	}
	if g.decided {
		return
	}
	g.decided = true
	h := g.Header()
	h.Add("Vary", "Accept-Encoding")
	plain := g.head || code == http.StatusNoContent || code == http.StatusNotModified ||
		code == http.StatusSwitchingProtocols || h.Get("Content-Encoding") != "" || h.Get("Content-Range") != ""
	if !plain {
		h.Set("Content-Encoding", "gzip")
		h.Del("Content-Length")
		if h.Get("Content-Type") == "" {
			h.Set("Content-Type", "application/octet-stream")
		}
		g.gz = gzip.NewWriter(g.ResponseWriter)
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) Write(p []byte) (int, error) {
	if !g.decided {
		if g.Header().Get("Content-Type") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(p))
		}
		g.WriteHeader(http.StatusOK)
	}
	if g.gz == nil {
		return g.ResponseWriter.Write(p)
	}
	return g.gz.Write(p)
}

func (g *gzipWriter) Flush() {
	if !g.decided {
		g.WriteHeader(http.StatusOK)
	}
	if g.gz != nil {
		g.gz.Flush()
	}
	http.NewResponseController(g.ResponseWriter).Flush()
}

func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }
