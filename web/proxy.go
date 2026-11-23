package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"

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
	return &httputil.ReverseProxy{
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
	}, nil
}
