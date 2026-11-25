package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"os"

	"github.com/swiftly-headquarters/minato/example/store"
	"github.com/swiftly-headquarters/minato/server"
)

func registerAdmin(api *server.API) {
	token := os.Getenv("ADMIN_TOKEN")
	if token == "" {
		token = "dev-token"
	}
	api.AddScheme("bearer", server.Scheme{
		Spec:      map[string]any{"type": "http", "scheme": "bearer"},
		Challenge: "Bearer",
		Verify: func(ctx context.Context, r *http.Request, _ []string) (context.Context, error) {
			got, ok := server.BearerToken(r)
			if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				return nil, errors.New("wrong token")
			}
			return ctx, nil
		},
	})
	admin := api.Group("/admin")
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
