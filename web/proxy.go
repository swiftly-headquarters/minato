package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/swiftly-headquarters/minato/server"
)

func Proxy(target string) (http.Handler, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("web: proxy target %q must be an http:// or https:// URL", target)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			slog.Warn("minato: proxy upstream failed", "err", err, "path", r.URL.Path)
			server.WriteError(w, server.NewError(http.StatusBadGateway, "upstream unavailable"))
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Opaque != "" {
			server.WriteError(w, server.Error400("request target must be a path"))
			return
		}
		proxy.ServeHTTP(w, r)
	}), nil
}
