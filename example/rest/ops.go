package main

import (
	"context"
	"time"

	"github.com/swiftly-headquarters/minato/example/store"
	"github.com/swiftly-headquarters/minato/middleware"
	"github.com/swiftly-headquarters/minato/server"
	"github.com/swiftly-headquarters/minato/web"
)

func registerOps(api *server.API, metrics *middleware.Metrics) {
	server.Handle(api, "GET /healthz", web.Live())
	server.Handle(api, "GET /readyz", web.ReadyFor(api, 2*time.Second, map[string]web.Check{
		"store": func(context.Context) error { store.Count(); return nil },
	}))
	server.Handle(api, "GET /metrics", metrics)
}
