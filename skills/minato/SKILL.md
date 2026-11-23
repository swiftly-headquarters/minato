---
name: minato
description: Write and change HTTP endpoints with minato, a Go app server framework where each route is a typed function and its input and output structs define binding, validation and the OpenAPI 3.1 document. Covers path, query, header and cookie params, JSON bodies, form and file uploads, streaming and server-sent events, response formats, groups, middleware (logging, CORS, gzip, timeouts), security, and serving static files, a single-page app or a reverse proxy to a dev server. Use when a Go project imports github.com/swiftly-headquarters/minato/server, middleware or web, or when asked to add or edit an endpoint, request or response type, validation rule, upload, stream, error response or API spec in such a project.
---

# minato

Go app server framework, standard library only. A handler is a plain function; its input and output struct types drive request binding, validation and the OpenAPI spec. Routes are registered in code: no code generation, no file-based routing.

Install: `go get github.com/swiftly-headquarters/minato/server`. Needs Go 1.24+.

## Skeleton

```go
api := server.New("My API", "1.0.0")
api.Spec.Servers = []server.Server{{URL: "https://api.example.com"}}

server.Get(api, "/users/{id}", getUser)
server.Post(api, "/users", createUser, func(o *server.Operation) {
	o.Summary = "Create a user"
	o.Tags = []string{"users"}
	o.DefaultStatus = http.StatusCreated
})

ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()
err := api.Run(ctx, ":8080") // serves /openapi.json and /docs too; on ctx done it drains in-flight requests (api.ShutdownTimeout, default 10s) and returns nil
```

`*API` is also a plain `http.Handler` if you want your own `http.Server` or `httptest`.

- Helpers: `server.Get/Post/Put/Patch/Delete/Head/Options(r, path, handler, opts ...func(*Operation))`, where `r` is the `*API` or a `*Group`. (A GET route already answers HEAD.) Anything else (custom `OperationID`, `MaxBodyBytes`, `Deprecated`): `server.Register(r, server.Operation{Method: ..., Path: ...}, handler)`.
- Handler signature is always `func(ctx context.Context, in *In) (*Out, error)`. `In` and `Out` must be structs; use `*struct{}` and return `nil, nil` for no output (204).
- Paths use `net/http` patterns: `{id}`, catch-all `{path...}`, exact root `/`.
- Operation ID defaults to e.g. `get-users-by-id`.
- Registration **panics** on bad types, bad tags, or a path param missing from the path. That is intended; run the server (or a test that builds the API) after changing types.

## Input struct

| Field | Source |
|-------|--------|
| tag `path:"id"`, `query:"q"`, `header:"X-Token"`, `cookie:"sid"` | request parameter. Scalar or slice of scalar only. Path params are always required. |
| field named `Body` | JSON body, validated against its type. `[]byte` = raw bytes, no JSON. Required unless a pointer or `required:"false"`. |
| field named `Form` | form body, see Forms and uploads. Can't be combined with `Body`. |
| embedded structs | promoted, so shared params can live in one embedded type |

Slices in query/header accept repeated keys and commas (`?tag=a&tag=b,c`).

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
```

## Output struct

```go
type CreateOutput struct {
	Status   int    // optional: non-zero overrides the default status
	Location string `header:"Location"`
	Body     User   // omit for 204; []byte = raw body (set Content-Type via a header field)
}
```

Default status is 200 (204 with no `Body`); override per route with `o.DefaultStatus`. The body is encoded by `Accept` negotiation (JSON by default, see Formats); a `[]byte`, `io.Reader` or `server.SSE` body is sent as is. A `header:"Content-Type"` field on the output overrides the type for encoded and raw bodies.

## Forms and uploads

```go
type UploadInput struct {
	Form struct {
		Title string                `form:"title" minLength:"1"`
		Tags  []string              `form:"tag"`
		File  *multipart.FileHeader `form:"file" required:"true"` // []*multipart.FileHeader for several
	}
}
```

- Fields tagged `form:"name"` bind and validate like query params (same tags; a 422 reports location `form.title`).
- Without file fields the route accepts urlencoded and multipart; with one, multipart only. Any other content type returns 415.
- The whole body is capped by `MaxBodyBytes` (default 1 MiB): raise `o.MaxBodyBytes` on upload routes. Parts over 8 MiB spill to temp files that are removed after the handler returns, so open the file inside the handler.

## Streaming

- `Body io.Reader` (or `io.ReadCloser`, closed afterwards) is copied to the client and flushed as it goes. Set the type with a `header:"Content-Type"` field; the default is `application/octet-stream`.
- `Body server.SSE` sends server-sent events: `func(ctx context.Context, send func(server.SSEEvent) error) error`. Call `send(server.SSEEvent{Event: "tick", Data: v})` (Data is JSON-encoded unless it is a string or `[]byte`). Return when `ctx` is done or `send` fails; the response ends when the function returns. The 200 is sent before the function runs, so any error it returns can only be logged, not sent to the client.
- Neither is negotiated or validated. `middleware.Gzip` flushes correctly with both.

## Formats

`api.Formats` maps lower-case media types to `func(any) ([]byte, error)`, JSON by default. Add to it before registering routes so the spec lists them: `api.Formats["application/xml"] = xml.Marshal` (use `xml:` tags on the body types). The `Accept` header picks the best match by q-value (JSON wins ties); no match returns 406 before the handler runs. Errors are always `application/problem+json`. Request bodies are JSON or forms only.

## Custom schemas

A type whose JSON form differs from its Go shape implements `server.Schemer` (`Schema() *server.Schema`, called on a zero value) so the spec describes it correctly. Without a hook, `encoding.TextMarshaler` types are documented as `string`, `json.Marshaler` types as "anything", and `*multipart.FileHeader` as binary. Field tags (`doc`, `example`, ...) still apply on top.

## Validation tags

`doc format enum default example minimum maximum minLength maxLength minItems maxItems pattern required`

- `enum:"a,b"` is a comma list; on a slice it constrains the items.
- Body object fields are required unless they are pointers, `omitempty`/`omitzero`, have a `default`, or set `required:"false"`. `required:"true"` forces it.
- Defaults are filled in before the handler runs.
- `format` is documented in OpenAPI and, for strings, enforced for `email` (a bare address, no display name), `uuid`, `date-time` (RFC 3339), `date`, `uri` (needs a scheme), `ipv4`, `ipv6` and `byte` (base64; automatic for `[]byte`). Any other format is documentation only: add `pattern` to enforce it.
- Invalid input returns 422 with every problem listed; bad JSON returns 400; oversized body returns 413 (`api.MaxBodyBytes`, default 1 MiB).
- Statuses minato sends on its own: 406 (no registered format matches `Accept`, checked before the handler runs), 415 (form route, other content type), 503 (`middleware.Timeout` fired), 413, 422, 400.

## Errors

Return `server.Error400/401/403/404/409/422(detail)` or `server.NewError(status, detail, details...)`. They produce an RFC 9457 `application/problem+json` response. Any other error becomes an opaque 500 (logged via `slog`, message not sent to the client), so wrap expected failures in these helpers. A panic in a handler or middleware is recovered the same way (logged with a stack, 500 to the client).

The spec documents a generic `default` error response. To list specific statuses per route, set `o.Errors = []int{404, 409}` (4xx/5xx only, otherwise registration panics). Validation 422s are not listed automatically. In middleware, reject with `server.WriteError(w, server.Error401("..."))` so the response matches the rest of the API.

## Groups and middleware

```go
// server.Middleware is plain net/http middleware: func(http.Handler) http.Handler

api.Use(logging)                                  // whole API, docs and 404s included; first added = outermost; call before serving
v1 := api.Group("/v1", rateLimit)                 // prefix + middleware
v1.Tags = []string{"v1"}                          // inherited by routes registered after this line
admin := v1.Group("/admin", requireToken)         // sub-group: parent prefix, middleware, tags, security carry over
server.Get(admin, "/stats", stats)                // GET /v1/admin/stats
```

- Group middleware runs before the request is bound, so a rejected request never reaches validation or the handler. Parent middleware runs before child.
- `Group.Use`, `Group.Tags` and `Group.Security` only affect routes registered afterwards.
- `server.Get(g, "/", h)` inside group `/v1` matches exactly `/v1/`.

## Middleware package

`github.com/swiftly-headquarters/minato/middleware`, all plain `func(http.Handler) http.Handler`:

- `Logger(*slog.Logger)`: one line per request (method, path without query, status, bytes, duration); nil uses `slog.Default()`.
- `CORS(CORSConfig{AllowOrigins, AllowMethods, AllowHeaders, ExposeHeaders, AllowCredentials, MaxAge})`: install with `api.Use`, never on a group, because preflights match no route. `AllowCredentials` with `"*"` panics.
- `Gzip()`: compresses everything for gzip clients (no size or type filter) and passes 204, HEAD and already-encoded responses through.
- `Timeout(d)`: sets a context deadline only, so handlers must pass `ctx` to slow calls. A returned error wrapping `context.DeadlineExceeded` becomes a 503 problem. Keep it off SSE and long streams.

Typical order: `api.Use(middleware.Logger(nil), middleware.CORS(cfg), middleware.Gzip())`.

## Static files, SPAs and proxies

`github.com/swiftly-headquarters/minato/web` returns plain `http.Handler`s; mount them with `server.Handle(r, pattern, h)` (`r` is the `*API` or a `*Group`; the group prefix and middleware apply; not in the spec).

```go
build, _ := fs.Sub(dist, "dist")                                   // dist is an embed.FS of the front-end build
server.Handle(api, "/", web.Static(build, web.Config{SPA: true, Immutable: "/assets/"}))

h, err := web.Proxy("http://localhost:5173")                       // dev: forward to Vite/Next; err if not http(s)://host
server.Handle(api, "/", h)
```

- `"/"` is the catch-all: typed routes, `/openapi.json` and `/docs` win because the mux prefers the most specific pattern. Put the API under routes that are real patterns, not under the catch-all. Registering two handlers for the same pattern panics.
- `Static`: no directory listings, dotfiles are 404 (except `.well-known`), `index.html` is always `Cache-Control: no-cache`, Range works, only GET/HEAD (405 otherwise). `Immutable` is a prefix inside the FS (`"/assets/"`) for content-hashed files only.
- `SPA: true` falls back to `index.html` only for GET/HEAD requests with `Accept: text/html` and no file extension. A `fetch` to an unknown `/api/x` or a missing `/x.js` is still a 404, so don't expect curl without an Accept header to see the fallback.
- A subtree mount needs `http.StripPrefix`: `server.Handle(api, "/static/", http.StripPrefix("/static", web.Static(fsys, web.Config{})))`. The handler otherwise sees the full path, group prefix included.
- `Proxy` replaces client `X-Forwarded-*`/`Forwarded` headers, proxies WebSocket upgrades (HMR), prepends the target's path, and returns a 502 problem if the upstream is down. Keep `middleware.Timeout` off routes that proxy WebSockets or streams.
- Switching embedded build vs dev proxy on an env var (see `example/web.go`) is the usual pattern. `go:embed` needs the build directory to exist when compiling.

## Security

```go
api.BearerAuth("bearer")                       // declares the scheme in the spec
admin.Security = server.Secured("bearer")      // or o.Security = server.Secured("oauth", "read") on one operation; an operation's own value replaces the group's
```

This is documentation only. minato never checks credentials: enforce them in middleware (see above). An operation cannot opt out of `Spec.Security` set globally.

## OpenAPI

Served at `/openapi.json`, docs UI at `/docs` (Stoplight Elements from a CDN, needs internet). Don't register your own routes on those two paths. `api.Spec` is a plain struct tree you can edit before serving: `Servers`, `Info`, `Security`, `Components.SecuritySchemes`. Print it without running a server by marshalling `api.Spec` to JSON.

## Not supported (don't invent it)

Enforcement of `Security` (it is documentation), an automatic 422 entry in the spec, request bodies other than JSON and forms, a hard request timeout (`Timeout` is cooperative), non-HTTP transports.

## Testing

```go
rec := httptest.NewRecorder()
api.ServeHTTP(rec, httptest.NewRequest("GET", "/users/1", nil))
```

Build the API in a helper so tests exercise real registration and catch startup panics.
