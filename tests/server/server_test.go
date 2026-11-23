package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/swiftly-headquarters/minato/server"
)

type params struct {
	ID    int      `path:"id" minimum:"1"`
	Limit int      `query:"limit" default:"10" maximum:"100"`
	Tags  []string `query:"tag"`
	Token string   `header:"X-Token" required:"true"`
}

type item struct {
	Name string  `json:"name" minLength:"2"`
	Kind string  `json:"kind" enum:"a,b" default:"a"`
	Note *string `json:"note,omitempty"`
}

type itemIn struct {
	params
	Body item
}

type itemOut struct {
	ETag string `header:"ETag"`
	Body struct {
		ID    int      `json:"id"`
		Limit int      `json:"limit"`
		Tags  []string `json:"tags"`
		Item  item     `json:"item"`
	}
}

func newTestAPI() *server.API {
	api := server.New("test", "1")
	server.Register(api, server.Operation{Method: "POST", Path: "/items/{id}", DefaultStatus: 201}, func(ctx context.Context, in *itemIn) (*itemOut, error) {
		switch in.ID {
		case 404:
			return nil, server.Error404("no such item")
		case 500:
			return nil, errors.New("secret db failure")
		}
		out := &itemOut{ETag: "v1"}
		out.Body.ID, out.Body.Limit, out.Body.Tags, out.Body.Item = in.ID, in.Limit, in.Tags, in.Body
		return out, nil
	})
	return api
}

func do(api *server.API, url, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-Token", token)
	}
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	return rec
}

func TestValidRequest(t *testing.T) {
	rec := do(newTestAPI(), "/items/5?tag=x&tag=y,z", "t", `{"name":"ab"}`)
	if rec.Code != 201 || rec.Header().Get("ETag") != "v1" {
		t.Fatalf("got %d %v: %s", rec.Code, rec.Header(), rec.Body)
	}
	want := `{"id":5,"limit":10,"tags":["x","y","z"],"item":{"name":"ab","kind":"a"}}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("body\n got %s\nwant %s", got, want)
	}
}

func TestValidationErrors(t *testing.T) {
	rec := do(newTestAPI(), "/items/0?limit=500", "", `{"name":"a","kind":"c"}`)
	var em server.ErrorModel
	json.Unmarshal(rec.Body.Bytes(), &em)
	var locs []string
	for _, e := range em.Errors {
		locs = append(locs, e.Location)
	}
	want := "path.id query.limit header.X-Token body.kind body.name"
	if rec.Code != 422 || strings.Join(locs, " ") != want {
		t.Fatalf("got %d %q, want 422 %q: %s", rec.Code, locs, want, rec.Body)
	}
	if rec := do(newTestAPI(), "/items/abc", "t", `{"name":"ab"}`); rec.Code != 422 {
		t.Fatalf("non-numeric path id: got %d", rec.Code)
	}
	if rec := do(newTestAPI(), "/items/1", "t", `{}`); rec.Code != 422 || !strings.Contains(rec.Body.String(), "body.name") {
		t.Fatalf("missing required field: got %d %s", rec.Code, rec.Body)
	}
	if rec := do(newTestAPI(), "/items/1", "t", ``); rec.Code != 422 {
		t.Fatalf("missing body: got %d", rec.Code)
	}
}

func TestErrors(t *testing.T) {
	api := newTestAPI()
	if rec := do(api, "/items/1", "t", `{bad`); rec.Code != 400 {
		t.Fatalf("bad json: got %d", rec.Code)
	}
	rec := do(api, "/items/404", "t", `{"name":"ab"}`)
	if rec.Code != 404 || rec.Header().Get("Content-Type") != "application/problem+json" || !strings.Contains(rec.Body.String(), "no such item") {
		t.Fatalf("404: got %d %s", rec.Code, rec.Body)
	}
	rec = do(api, "/items/500", "t", `{"name":"ab"}`)
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("500 must not leak: got %d %s", rec.Code, rec.Body)
	}
	api.MaxBodyBytes = 8
	if rec := do(api, "/items/1", "t", `{"name":"abcdefgh"}`); rec.Code != 413 {
		t.Fatalf("large body: got %d", rec.Code)
	}
}

func TestConvenience(t *testing.T) {
	api := server.New("test", "1")
	type in struct {
		ID int `path:"id"`
	}
	server.Delete(api, "/things/{id}", func(ctx context.Context, i *in) (*struct{}, error) { return nil, nil },
		func(o *server.Operation) { o.Summary = "Remove a thing" })
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest("DELETE", "/things/3", nil))
	op := api.Spec.Paths["/things/{id}"]["delete"]
	if rec.Code != 204 || op == nil || op.Summary != "Remove a thing" || op.OperationID != "delete-things-by-id" {
		t.Fatalf("got %d %+v", rec.Code, op)
	}
}

func TestOpenAPI(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestAPI().ServeHTTP(rec, httptest.NewRequest("GET", "/openapi.json", nil))
	var spec struct {
		Paths      map[string]map[string]server.OperationDoc
		Components struct{ Schemas map[string]server.Schema }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	op := spec.Paths["/items/{id}"]["post"]
	if op.OperationID != "post-items-by-id" || len(op.Parameters) != 4 || op.Responses["201"] == nil || op.RequestBody == nil {
		t.Fatalf("bad operation: %+v", op)
	}
	if s := spec.Components.Schemas["item"]; strings.Join(s.Required, ",") != "name" || len(s.Properties["kind"].Enum) != 2 {
		t.Fatalf("bad item schema: %+v", s)
	}
	if _, ok := spec.Components.Schemas["ErrorModel"]; !ok {
		t.Fatal("missing ErrorModel")
	}
}
