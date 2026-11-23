package main

import (
	"context"
	"crypto/subtle"
	"net/http"
	"os"
	"strings"

	"github.com/swiftly-headquarters/minato/example/store"
	"github.com/swiftly-headquarters/minato/server"
)

func registerAdmin(api *server.API) {
	token := os.Getenv("ADMIN_TOKEN")
	if token == "" {
		token = "dev-token"
	}
	admin := api.Group("/admin", requireToken(token))
	admin.Tags = []string{"admin"}
	admin.Security = server.Secured("bearer")
	server.Get(admin, "/stats", stats, summary("Count users"), errs(401))
}

func requireToken(want string) server.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				server.WriteError(w, server.Error401("missing or invalid bearer token"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
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
