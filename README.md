# Minato

[![Build Status](https://img.shields.io/github/actions/workflow/status/swiftly-headquarters/minato/build.yml?branch=main)](https://github.com/swiftly-headquarters/minato/actions)
[![Release](https://img.shields.io/github/v/release/swiftly-headquarters/minato?include_prereleases)](https://github.com/swiftly-headquarters/minato/releases)

App server framework for Go: typed handler functions, request validation and generated OpenAPI 3.1.

Standard library only.

## Install

```bash
go get github.com/swiftly-headquarters/minato/server
```

## Layout

| Package   | Purpose                                                                                                          |
| --------- | ---------------------------------------------------------------------------------------------------------------- |
| `server`  | The framework: typed handlers, request binding and validation, RFC 9457 errors, OpenAPI document and docs page. |
| `middleware` | `Logger`, `CORS`, `Gzip` and `Timeout` as plain `net/http` middleware.                                        |
| `web`     | `Static` files, SPA fallback and a reverse `Proxy`, mounted with `server.Handle`.                                |
| `tests`   | External tests, mirroring the package layout.                                                                    |
| `example` | A small app using all of the above.                                                                              |

## Quick start

```go
type GreetInput struct {
	Name string `path:"name" maxLength:"32" doc:"Who to greet"`
}

type GreetOutput struct {
	Body struct {
		Message string `json:"message" example:"Hello, world!"`
	}
}

func main() {
	api := server.New("My API", "1.0.0")

	server.Get(api, "/greet/{name}", func(ctx context.Context, in *GreetInput) (*GreetOutput, error) {
		out := &GreetOutput{}
		out.Body.Message = "Hello, " + in.Name + "!"
		return out, nil
	})

	http.ListenAndServe(":8080", api) // also serves /openapi.json and /docs
}
```

`server.Get`, `Post`, `Put`, `Patch` and `Delete` take optional funcs to tweak the operation, and `server.Register` takes the full `Operation`:

```go
server.Post(api, "/users", createUser, func(o *server.Operation) {
	o.Summary = "Create a user"
	o.Tags = []string{"users"}
	o.DefaultStatus = http.StatusCreated
})

server.Register(api, server.Operation{
	Method:      http.MethodGet,
	Path:        "/files/{path...}", // net/http patterns: {name} and trailing {name...}
	OperationID: "get-file",
	Summary:     "Download a file",
}, getFile)
```

The operation ID defaults to e.g. `get-users-by-id`. Registration panics on bad types or tags, so mistakes show up at startup.

## Groups, middleware and security

Middleware is plain `func(http.Handler) http.Handler`. `api.Use` wraps everything; a group adds a path prefix, middleware, tags and security to the routes registered on it, and groups nest:

```go
api.Use(logging)

admin := api.Group("/admin", requireToken)
admin.Tags = []string{"admin"}
admin.Security = server.Secured("bearer") // documented in the spec only
api.BearerAuth("bearer")

server.Get(admin, "/stats", stats, func(o *server.Operation) {
	o.Errors = []int{401} // listed as responses in the spec
})
```

`requireToken` is where the check actually happens; reject with `server.WriteError(w, server.Error401("..."))` to match the API's error format. See `example/admin.go`.

Panics in handlers and middleware are recovered and returned as a 500 problem (the stack goes to `slog`).

## Running

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()
err := api.Run(ctx, ":8080") // on ctx done, waits up to api.ShutdownTimeout (10s) for in-flight requests
```

`*API` is also a plain `http.Handler`, so `http.ListenAndServe(":8080", api)` works too.

## Inputs and outputs

```go
type CreateInput struct {
	Org   string `path:"org"`
	Dry   bool   `query:"dry" default:"false"`
	Token string `header:"X-Token" required:"true"`
	Body  struct {
		Name string `json:"name" minLength:"1" maxLength:"64" doc:"Display name"`
		Role string `json:"role" enum:"admin,member" default:"member"`
	}
}

type CreateOutput struct {
	Status   int    // optional, overrides the default status
	Location string `header:"Location"`
	Body     User   // []byte = raw body
}
```

Validation tags: `doc format enum default example minimum maximum minLength maxLength minItems maxItems pattern required`. These `format` values are enforced on strings: `email`, `uuid`, `date-time`, `date`, `uri`, `ipv4`, `ipv6`, `byte`; others are documentation only. Body fields are required unless they're pointers, `omitempty`/`omitzero`, have a `default`, or set `required:"false"`. Invalid requests get a `422` RFC 9457 problem listing every error. Return `server.Error404(...)` / `server.NewError(status, ...)` from handlers. Any other error comes back as an opaque 500 and is logged.

## Forms and uploads

Fields of a `Form` struct bind like query params, and `*multipart.FileHeader` fields receive uploads:

```go
type UploadInput struct {
	Form struct {
		Title string                `form:"title" minLength:"1"`
		File  *multipart.FileHeader `form:"file" required:"true"`
	}
}
```

A route with no file field accepts urlencoded and multipart bodies; with one, multipart only. The whole body is capped by `MaxBodyBytes` (1 MiB by default), so raise `Operation.MaxBodyBytes` on upload routes.

## Streaming

A `Body io.Reader` is copied to the client and flushed as it goes. A `Body server.SSE` sends server-sent events:

```go
type ClockOutput struct{ Body server.SSE }

func clock(ctx context.Context, _ *struct{}) (*ClockOutput, error) {
	return &ClockOutput{Body: func(ctx context.Context, send func(server.SSEEvent) error) error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case t := <-time.After(time.Second):
				if err := send(server.SSEEvent{Event: "tick", Data: t}); err != nil {
					return err
				}
			}
		}
	}}, nil
}
```

## Response formats

JSON is the default. Register more encoders before the routes and the `Accept` header chooses (by q-value, JSON wins ties; no match is a 406 before the handler runs). The spec lists every format:

```go
api.Formats["application/xml"] = xml.Marshal
```

Errors are always `application/problem+json`.

## Custom schemas

Types whose JSON differs from their Go shape can describe themselves:

```go
func (Color) Schema() *server.Schema { return &server.Schema{Type: "string", Pattern: "^#[0-9a-f]{6}$"} }
```

Without a hook, `encoding.TextMarshaler` types are documented as strings (`netip.Addr`, `net.IP`, ...) and `json.Marshaler` types as "anything".

## Middleware

```go
api.Use(middleware.Logger(nil), middleware.CORS(middleware.CORSConfig{AllowOrigins: []string{"https://app.example.com"}}), middleware.Gzip())

api.Group("/v1", middleware.Timeout(5*time.Second))
```

`CORS` goes on the API, not a group, so it sees preflight requests. `Timeout` only sets a context deadline: handlers must pass `ctx` to slow calls, and a returned `context.DeadlineExceeded` becomes a 503. `Gzip` flushes correctly, so it works with SSE. `HEAD` and `OPTIONS` handlers can be registered with `server.Head` and `server.Options`.

## Static files, SPAs and proxies

`server.Handle` mounts any `http.Handler` (not part of the spec). Mounted at `/`, it catches whatever no more specific route claims, so your typed routes, `/openapi.json` and `/docs` keep working:

```go
//go:embed dist
var dist embed.FS

build, _ := fs.Sub(dist, "dist")
server.Handle(api, "/", web.Static(build, web.Config{SPA: true, Immutable: "/assets/"}))
```

`web.Static` never lists directories, hides dotfiles (`.env`, `.git`; `.well-known` is allowed), always sends `index.html` with `no-cache`, and supports ranges. With `SPA: true`, page navigations (GET/HEAD, `Accept: text/html`, no file extension) to unknown paths get `index.html`, so `/users/42` survives a reload, while `fetch` calls to a mistyped `/api/x` and missing assets still get a 404. `Immutable` marks a prefix of content-hashed build output (Vite's `/assets/`) as cacheable for a year.

During development, forward everything else to the front-end dev server instead. WebSocket upgrades are proxied, so hot reload works:

```go
h, err := web.Proxy("http://localhost:5173")
server.Handle(api, "/", h)
```

Client-supplied `X-Forwarded-*` headers are replaced, and an unreachable upstream gives a 502 problem response. For a subtree, use `http.StripPrefix`, e.g. `server.Handle(api, "/static/", http.StripPrefix("/static", web.Static(fsys, web.Config{})))`. On a group, the prefix and middleware apply. Don't put `Timeout` on a group that proxies WebSockets.

## Example

```bash
go run ./example           # http://localhost:8080/docs
go run ./example openapi   # print the spec
WEB_PROXY=http://localhost:5173 go run ./example   # proxy "/" to a dev server instead of the embedded build
```

## Development

```bash
go vet ./...
go test ./...
```
