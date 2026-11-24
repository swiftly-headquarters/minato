package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"os"

	"github.com/swiftly-headquarters/minato/example/store"
	"github.com/swiftly-headquarters/minato/middleware"
	"github.com/swiftly-headquarters/minato/server"
)

func registerAdmin(api *server.API) {
	token := os.Getenv("ADMIN_TOKEN")
	if token == "" {
		token = "dev-token"
	}
	admin := api.Group("/admin", middleware.Bearer(func(ctx context.Context, got string) (context.Context, error) {
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			return nil, errors.New("wrong token")
		}
		return ctx, nil
	}))
	admin.Tags = []string{"admin"}
	admin.Security = server.Secured("bearer")
	server.Get(admin, "/stats", stats, summary("Count users"), errs(401))
}

type StatsOutput struct {
	Body struct {
		Users int `json:"users"`
	}
}

func stats(ctx context.Context, _ *struct{}) (*StatsOutput, error) {
	out := &StatsOutput{}
	out.Body.Users = store.Count()
	return out, nil
}
