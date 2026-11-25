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
| `example/app` | The API shared by both examples: middleware, hello, files, users and admin, served over REST and gRPC. |
| `example/rest` | The REST example: docs, spec files, uploads, server-sent events, health and metrics, the embedded single-page app. |
| `example/grpc` | The gRPC example: the shared API, plus `proto` to print the `.proto` file. |

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

`requireToken` is where the check actually happens; reject with `server.WriteError(w, server.Error401("..."))` to match the API's error format. See `example/app/admin.go`.

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
- **gRPC and cleartext HTTP/2:** without `api.TLS`, `Run` and `Serve` also accept cleartext HTTP/2 (h2c) next to HTTP/1.1 on the same port, which is what gRPC clients use. `api.DisableGRPC = true` turns that off. See [Protobuf and gRPC](#protobuf-and-grpc).
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

Keys in a JSON body must match field names exactly: a key that differs only in case is a 422, not silently bound. On a slice, `enum`, `minimum`/`maximum`, `minLength`/`maxLength`, `pattern` and `format` apply to each item, while `minItems`, `maxItems` and `uniqueItems` apply to the slice. A `null` item is a 422 unless the element type is `server.Nullable`. A failing request reports at most 100 problems, and an array over `maxItems` is reported without echoing it back.

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

JSON is the default, and `application/x-protobuf` is built in (see [Protobuf and gRPC](#protobuf-and-grpc)). Register more encoders before the routes and the `Accept` header chooses (by q-value; ties go to JSON, then to your own formats, then to protobuf; no match is a 406 before the handler runs). The spec lists every format:

```go
api.Formats["application/xml"] = xml.Marshal
```

Errors are always `application/problem+json`.

### Validation errors

A 422 reports every problem found in one response, up to 100: parameters, body fields and array items together. Each problem appears twice, in the form that suits the client:

```json
{
  "title": "Unprocessable Entity", "status": 422, "detail": "validation failed",
  "errors": [
    {"location": "query.limit", "message": "expected number >= 1", "value": 0},
    {"location": "body.addr.city", "message": "expected length >= 2", "value": "x"},
    {"location": "body.items[1].qty", "message": "expected number >= 1", "value": 0}
  ],
  "fields": {
    "query": {"limit": {"_errors": [{"message": "expected number >= 1"}]}},
    "body": {
      "addr": {"city": {"_errors": [{"message": "expected length >= 2"}]}},
      "items": {"1": {"qty": {"_errors": [{"message": "expected number >= 1"}]}}}
    }
  }
}
```

`errors` is the flat list (with the rejected `value`). `fields` has the same shape as the input, like the error object of the Discord API: object keys and array indexes are keys, and the messages of each field are under `_errors`. Valid fields are absent, and a problem about the whole request is under a top-level `_errors`. A field that is itself named `_errors` would clash. `fields` is only present when there are field problems.

The custom `Validate` hooks run only when the tag checks pass, so their problems arrive in a second response. A malformed JSON body is a single 400, because nothing can be checked.

Request bodies work the same way through `api.Decoders`, picked by `Content-Type`. A decoder returns a generic value (maps with string keys, slices, strings, numbers, bools), which is then validated and bound like JSON:

```go
api.Decoders["application/yaml"] = func(b []byte) (any, error) {
	var v any
	err := yaml.Unmarshal(b, &v)
	return v, err
}
```

JSON is built in. A missing `Content-Type` is read as JSON, and any other type that is neither JSON, `+json` (such as `application/merge-patch+json`) nor a registered decoder gets a 415. That stops a cross-site `text/plain` form from posting a body that is then read as JSON. Every registered type is listed in the spec. Bind `xml` and similar formats that can't produce a generic value in a handler instead.

## Protobuf and gRPC

The same handlers speak JSON and protobuf over REST, and are also served as unary gRPC methods. There is no dependency and no code generation on the server: the message layout comes from the same structs and tags that produce the OpenAPI document.

The gRPC commands below were run against `go run ./example/grpc` (service `minato.example.v1.Example`), the REST ones against `go run ./example/rest`, with `protoc` 3.21, `grpcurl` 1.9 and grpc-go 1.72.

### Quick start

1. Name the service once, before registering routes. The name is part of every call, so choose a versioned one and keep it:

   ```go
   api := server.New("Users", "1.0.0")
   api.GRPCService = "acme.users.v1.Users" // default: "minato.API"
   ```

2. Run the server as usual (`api.Run(ctx, ":8080")`). Without TLS it accepts cleartext HTTP/2 (h2c) and HTTP/1.1 on the same port. With `api.TLS` HTTP/2 is negotiated.

3. Fetch the contract. It is generated from your routes and always matches the running server:

   ```bash
   curl -s localhost:8080/api.proto -o api.proto
   ```

   Without a running server, `go run ./example/grpc proto api.proto` writes the same file. In code, `api.ProtoFile()` returns the text too. To keep a copy in your repository, write it to a file in a `go generate` step or a test and commit it, then fail CI when it changes.

4. Call it with any gRPC client. The server has no reflection, so tools need the file:

   ```bash
   grpcurl -plaintext -proto api.proto -d '{"name":"Ada"}' localhost:8080 minato.example.v1.Example/GetHello
   grpcurl -plaintext -proto api.proto -H 'authorization: Bearer secret-token-123' -d '{}' localhost:8080 minato.example.v1.Example/GetAdminStats
   ```

   Drop `-plaintext` when the server uses TLS. `grpcurl -proto api.proto list` shows the services.

### How routes become methods

| In your code | In gRPC |
| --- | --- |
| `server.Get(api, "/users/{id}", ...)` with `OperationID` `get-users-by-id` (the default) | method `GetUsersById` on the service `api.GRPCService` |
| `path`, `query`, `header`, `cookie` fields of the input | fields of the request message, named after the parameter (`X-Trace` becomes `X_Trace`, with a `json_name`) |
| the input's `Body` | the request field `body` |
| `header` fields of the output | fields of the response message |
| the output's `Body` | the response field `body` |
| middleware, `Security` schemes, validation, the handler | unchanged: a call is turned into an ordinary request |

- Two routes that end up with the same method name panic at registration. Set `o.OperationID` on one of them.
- Multipart forms, streaming bodies (`io.Reader`) and SSE are not exposed. `[]byte` bodies are exposed as `bytes`.
- A route that protobuf cannot carry (see below) is left out of `api.proto` and can still be called as `application/grpc+json`.

### Calling from Go

Generate the client from `api.proto`. The file has no `go_package` option, so tell the plugins where the package goes:

```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
protoc --go_out=. --go_opt=Mapi.proto=example.com/app/pb --go_opt=module=example.com/app \
       --go-grpc_out=. --go-grpc_opt=Mapi.proto=example.com/app/pb --go-grpc_opt=module=example.com/app api.proto
```

```go
conn, err := grpc.NewClient("localhost:8080", grpc.WithTransportCredentials(insecure.NewCredentials()))
if err != nil {
	log.Fatal(err)
}
defer conn.Close()
client := pb.NewExampleClient(conn)

ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // sent as grpc-timeout
defer cancel()

hello, err := client.GetHello(ctx, &pb.GetHelloRequest{Name: proto.String("Ada")})
fmt.Println(hello.GetBody().GetMessage())

authed := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer secret-token-123")
stats, err := client.GetAdminStats(authed, &pb.GetAdminStatsRequest{})

_, err = client.PostUsers(ctx, &pb.PostUsersRequest{Body: &pb.UserInput{Name: "", Email: "nope"}})
fmt.Println(status.Code(err), status.Convert(err).Message())
// InvalidArgument validation failed; body.email: expected format email; body.name: expected length >= 1
```

- **Optional fields** (anything that is a pointer, `Optional`, `Nullable`, has `omitempty` or a `default`) are generated as pointers, so use `proto.String`, `proto.Int64` and so on. Send nothing to get the server's `default`.
- **Metadata becomes headers.** `authorization`, `idempotency-key`, `x-request-id` and your own keys reach your middleware and security schemes as ordinary request headers, so an API that works over REST works over gRPC with no changes. Binary metadata (`-bin` keys) and the transport headers (`content-type`, `te`, `accept`, `accept-encoding` and the `grpc-*` ones) are not forwarded.
- **Deadlines:** the `grpc-timeout` of the call becomes the context deadline of the handler, so `middleware.Timeout` and your own `ctx.Done()` checks work. An expired deadline is `DeadlineExceeded`.
- **Compression:** requests may be gzip-compressed (`grpc.UseCompressor(gzip.Name)`). Responses are not compressed.

Other languages work the same way, because `api.proto` is plain proto3. Only Go and `grpcurl` were run for this guide.

### Errors and status codes

An error response of the REST API becomes a gRPC status. `grpc-message` carries the problem `detail` followed by the validation problems (`body.email: expected format email`),. With the `RequestID` middleware installed, the id is also sent back as the `x-request-id` response header.

| HTTP status of the handler | gRPC code |
| --- | --- |
| 400, 406, 415, 422 | `InvalidArgument` |
| 401 | `Unauthenticated` |
| 403 | `PermissionDenied` |
| 404 | `NotFound` |
| 409 | `Aborted` |
| 412 and other 4xx | `FailedPrecondition` |
| 413, 429 | `ResourceExhausted` |
| 408, 504, an expired `grpc-timeout` | `DeadlineExceeded` |
| the client cancelled the call | `Cancelled` |
| 501 | `Unimplemented` |
| 502, 503 | `Unavailable` |
| other 5xx | `Internal` |

Unknown services and methods are `Unimplemented`, and a message that does not parse is `InvalidArgument` (`invalid request message: ...`). A single message is limited to `api.MaxBodyBytes` (or the route's `MaxBodyBytes`), before and after decompression, and is `ResourceExhausted` when larger.

### Codecs

| Content-Type | Meaning |
| --- | --- |
| `application/grpc`, `application/grpc+proto` | protobuf, what generated clients and `grpcurl` use |
| `application/grpc+json` | JSON messages with the same field names as REST. It also accepts what `protojson` writes: 64-bit integers as strings and omitted zero values. |

To use JSON from grpc-go, register a codec named `json` (for example one built on `protojson`) and call with `grpc.CallContentSubtype("json")`. gRPC-Web is not supported.

### Protobuf over REST

Send `Content-Type: application/x-protobuf` (also `application/protobuf` and `application/vnd.google.protobuf`) and ask for `Accept: application/x-protobuf`. Only the `Body` is protobuf; path, query and header parameters stay plain HTTP. Binding, validation and the handler are the same as for JSON, and errors stay `application/problem+json`. A protobuf request with an empty body is an empty message.

The message of a REST body is the type of the `body` field in `api.proto` (`User`, or `GetHelloResponseBody` for an anonymous struct), so `protoc` can read and write it:

```bash
curl -s -H 'Accept: application/x-protobuf' 'localhost:8080/hello?name=Ada' \
  | protoc --decode=minato.example.v1.GetHelloResponseBody api.proto
# message: "Hello, Ada!"

echo 'name: "Linus" email: "linus@example.com"' | protoc --encode=minato.example.v1.UserInput api.proto > user.bin
curl -s -X POST localhost:8080/users -H 'Content-Type: application/x-protobuf' -H 'Accept: application/x-protobuf' \
  --data-binary @user.bin | protoc --decode=minato.example.v1.User api.proto
```

The same `Accept` negotiation as for JSON applies: ties go to JSON, then to your own formats, then to protobuf. To turn protobuf off, delete `"application/x-protobuf"` from `api.Formats` and from `api.Decoders`. `api.DisableGRPC = true` turns off gRPC and h2c.

### Field numbers and compatibility

Field numbers follow struct field order, starting at 1, and names are the `json` tag names. Inserting or reordering a field renumbers the others, which breaks clients that were generated earlier. Before you publish an API, pin the numbers with a `proto` tag. Valid numbers are 1 to 536870911, except 19000 to 19999:

```go
type CreateUser struct {
	Name  string `json:"name" proto:"1"`
	Email string `json:"email" proto:"2"`
	Role  string `json:"role" proto:"3"`
}

type GetUserInput struct {
	ID int64 `path:"id" proto:"1"` // numbers in the request message of the gRPC method
}
```

Duplicate or reserved numbers panic at registration. Fields without a tag take the lowest free number, so new fields can go anywhere once the others are pinned. Never reuse or change the number of a published field, and treat removing a field as a breaking change.

### What protobuf can carry

| Go | Protobuf |
| --- | --- |
| `string`, `bool` | `string`, `bool` |
| `int32`, `int64` (`int` is 64-bit), `uint32`, `uint64` | `int32`, `int64`, `uint32`, `uint64` |
| `float32`, `float64` | `float`, `double` |
| `[]byte` | `bytes` |
| struct | message |
| slice of scalars | `repeated`, packed |
| slice of structs | `repeated` message |
| `map[string]T` | `map<string, T>` |
| `time.Time` and other string types | `string` |
| `any`, `json.RawMessage`, types with a custom schema | `bytes` holding JSON text |

- **Presence:** a pointer, `Optional`, `Nullable`, a `default` or `omitempty` makes a scalar `optional`, so a client can tell "not sent" from zero. A required scalar that is not on the wire is its zero value, and the validation tags still check it (`minLength:"1"` rejects an empty string). A missing nested message counts as missing and fails if required. A `null` is written as not sent.
- **Not representable:** lists of lists and maps of lists or maps. For such a route protobuf answers 406 (response) or 415 (request), and the gRPC method is only available as `application/grpc+json`. JSON keeps working.
- **A body that is not an object** (a bare list, say) is wrapped in a message with one field, `value = 1`.
- **Unknown fields are skipped**, so an old server accepts messages from a newer client. A field sent with the wrong wire type is a 400.
- **Encoding:** fields are written in number order, zero required scalars are omitted and numbers are packed, so the bytes equal what the official libraries produce.

### Testing gRPC

`servertest` is in process and speaks REST only. To test gRPC, serve on a free port and use a real client:

```go
ln, _ := net.Listen("tcp", "127.0.0.1:0")
ctx, stop := context.WithCancel(context.Background())
defer stop()
go api.Serve(ctx, ln)

conn, _ := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
```

### Running behind a proxy

gRPC needs HTTP/2 from the client to minato. A proxy must forward gRPC as such (nginx `grpc_pass`, Envoy with an HTTP/2 upstream) and not downgrade it to HTTP/1.1. If TLS ends at the proxy, the upstream is cleartext HTTP/2, which minato accepts. This setup was not run here. The rate limiter and `Logger` see the address of the connection, so behind a proxy give `RateLimit` a `Key` function, as for REST.

### Troubleshooting

| You see | Cause and fix |
| --- | --- |
| `server does not support the reflection API` | Pass `-proto api.proto` to `grpcurl`. There is no reflection. |
| `Unimplemented: unknown method /x.Y/Z` | The service or method name differs from the server's. Compare with `GET /api.proto`: the service is `api.GRPCService` and the method is the CamelCase `OperationID`. |
| `Unimplemented: protobuf is not available for this method, use application/grpc+json` | The route uses a shape protobuf cannot carry. Change the type or use the JSON codec. |
| `InvalidArgument: invalid request message` | The client was generated from a different `api.proto`. Regenerate it and pin field numbers. |
| `InvalidArgument: path parameter "id" is required` | A path field was left empty. |
| the client cannot connect, or speaks HTTP/1.1 | gRPC needs HTTP/2. Check that `api.DisableGRPC` is false and that no proxy downgrades the connection. |
| `415` or `406` on a REST protobuf call | The body or response type cannot be protobuf, or protobuf was removed from `api.Formats` or `api.Decoders`. |
| `Unauthenticated` for a call that works over REST | Send the credential as metadata (`authorization`), not in the message. |

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

## Auth schemes

Register a scheme with `AddScheme` and the framework enforces it on every route that names it in `Secured`. `Verify` receives the request and the scopes of the operation, and returns a context for the handler or an error:

```go
api.AddScheme("bearer", server.Scheme{
	Spec:      map[string]any{"type": "http", "scheme": "bearer"},
	Challenge: "Bearer",
	Verify: func(ctx context.Context, r *http.Request, scopes []string) (context.Context, error) {
		token, ok := server.BearerToken(r)
		if !ok {
			return nil, errors.New("missing token")
		}
		user, err := users.ByToken(ctx, token)
		if err != nil {
			return nil, err
		}
		return context.WithValue(ctx, userKey{}, user), nil
	},
})

admin := api.Group("/admin")
admin.Security = server.Secured("bearer")
server.Get(admin, "/stats", stats, func(o *server.Operation) {
	o.Security = server.Secured("bearer", "admin") // scopes are passed to Verify
})
```

- Enforcement runs after the group's middleware and before the body is bound, so rejected requests never reach validation or the handler.
- A plain error from `Verify` becomes a generic 401 with the scheme's `Challenge` as `WWW-Authenticate`. Return a `*server.ErrorModel`, such as `server.Error403(...)` for a missing scope, and it is sent as is.
- Alternatives are OR-ed: `[]map[string][]string{{"bearer": nil}, {"key": nil}}` passes when either scheme does. Schemes inside one alternative are AND-ed.
- Schemes registered only with `BearerAuth` are documentation and are not enforced (the check happens in your own middleware). A name that was never registered panics at registration, so typos fail at startup. Register schemes before the routes that use them.
- `AddScheme` without a `Verify` panics. In an OR list, an alternative made only of documentation-only schemes (or an empty one) next to an enforced alternative also panics, because it would let every request through.
- `server.Handle` enforces the group's `Security` like typed routes do, so a handler mounted on a secured group is protected too.
- `api.Spec.Security` applies to every route and `Handle` mount that sets no security of its own. Set it before registering routes. An explicit `o.Security = []map[string][]string{}` opts one route out.
- `middleware.Bearer` still works for a single group if you prefer a middleware.

Routes that use a scheme document a 401 in the spec. Compare secrets in constant time (`crypto/subtle`).

## Security headers

```go
api.Use(middleware.SecureHeaders(middleware.SecureHeadersConfig{}))
```

Sets `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY` and `Referrer-Policy: no-referrer` by default. `ContentSecurityPolicy` and `HSTSMaxAge` are opt-in: a CSP such as `default-src 'none'` suits an API, but not a page that loads scripts, like `/docs` or a SPA. Handlers can override any header.

## Idempotency keys

```go
api.Use(middleware.Idempotency(middleware.IdempotencyConfig{}))
```

- For POST and PATCH requests that send `Idempotency-Key`, a repeat from the same caller with the same key, method, path, body and `Accept-Encoding` replays the first response, marked `Idempotent-Replayed: true`. The same key and caller with anything else is a 422, and a duplicate that arrives while the first is still running is a 409.
- Keys are scoped per caller: the store key is the `Idempotency-Key` plus a hash of the caller's `Authorization` header, `Cookie` header and `Host`, so another user can neither get your response nor block your key. Set `IdempotencyConfig.Scope` (`func(*http.Request) string`) to scope by something else, such as a tenant or API key id. A `Scope` that returns the same value for everyone shares keys between all callers.
- A replay sends the status, body and the headers the handler itself set. It never copies `Set-Cookie`, `Vary`, hop-by-hop headers, `X-Request-Id` or `Access-Control-*`, nor headers added by middleware outside `Idempotency`, so they keep the values of the current request.
- Only final answers are stored. 5xx responses and 401, 403, 408, 425 and 429 are not, so a failed attempt can be retried. Bodies up to 1 MiB are stored.
- The default store is in memory for one process. With several instances, or to survive restarts, implement `IdempotencyStore` (below). `TTL` (default 24 h) is how long a stored response lives and `LockTTL` (default 5 min) how long an unfinished request holds its key, so keep it above your longest request: a handler that runs longer can be started a second time. The in-memory store keeps at most `MaxEntries` (10000) keys and `MaxBytes` (32 MiB) of responses. When full it drops the oldest finished responses, never an unfinished claim, and answers 503 if only unfinished claims are left.
- If the store fails on `Begin`, the request gets a 503 and the handler does not run, because running it could repeat the work. Errors from `Finish` and `Release` are logged and the client still gets the real response.

### Writing a store

```go
type IdempotencyStore interface {
	Begin(ctx context.Context, key, fingerprint string, ttl time.Duration) (existing *IdempotencyEntry, claim string, err error)
	Finish(ctx context.Context, key, claim string, e IdempotencyEntry, ttl time.Duration) error
	Release(ctx context.Context, key, claim string) error
}
```

- `Begin` must be one atomic step (Redis `SET key value NX PX ttl`, a unique-key insert in SQL). If the key is free, store a pending entry with the fingerprint that lives for `ttl` (the `LockTTL`) and return `nil`, a non-empty `claim` token and no error. If it exists, return a copy of it and an empty claim. Two concurrent callers must never both get a claim.
- `Finish` replaces the pending entry with the final one (`Done` is set by the store) and gives it a new lifetime of `ttl` (the response `TTL`). `Release` deletes the key and must not fail for a key that doesn't exist. Both act only while `claim` still owns the key: once a claim has expired and someone else holds the key, a stale `Finish` or `Release` must do nothing, so an old request can't overwrite a newer result.
- The pending entry must expire by itself, so a process that crashes mid-request doesn't block the key for a day.
- Return an error only for store failures, never for "key exists". `Finish` and `Release` get a context that is not cancelled when the client disconnects.
- An entry holds the fingerprint, status, headers and body. Encode them however you like (JSON works, `Body` is base64 in it), and return copies so callers can't alter stored data.

`idempotencytest.Run` checks all of this against your implementation, including 64 concurrent `Begin` calls, both expiry rules and stale claims:

```go
func TestMyStore(t *testing.T) {
	idempotencytest.Run(t, func(t *testing.T) middleware.IdempotencyStore { return newRedisStore(t) })
}
```

Each call of the factory must return a store with no keys in it. The suite sleeps up to a few hundred milliseconds for the expiry cases.

## Pagination

```go
type ListInput struct {
	server.Page // limit (default 20, max 100) and cursor
}
type ListOutput struct {
	Body server.PageBody[User]
}

func listUsers(ctx context.Context, in *ListInput) (*ListOutput, error) {
	after := 0
	if in.Cursor != "" {
		if err := server.DecodeCursor(in.Cursor, &after); err != nil {
			return nil, server.InvalidCursor(err)
		}
	}
	rows := store.List(after, in.Limit+1) // one extra row tells us whether there is a next page
	return &ListOutput{Body: server.Paginate(rows, in.Limit, func(u User) any { return u.ID })}, nil
}
```

The response is `{"items": [...], "next_cursor": "..."}`, and `next_cursor` is absent on the last page. Cursors are opaque: clients must send them back unchanged. Generic bodies get a readable name in the spec, here `PageBodyUser`.

## Shutdown and readiness

```go
api.ShutdownDelay = 5 * time.Second // keep serving while the load balancer notices
server.Handle(api, "GET /readyz", web.ReadyFor(api, 2*time.Second, checks))
```

- When the context is done, `/readyz` answers 503 with `"status":"draining"` straight away. The server keeps serving for `ShutdownDelay`, then drains in-flight requests for `ShutdownTimeout`.
- HTTP timeouts: `ReadHeaderTimeout` 10 s, `ReadTimeout` 1 min (it covers the whole body, so raise it for large uploads), `IdleTimeout` 2 min, and `WriteTimeout` none, because streams and SSE can be long.

## Tracing

```go
type Tracer interface {
	Start(r *http.Request) (context.Context, Span)
}
type Span interface {
	End(SpanResult) // Route is the matched pattern such as "GET /users/{id}", or "unrouted"; Status is the final status
}
```

`api.Use(middleware.Trace(myTracer))` calls `Start` for every request, passes the returned context to the handler and the rest of the middleware, and calls `End` once when the request is over. Implement the two interfaces on top of whatever you use (OpenTelemetry, Datadog, your own logger), so the framework has no dependency:

```go
func (t otelTracer) Start(r *http.Request) (context.Context, middleware.Span) {
	ctx, span := t.tracer.Start(r.Context(), r.Method+" "+r.URL.Path)
	return ctx, otelSpan{span}
}

func (s otelSpan) End(res middleware.SpanResult) {
	s.span.SetName(res.Route)
	s.span.SetAttributes(attribute.Int("http.status_code", res.Status))
	s.span.End()
}
```

- The route is only known when the request ends, so name the span in `End`, not in `Start`.
- Return a nil span if you decide not to trace a request, and a nil context to keep the request's own. `Trace(nil)` panics at setup.
- A panic in a handler ends the span with status 500. `middleware.TracerFunc` turns a plain function into a `Tracer`.
- Put `Trace` first (after `RequestID`) so the span covers the other middleware. `Start` runs before routing, so it can read headers (for example `traceparent`) but not path parameters.

## Rate limiting

```go
api.Use(middleware.CORS(cfg), middleware.RateLimit(middleware.RateLimitConfig{Rate: 20, Burst: 40}))
```

A token bucket per client: `Rate` (a finite number above 0, otherwise it panics at setup) requests per second on average, up to `Burst` at once (default: `Rate` rounded up). Over the limit is a 429 problem with `Retry-After`.

- **Client key:** the connecting address. IPv6 clients are grouped by /64, because one user usually owns the whole prefix. Behind a proxy every client would share the proxy's address, so give `Key` a function that reads the address your proxy vouches for. Return `""` to skip limiting a request. An address with no host (such as `RemoteAddr` `":1234"`) is limited under the shared key `unknown`. The table holds at most `MaxKeys` clients: fully refilled ones are dropped first, and clients past the limit share one overflow bucket, so churning other addresses can't reset a limited client.
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

To serve a folder on disk, use `web.Dir`, not `os.DirFS`. `os.DirFS` follows symbolic links and Windows junctions, so a link inside the folder (for example from a linked package workspace) would serve files from outside it. `web.Dir` is rooted with `os.OpenRoot`, which refuses to leave the folder. It panics if the folder cannot be opened, and has a `Close` method for tests:

```go
server.Handle(api, "/", web.Static(web.Dir("./dist"), web.Config{SPA: true}))
```

Keep secrets out of the folder anyway: the dotfile filter is a safety net, not a place to keep `.env`.

`web.Static` never lists directories, hides dotfiles (`.env`, `.git`; `.well-known` is allowed), also hides Windows 8.3 short names such as `ENV~1`, which would otherwise reach a dotfile, always sends `index.html` with `no-cache`, and supports ranges. With `SPA: true`, page navigations (GET/HEAD, `Accept: text/html`, no file extension) to unknown paths get `index.html`, so `/users/42` survives a reload, while `fetch` calls to a mistyped `/api/x` and missing assets still get a 404. `Immutable` marks a prefix of content-hashed build output (Vite's `/assets/`) as cacheable for a year. It matches whole path segments, so `/assets` does not catch `/assets-config.json`. Every file is sent with `X-Content-Type-Options: nosniff`.

During development, forward everything else to the front-end dev server instead. WebSocket upgrades are proxied, so hot reload works:

```go
h, err := web.Proxy("http://localhost:5173")
server.Handle(api, "/", h)
```

Client-supplied `X-Forwarded-*` headers are replaced, and an unreachable upstream, or one that sends no response headers within 30 s, gives a 502 problem response. A request target that is not a plain path (such as `A:A:`) gets a 400. For a subtree, use `http.StripPrefix`, e.g. `server.Handle(api, "/static/", http.StripPrefix("/static", web.Static(fsys, web.Config{})))`. On a group, the prefix and middleware apply. Don't put `Timeout` on a group that proxies WebSockets.

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

The REST example does this with `go run ./example/rest openapi openapi.json` (no file name prints JSON). Commit the result and let CI fail when it is stale, so API changes show up in review. This is the step in `.github/workflows/build.yml`:

```bash
go run ./example/rest openapi example/rest/openapi.json
if [ -n "$(git status --porcelain -- example/rest/openapi.json)" ]; then git --no-pager diff -- example/rest/openapi.json; exit 1; fi
```

The YAML is written by a small built-in converter from the JSON, so there is still no dependency, and every string and ambiguous key (`"on"`, `"200"`, `"null"`) is quoted. The in-repo tests check the quoting rules, not full YAML parsing, because that needs a YAML library.

The spec feeds [takumi](https://github.com/swiftly-headquarters/takumi), which generates typed Go and TypeScript clients. CI generates a Go SDK from the example spec and vets it, and generates a TypeScript SDK, compiles it in strict mode and runs `tests/sdk/typescript/run.mjs` against the running example (JSON, a multipart upload, a catch-all path, bearer headers, 422 and 404 as errors). The Go SDK step:

```bash
go install github.com/swiftly-headquarters/takumi@v0.1.1
takumi -input example/rest/openapi.json -output ./sdk -lang go
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
go run ./example/rest           # http://localhost:8080/docs, REST API with uploads, events, health and the SPA
go run ./example/rest openapi     # print the spec (add a file name to write it, .yaml for YAML)
OFFLINE_DOCS=1 go run ./example/rest   # /docs rendered by the server, no CDN
SHUTDOWN_DELAY=5s go run ./example/rest # on SIGTERM, /readyz answers 503 "draining" for 5s before the server stops accepting
WEB_PROXY=http://localhost:5173 go run ./example/rest   # proxy "/" to a dev server instead of the embedded build
REQUEST_ID_PREFIX=ex go run ./example/rest              # custom request id generator: ex-<random>
TLS_CERT=cert.pem TLS_KEY=key.pem go run ./example/rest # serve HTTPS
go run ./example/grpc             # the shared API only, served over REST and gRPC on :8080
go run ./example/grpc proto       # print the .proto file (add a file name to write it)
```

## Development

```bash
go vet ./...
go test ./...
```

After changing the API, regenerate the committed spec: `go run ./example/rest openapi example/rest/openapi.json` and the same for `example/rest/openapi.yaml`.
