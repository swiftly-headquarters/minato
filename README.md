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
| `middleware` | `RequestID`, `Logger`, `CORS`, `Gzip`, `Timeout`, `Bearer`, `RateLimit` and `Metrics` as plain `net/http` middleware. |
| `web`     | `Static` files, SPA fallback, a reverse `Proxy` and `Live`/`Ready` health checks, mounted with `server.Handle`.  |
| `servertest` | An in-process typed client for tests.                                                                         |
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

- **Unix socket:** `api.Run(ctx, "unix:/run/app.sock")`. A socket file left behind by a crash is replaced. A socket with a live server on it, or a regular file at that path, makes `Run` fail instead of being taken over.
- **TLS:** set `api.TLS` to a `*tls.Config` with `Certificates` or `GetCertificate` (for example from `autocert`) before running. HTTP/2 is enabled. A config with no certificate makes `Run` return an error.
- **Your own listener:** `api.Serve(ctx, ln)` does what `Run` does on a listener you made, such as port 0 in a test or a socket handed over by systemd.
- **Hooks:** `api.OnStart = func(addr net.Addr)` runs once the listener is bound, before requests are served, so it knows the real port. `api.OnStop = func(ctx context.Context)` runs after in-flight requests finished or the timeout passed, with a fresh `ShutdownTimeout` deadline: close databases there. It also runs if the server fails after starting, and not at all if the listener could not be bound.

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

Validation tags: `doc format enum default example minimum maximum exclusiveMinimum exclusiveMaximum multipleOf minLength maxLength minItems maxItems uniqueItems pattern required`. These `format` values are enforced on strings: `email`, `uuid`, `date-time`, `date`, `uri`, `ipv4`, `ipv6`, `byte`; others are documentation only. Body fields are required unless they're pointers, `omitempty`/`omitzero`, have a `default`, or set `required:"false"`. Invalid requests get a `422` RFC 9457 problem listing every error, and every route that has inputs documents that 422 in the spec automatically. Return `server.Error404(...)` / `server.NewError(status, ...)` from handlers. Any other error comes back as an opaque 500 and is logged.

## Custom validation

Checks that span fields go in a `Validate` method on the input type, or on the `Body` type. It runs after binding and the tag checks succeed, so it sees defaults and parsed values, and its errors join the same 422:

```go
func (b *Booking) Validate(ctx context.Context) []error {
	if !b.End.After(b.Start) {
		return []error{server.FieldError("body.end", "must be after start")}
	}
	return nil
}
```

A plain `error` becomes an entry without a location, and its text is sent to the client, so don't return internal errors from it. Only the input struct and its `Body` are checked, not deeper nested types.

## PATCH: omitted, null and set

`server.Optional[T]` tells "field not sent" from "sent as the zero value". `server.Nullable[T]` also allows an explicit `null`, so a client can clear a value:

```go
type PatchBody struct {
	Name server.Optional[string] `json:"name" minLength:"1"`
	Nick server.Nullable[string] `json:"nick" maxLength:"20"`
}

if b.Name.Set { user.Name = b.Name.Value }
if b.Nick.Set { if b.Nick.Null { user.Nick = "" } else { user.Nick = b.Nick.Value } }
```

Both are never required, and the usual tags apply to `T`. `null` for an `Optional` field is a 422 that points the client at omitting it. In the spec, a `Nullable` shows as `type: ["string", "null"]`, or as `anyOf` with `null` for objects. They also work as query and header params, where `Optional` tells a missing param from a supplied zero (`Nullable` is documented as nullable there, but a param can't be null). Marshalling them with `omitzero` leaves out unset values.

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

Request bodies work the same way through `api.Decoders`, picked by `Content-Type`. A decoder returns a generic value (maps with string keys, slices, strings, numbers, bools), which is then validated and bound like JSON:

```go
api.Decoders["application/yaml"] = func(b []byte) (any, error) {
	var v any
	err := yaml.Unmarshal(b, &v)
	return v, err
}
```

JSON is built in, and a missing or unrecognised `Content-Type` (including `application/merge-patch+json`) is treated as JSON. Every registered type is listed in the spec. Bind `xml` and similar formats that can't produce a generic value in a handler instead.

## Several response types

A route can return different bodies and statuses. Declare them on the operation so the spec shows each one, and have the output use `Status` and a `Body any`:

```go
type JobOutput struct {
	Status int
	Body   any
}

server.Post(api, "/jobs", createJob, func(o *server.Operation) {
	o.DefaultStatus = http.StatusCreated
	o.Responses = []server.Response{
		{Status: 201, Body: Job{}},
		{Status: 202, Body: Accepted{}, Description: "Queued, poll the job"},
	}
})

return &JobOutput{Status: 202, Body: Accepted{JobID: "j1"}}, nil
```

The runtime doesn't check the body against the declared list. It is documentation, like `Errors`.

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

## Request IDs

```go
api.Use(middleware.RequestID(middleware.RequestIDConfig{}), middleware.Logger(nil))
```

Every request gets an ID, sent back as `X-Request-ID`, stored in the context (`middleware.RequestIDFromContext(ctx)`) and set on the request headers, so `web.Proxy` passes it to the upstream. `Logger` adds it to the request line, and minato's own error and panic logs carry it. Install `RequestID` before `Logger` and the other middleware.

- **Default:** 32 random hex characters.
- **Your own generator:** `RequestIDConfig{Generate: func(r *http.Request) string { return "api-" + middleware.NewRequestID() }}`. It gets the request, so it can use the route, a tenant header or a trace ID. An empty result falls back to the default.
- **Incoming IDs** from a proxy or caller are kept when they are 1 to 128 printable ASCII characters without spaces, otherwise replaced. Set `IgnoreIncoming: true` for an edge service that shouldn't trust callers.
- **Header name:** `Header: "X-Correlation-ID"`.
- **Your own logs:** `slog.SetDefault(slog.New(middleware.SlogContext(handler)))` adds `request_id` to every record logged with a request context (`slog.InfoContext(ctx, ...)`). Wrap a real handler such as `slog.NewJSONHandler(os.Stdout, nil)`, not `slog.Default().Handler()`, which deadlocks.

## Bearer auth

`Security` in the spec is documentation. `middleware.Bearer` is the enforcement:

```go
admin := api.Group("/admin", middleware.Bearer(func(ctx context.Context, token string) (context.Context, error) {
	user, err := users.ByToken(ctx, token)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, userKey{}, user), nil
}))
admin.Security = server.Secured("bearer")
```

A missing, empty or non-Bearer `Authorization` header is a 401 with `WWW-Authenticate: Bearer`. If `verify` returns an ordinary error, the client gets a 401 with `error="invalid_token"` and no details. To pick another response, return a `*server.ErrorModel`: `server.Error403(...)`, or `server.NewError(503, ...)` when the auth backend is down. Compare secrets in constant time (`crypto/subtle`) inside `verify`.

## Rate limiting

```go
api.Use(middleware.CORS(cfg), middleware.RateLimit(middleware.RateLimitConfig{Rate: 20, Burst: 40}))
```

A token bucket per client: `Rate` requests per second on average, up to `Burst` at once (default: `Rate` rounded up). Over the limit is a 429 problem with `Retry-After`.

- **Client key:** the connecting address. IPv6 clients are grouped by /64, because one user usually owns the whole prefix. Behind a proxy every client would share the proxy's address, so give `Key` a function that reads the address your proxy vouches for. Return `""` to skip limiting a request.
- **Memory:** buckets live in memory, at most `MaxKeys` (default 10000). Idle ones are swept, and when the table is full an arbitrary bucket is dropped, which resets that client. State is per process, so with several instances each has its own limit.
- **Order:** put it after `CORS`, so rejected browser requests still carry CORS headers.
- `Now` replaces the clock, for tests.

## Health and metrics

```go
metrics := middleware.NewMetrics()
api.Use(middleware.RequestID(ids), middleware.Logger(nil), metrics.Middleware())

server.Handle(api, "GET /healthz", web.Live())
server.Handle(api, "GET /readyz", web.Ready(2*time.Second, map[string]web.Check{
	"db": func(ctx context.Context) error { return db.PingContext(ctx) },
}))
server.Handle(api, "GET /metrics", metrics)
```

- **`web.Live`** always answers `{"status":"ok"}`. **`web.Ready`** runs its checks concurrently with a shared timeout and answers 200, or 503 when any check fails, panics or times out. The body names each check as `ok`, `failed` or `timeout` and never includes the error text, which is logged instead.
- **`/metrics`** is Prometheus text: `http_requests_total{method,route,status}`, `http_request_duration_seconds` (histogram) and `http_requests_in_flight`. The `route` label is the registered pattern (`GET /users/{id}`), never the URL, so IDs don't create series. Requests that reached no route (404, 405, or rejected by middleware that runs before routing, such as the rate limiter) are `unrouted`, and methods outside the standard set are `OTHER`.
- Install `metrics.Middleware()` before middleware you want counted, such as `RateLimit`. The endpoint is public unless you mount it on a group with `Bearer` or on another port.

## Conditional requests

```go
api.ETag = true
```

Every JSON (or other negotiated) 200 response to a `GET` or `HEAD` gets a weak `ETag` computed from the encoded body, and a matching `If-None-Match` (including `*` and lists) is answered with an empty 304. The handler still runs, so this saves bandwidth, not work. Each `Accept` format has its own tag. A handler can set its own tag with an output field `ETag string `header:"ETag"``: it is used as is, and `If-None-Match` is honoured for it even when `api.ETag` is off. Streams, raw bodies, errors and non-200 statuses are left alone.

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

## Docs page

`/docs` is Stoplight Elements loaded from a CDN. That needs internet access in the browser, and `/openapi.json` always works without it. For a page with no external requests, or your own:

```go
api.Docs = api.OfflineDocs()
api.Docs = myHandler
```

`OfflineDocs` renders the spec on the server: operations, parameters, bodies, responses and schemas, with no JavaScript, and it is read on each request so it always matches the spec.

## Spec files, CI and SDKs

The spec is served at `/openapi.json` and `/openapi.yaml`, and can be written without starting a server:

```go
api.WriteSpec("openapi.yaml") // .yaml and .yml give YAML, anything else JSON
b, err := api.SpecJSON()      // or api.SpecYAML()
```

The example does this with `go run ./example openapi openapi.json` (no file name prints JSON). Commit the result and let CI fail when it is stale, so API changes show up in review. This is the step in `.github/workflows/build.yml`:

```bash
go run ./example openapi example/openapi.json
if [ -n "$(git status --porcelain -- example/openapi.json)" ]; then git --no-pager diff -- example/openapi.json; exit 1; fi
```

The YAML is written by a small built-in converter from the JSON, so there is still no dependency, and every string and ambiguous key (`"on"`, `"200"`, `"null"`) is quoted. The in-repo tests check the quoting rules, not full YAML parsing, because that needs a YAML library.

The spec feeds [takumi](https://github.com/swiftly-headquarters/takumi), which generates typed Go and TypeScript clients. CI generates a Go SDK from the example spec and vets it, and generates a TypeScript SDK, compiles it in strict mode and runs `tests/sdk/typescript/run.mjs` against the running example (JSON, a multipart upload, a catch-all path, bearer headers, 422 and 404 as errors). The Go SDK step:

```bash
go install github.com/swiftly-headquarters/takumi@v0.1.1
takumi -input example/openapi.json -output ./sdk -lang go
```

## Testing your API

`servertest` calls an API in process, with no server and no ports:

```go
c := servertest.New(t, api)
c.Header.Set("Authorization", "Bearer dev-token") // sent with every request

u := servertest.Decode[User](c.Post("/users", map[string]string{"name": "Ada"}).Status(201))
c.Get("/users/99").Status(404)
c.Get("/whoami", "X-Token", "override") // per-call headers are key, value pairs
```

A body that is a struct, map or slice is sent as JSON. A `string`, `[]byte` or `io.Reader` is sent as is, so pass `"Content-Type", "application/x-www-form-urlencoded"` for a form. `Status` and `Decode` fail the test through `t` with the status and response body in the message. A `*Response` embeds `*httptest.ResponseRecorder`, so `Code`, `Header()` and `Body` are there too. `Do(method, path, body, headers...)` covers methods without a helper.

## Example

```bash
go run ./example           # http://localhost:8080/docs
go run ./example openapi   # print the spec (add a file name to write it, .yaml for YAML)
OFFLINE_DOCS=1 go run ./example   # /docs rendered by the server, no CDN
WEB_PROXY=http://localhost:5173 go run ./example   # proxy "/" to a dev server instead of the embedded build
REQUEST_ID_PREFIX=ex go run ./example              # custom request id generator: ex-<random>
TLS_CERT=cert.pem TLS_KEY=key.pem go run ./example # serve HTTPS
```

## Development

```bash
go vet ./...
go test ./...
```

After changing the API, regenerate the committed spec: `go run ./example openapi example/openapi.json` and the same for `example/openapi.yaml`.
