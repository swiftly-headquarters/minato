---
name: minato
description: Write and change HTTP endpoints with minato, a Go app server framework where each route is a typed function and its input and output structs define binding, validation and the OpenAPI 3.1 document. Covers path, query, header and cookie params, JSON bodies, custom validation hooks, PATCH with optional and nullable fields, other request body formats, several response types per route, form and file uploads, streaming and server-sent events, response formats, groups, middleware (logging, CORS, gzip, timeouts), security, serving static files, a single-page app or a reverse proxy to a dev server, writing the OpenAPI spec to a file or checking it in CI, generating SDKs from it with takumi, running over TLS or a unix socket with start and stop hooks, in-process API tests, request IDs, bearer auth, rate limiting, health checks, Prometheus metrics, ETags. Use when a Go project imports github.com/swiftly-headquarters/minato/server, middleware, web or servertest, or when asked to add or edit an endpoint, request or response type, validation rule, upload, stream, error response or API spec in such a project.
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

`api.Formats` maps lower-case media types to `func(any) ([]byte, error)`, JSON by default. Add to it before registering routes so the spec lists them: `api.Formats["application/xml"] = xml.Marshal` (use `xml:` tags on the body types). The `Accept` header picks the best match by q-value (JSON wins ties); no match returns 406 before the handler runs. Errors are always `application/problem+json`.

Request bodies: JSON is built in, and a missing or unknown `Content-Type` (including `application/merge-patch+json`) is treated as JSON. `api.Decoders["application/yaml"] = func(b []byte) (any, error)` adds a type, chosen by `Content-Type`; set it before registering routes so the spec lists it. The decoder must return a generic value (string-keyed maps, slices, strings, numbers, bools), because the result is validated and bound like JSON. Formats that can't (XML) belong in a handler with a `[]byte` body.

## Several response types

`o.Responses = []server.Response{{Status: 202, Description: "...", Body: Accepted{}}}` documents extra statuses and bodies in the spec (any status 100-599, else registration panics; a `Body` is a zero value of the type, nil for none). The output struct then returns whichever it wants: `Status int` plus `Body any`. Documentation only, the runtime doesn't check the body against the list. An entry for the default status replaces its description and body, and keeps the header fields.

## Custom schemas

A type whose JSON form differs from its Go shape implements `server.Schemer` (`Schema() *server.Schema`, called on a zero value) so the spec describes it correctly. Without a hook, `encoding.TextMarshaler` types are documented as `string`, `json.Marshaler` types as "anything", and `*multipart.FileHeader` as binary. Field tags (`doc`, `example`, ...) still apply on top.

## Validation tags

`doc format enum default example minimum maximum exclusiveMinimum exclusiveMaximum multipleOf minLength maxLength minItems maxItems uniqueItems pattern required`

- `multipleOf` must be above 0 (else registration panics). `uniqueItems:"true"` compares numbers by value (`1` equals `1.0`) and reports the later duplicate's index.

- `enum:"a,b"` is a comma list; on a slice it constrains the items.
- Body object fields are required unless they are pointers, `omitempty`/`omitzero`, have a `default`, or set `required:"false"`. `required:"true"` forces it.
- Defaults are filled in before the handler runs.
- `format` is documented in OpenAPI and, for strings, enforced for `email` (a bare address, no display name), `uuid`, `date-time` (RFC 3339), `date`, `uri` (needs a scheme), `ipv4`, `ipv6` and `byte` (base64; automatic for `[]byte`). Any other format is documentation only: add `pattern` to enforce it.
- Invalid input returns 422 with every problem listed; bad JSON returns 400; oversized body returns 413 (`api.MaxBodyBytes`, default 1 MiB).
- Statuses minato sends on its own: 406 (no registered format matches `Accept`, checked before the handler runs), 415 (form route, other content type), 503 (`middleware.Timeout` fired), 413, 422, 400.

## Custom validation

A cross-field check is a `Validate(ctx context.Context) []error` method on the input struct, or on the `Body` type (both run, input first). It runs only after binding and tag validation passed, so it sees defaults and parsed values, and its errors are added to the same 422. `server.FieldError("body.end", "must be after start")` sets the location; a plain `error` gets none and its text is sent to the client, so never return internal errors. Nested types deeper than `Body` are not checked.

## PATCH: Optional and Nullable

For partial updates use `server.Optional[T]` (omitted vs sent, including the zero value) and `server.Nullable[T]` (also an explicit `null`). Both have `Value T` and `Set bool`, `Nullable` also `Null bool`:

```go
type PatchBody struct {
	Name server.Optional[string] `json:"name" minLength:"1"`
	Nick server.Nullable[string] `json:"nick"`
}
if b.Name.Set { u.Name = b.Name.Value }
if b.Nick.Set { if b.Nick.Null { u.Nick = "" } else { u.Nick = b.Nick.Value } }
```

They are never required, tags apply to `T`, and `null` for an `Optional` is a 422 ("omit the field, or use server.Nullable"). The spec shows `Nullable` as `type: ["string","null"]` (or `anyOf` with null for objects and refs). Use `omitzero` when marshalling them in outputs. `Optional` also works on query and header params. Don't use pointers for this: a `*T` can't tell omitted from null.

## Errors

Return `server.Error400/401/403/404/409/422(detail)` or `server.NewError(status, detail, details...)`. They produce an RFC 9457 `application/problem+json` response. Any other error becomes an opaque 500 (logged via `slog`, message not sent to the client), so wrap expected failures in these helpers. A panic in a handler or middleware is recovered the same way (logged with a stack, 500 to the client).

The spec documents a generic `default` error response. To list specific statuses per route, set `o.Errors = []int{404, 409}` (4xx/5xx only, otherwise registration panics). A 422 is added to the spec automatically for any route with params, a body or a `Validate` hook, so don't list 422 yourself. In middleware, reject with `server.WriteError(w, server.Error401("..."))` so the response matches the rest of the API.

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

Typical order (first = outermost): `api.Use(middleware.RequestID(ids), middleware.Logger(nil), metrics.Middleware(), middleware.CORS(cfg), middleware.RateLimit(limits), middleware.Gzip())`.

### Production middleware

- **`RequestID(RequestIDConfig{Header, Generate, IgnoreIncoming})`**: every request gets an ID, set as `X-Request-ID` (or `Header`) on the response and on the request headers (so `web.Proxy` forwards it), and stored in the context: `middleware.RequestIDFromContext(ctx)`. Default is 32 random hex chars (`middleware.NewRequestID()`). A custom generator is `Generate: func(r *http.Request) string { return "api-" + middleware.NewRequestID() }`; an empty return falls back to the default. A valid incoming ID (1-128 printable ASCII, no spaces) is kept unless `IgnoreIncoming`; anything else is replaced. Put it first so `Logger` and everything after sees the ID. `Logger` adds `request_id` itself, and minato's unhandled-error and panic logs include it.
- **`SlogContext(handler)`**: wraps a slog handler so records logged with a request context (`slog.InfoContext(ctx, ...)`) get `request_id`. `slog.SetDefault(slog.New(middleware.SlogContext(slog.NewJSONHandler(os.Stdout, nil))))`. Never wrap `slog.Default().Handler()` (deadlock).
- **`Bearer(verify func(ctx, token) (context.Context, error))`**: enforces `Authorization: Bearer`. No or malformed header: 401 with `WWW-Authenticate: Bearer`. Ordinary error from `verify`: 401 `invalid_token` without details. Return a `*server.ErrorModel` (`server.Error403(...)`, `server.NewError(503, ...)`) to choose the response. Return a derived context to pass the user to handlers. Use on a group; pair it with `Security = server.Secured("bearer")` so the spec says the same. Compare secrets with `crypto/subtle`. `Bearer(nil)` panics.
- **`RateLimit(RateLimitConfig{Rate, Burst, Key, MaxKeys, Now})`**: in-memory token bucket per client, 429 problem with `Retry-After`. `Rate` (per second, above 0, else panic) and `Burst` (default `ceil(Rate)`). Default key is the connecting address with IPv6 grouped by /64; behind a proxy supply `Key` (the real client address), and return `""` to exempt a request. Per process, bounded by `MaxKeys` (10000). Place it after `CORS`.
- **`NewMetrics()`**: `m.Middleware()` records, `server.Handle(api, "GET /metrics", m)` serves Prometheus text: `http_requests_total{method,route,status}`, `http_request_duration_seconds`, `http_requests_in_flight`. `route` is the registered pattern; requests that reached no route (404, 405, or rejected by middleware that runs earlier, like `RateLimit`) are `unrouted`; unusual methods are `OTHER`. Protect `/metrics` if the server is public.
- **Health** (package `web`): `server.Handle(api, "GET /healthz", web.Live())` and `server.Handle(api, "GET /readyz", web.Ready(timeout, map[string]web.Check{"db": func(ctx) error {...}}))`. Checks run concurrently; 503 if any fails, panics or times out; the body only says `ok`/`failed`/`timeout` per check (errors are logged, not sent).
- **ETag**: `api.ETag = true` gives negotiated 200 GET/HEAD responses a weak `ETag` and answers `If-None-Match` with an empty 304 (the handler still runs). A handler-provided `header:"ETag"` output field is used instead and honoured even when `api.ETag` is off. Streams, raw bodies, errors and other statuses are untouched.

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
api.AddScheme("bearer", server.Scheme{
	Spec:      map[string]any{"type": "http", "scheme": "bearer"},
	Challenge: "Bearer",
	Verify: func(ctx context.Context, r *http.Request, scopes []string) (context.Context, error) {
		token, ok := server.BearerToken(r)
		if !ok { return nil, errors.New("missing token") }
		// return a context for the handler, or an error
		return ctx, nil
	},
})
admin.Security = server.Secured("bearer")      // or o.Security = server.Secured("bearer", "admin") for scopes on one operation; an operation's own value replaces the group's
```

- Registered with `AddScheme`, the scheme is enforced on every route that names it, after group middleware and before binding. A plain error from `Verify` is a generic 401 with `Challenge` as `WWW-Authenticate`; a `*server.ErrorModel` (for example `server.Error403`) is sent as is.
- Alternatives are OR-ed (`[]map[string][]string{{"bearer": nil}, {"key": nil}}`); schemes in one alternative are AND-ed.
- Registered only with `BearerAuth`, a scheme is documentation and not enforced. An unregistered name panics at registration, so register schemes before the routes that use them.
- Secured routes document a 401.

## Security headers, idempotency, pagination, readiness, tracing

- `api.Use(middleware.SecureHeaders(middleware.SecureHeadersConfig{}))`: nosniff, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`. `ContentSecurityPolicy` and `HSTSMaxAge` are opt-in (a CSP like `default-src 'none'` breaks pages that load scripts, such as `/docs` or a SPA).
- `api.Use(middleware.Idempotency(middleware.IdempotencyConfig{}))`: POST/PATCH with `Idempotency-Key` replay the first final response (marked `Idempotent-Replayed: true`); same key with a different method, path, body or `Authorization` is 422; a duplicate while the first runs is 409. 5xx and 401/403/408/425/429 are not stored so they can be retried. `TTL` (24 h) is how long a response is kept, `LockTTL` (5 min) how long an unfinished request holds its key. The default store is in-memory; for several instances implement `middleware.IdempotencyStore`:
  - `Begin(ctx, key, fingerprint, ttl) (existing *IdempotencyEntry, claimed bool, err error)` is one atomic step (Redis `SET NX PX`, unique insert): a free key stores a pending entry for `ttl` and returns `nil, true, nil`; a taken key returns a copy and `false`. Exactly one of N concurrent callers may claim. An error here makes the middleware answer 503 without running the handler.
  - `Finish(ctx, key, entry, ttl)` replaces the pending entry with the final one for `ttl` (the response TTL). `Release(ctx, key)` deletes it and is not an error for an unknown key. Their errors are only logged, and their context is not cancelled when the client hangs up.
  - Return copies, never "key exists" as an error, and let pending entries expire by themselves. Test with `idempotencytest.Run(t, func(t *testing.T) middleware.IdempotencyStore { return newStore(t) })`, each call returning an empty store.
- Pagination: embed `server.Page` in the input (`limit`, default 20, max 100; `cursor`), return `server.PageBody[T]`, and build it with `server.Paginate(rows, in.Limit, func(x T) any { return x.ID })` after fetching `in.Limit+1` rows. Decode incoming cursors with `server.DecodeCursor`, and on failure return `server.InvalidCursor(err)` (a 422 on `query.cursor`). Cursors are opaque; the next cursor is absent on the last page.
- `api.ShutdownDelay = 5 * time.Second` keeps serving after shutdown starts so a load balancer can drain. `server.Handle(api, "GET /readyz", web.ReadyFor(api, timeout, checks))` answers 503 `draining` from the moment shutdown starts. HTTP timeouts default to header 10 s, read 1 min, idle 2 min, write none (streams); raise `ReadTimeout` for big uploads.
- `api.Use(middleware.Trace(tracer))`, where `Tracer` is `Start(r *http.Request) (context.Context, middleware.Span)` and `Span` is `End(middleware.SpanResult)`. The returned context reaches the handler; `End` runs once with `Route` (the matched pattern such as `GET /users/{id}`, or `unrouted`) and `Status` (500 after a panic). Name the span in `End`, since the route is unknown in `Start`. A nil span skips the request, a nil context keeps the request's own, `Trace(nil)` panics. `middleware.TracerFunc` adapts a function. Wrap OpenTelemetry or anything else in the app; the framework has no dependency.

## OpenAPI

Served at `/openapi.json`, docs UI at `/docs` (Stoplight Elements from a CDN by default, so the browser needs internet). `api.Docs = api.OfflineDocs()` serves a plain server-rendered page with no external requests, and `api.Docs` takes any `http.Handler`. Also served as YAML at `/openapi.yaml`. Don't register your own routes on those paths. `api.Spec` is a plain struct tree you can edit before serving: `Servers`, `Info`, `Security`, `Components.SecuritySchemes`.

Without starting a server: `api.WriteSpec("openapi.yaml")` (`.yaml`/`.yml` give YAML, anything else JSON), or `api.SpecJSON()` / `api.SpecYAML()` for bytes. Typical use is a small `openapi` subcommand in the app's `main` that builds the API and calls `WriteSpec`, with the file committed and CI failing when regenerating it changes anything (`git status --porcelain -- openapi.json` non-empty). Spec files can be fed to `takumi` (`takumi -input openapi.json -output ./sdk -lang go|typescript`) to generate a typed client; in CI, pin its version.

## Running

`api.Run(ctx, addr)` serves until `ctx` is done. `addr` is `":8080"`, or `"unix:/run/app.sock"` for a unix socket (a stale socket file from a crash is replaced; a live socket or a regular file at that path is an error). `api.Serve(ctx, ln)` takes your own listener (port 0 in tests, systemd sockets). Set `api.TLS = &tls.Config{Certificates: ...}` (or `GetCertificate`) before running to serve HTTPS with HTTP/2; a config with no certificate makes it return an error. `api.OnStart = func(net.Addr)` runs after the listener is bound and before requests are served; `api.OnStop = func(context.Context)` runs after in-flight requests finished or `ShutdownTimeout` passed (the context has its own `ShutdownTimeout` deadline), and also if the server fails after starting, but not if binding failed. Use `OnStop` to close databases.

## Not supported (don't invent it)

Security schemes beyond a `Verify` hook (OAuth flows, scope checks beyond what `Verify` does, token issuance), request bodies that can't decode to a generic value (XML) besides JSON, forms and `Decoders`, validation hooks on types nested below `Body`, a hard request timeout (`Timeout` is cooperative), transports other than HTTP.

## Testing

Use `github.com/swiftly-headquarters/minato/servertest`, which calls the API in process (no ports):

```go
c := servertest.New(t, api)                        // build api in a helper so tests exercise real registration and catch startup panics
c.Header.Set("Authorization", "Bearer dev-token")  // default headers for every request
u := servertest.Decode[User](c.Post("/users", map[string]string{"name": "Ada"}).Status(201))
c.Get("/users/99").Status(404)
c.Get("/whoami", "X-Token", "override")            // per-call headers are key, value pairs and win over c.Header
```

- Helpers: `Get`, `Delete` (path, headers...), `Post`, `Put`, `Patch` (path, body, headers...), and `Do(method, path, body, headers...)` for the rest.
- A struct, map or slice body is sent as JSON with `Content-Type: application/json`. A `string`, `[]byte` or `io.Reader` is sent as is: add `"Content-Type", "application/x-www-form-urlencoded"` for forms.
- `Status(code)` and `Decode[T]` call `t.Fatalf` with the status and body on failure. `*Response` embeds `*httptest.ResponseRecorder` (`Code`, `Header()`, `Body`). `Decode` handles JSON only.
- It tests the handler chain, not a network listener: for TLS, sockets or shutdown use `api.Serve` on a `net.Listen("tcp", "127.0.0.1:0")` listener.
