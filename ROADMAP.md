# Minato roadmap

Status: nothing here is started. These items were written from the state at the end of the last session and were not re-checked against the code.

## 1. Raw TCP transport (requested, not started)

Goal: the same handlers (`func(ctx, *In) (*Out, error)`), registered once, are reachable over HTTP and over a plain TCP socket. Nothing in route code changes.

### 1.0 Check first: is HTTP/2 without TLS enough?

If what you need is one long-lived connection carrying many concurrent requests and long-running streams, Go 1.24+ can serve unencrypted HTTP/2 (h2c) from the same `http.Server`:

```go
var p http.Protocols
p.SetHTTP1(true)
p.SetUnencryptedHTTP2(true)
srv.Protocols = &p
```

That gives multiplexing, cancellation, flow control and SSE streams with no new protocol, and every HTTP client library already speaks it. It would be one option on `api.Run`. Build the custom protocol below only if the clients can't speak HTTP at all (e.g. a game-server plugin with only a raw socket) or need something smaller.

### 1.1 Approach: TCP in front of the existing handler

The earlier plan here was to move binding into a transport-neutral request type first. Don't start there. Instead, the TCP server turns each request frame into an `*http.Request`, runs it through `api.ServeHTTP` with an in-memory `ResponseWriter`, and sends what was written back as frames.

That reuses everything unchanged: routing and path params, groups, all middleware (auth, logger, timeout), binding, validation, defaults, forms, problem-document errors, panic recovery, `Accept` negotiation, streaming and SSE. The OpenAPI spec also describes the TCP API, because method, path, headers, bodies and statuses are the same.

The cost is that HTTP concepts (method, path, headers, status codes) appear on the wire. That's acceptable, and it means a request means the same thing on both transports.

Because it only needs an `http.Handler`, it lives in its own package, `tcp`, and doesn't touch `server` internals (except the small export in 1.4 for operation-ID routing).

Do the transport-neutral binding refactor only if a later protocol can't carry method, path and headers at all (e.g. a binary schema format).

### 1.2 Wire protocol

**Handshake.** The client sends 5 bytes: `MNTO` plus version `0x01`. The server answers with the same 5 bytes, or closes the connection if the magic or version is wrong. Any incompatible protocol change bumps the version byte. Handshake deadline: 10s.

**Frame layout** (all integers big-endian):

| Field     | Size    | Meaning                                                |
| --------- | ------- | ------------------------------------------------------ |
| length    | uint32  | number of bytes after this field (type + flags + id + payload) |
| type      | uint8   | frame type, see below                                  |
| flags     | uint8   | bit 0 = `END`: last frame of this stream in this direction |
| stream id | uint32  | chosen by the client, non-zero; 0 = connection-level frame |
| payload   | rest    | depends on the type                                    |

**Frame types:**

| Type | Name     | Stream | Payload |
| ---- | -------- | ------ | ------- |
| 0x00 | HELLO    | 0      | JSON `{"headers":{...}}`, headers applied to every request on the connection (phase 2) |
| 0x01 | REQUEST  | n      | JSON request header, see below |
| 0x02 | RESPONSE | n      | JSON response header, see below |
| 0x03 | DATA     | n      | raw body bytes |
| 0x04 | CANCEL   | n      | empty; client gives up on stream n |
| 0x05 | PING     | 0      | 8 opaque bytes |
| 0x06 | PONG     | 0      | the 8 bytes from the PING |
| 0x07 | GOAWAY   | 0      | JSON `{"last_stream":N,"reason":"shutdown"}` |

An unknown type, a malformed frame or a frame over `MaxFrameBytes` is a protocol error: the server sends GOAWAY with the reason and closes the connection. Being strict now keeps the version byte meaningful later.

**A request** is a REQUEST frame, then zero or more DATA frames, with `END` on the last one (or on the REQUEST itself when there's no body):

```
REQUEST  id=7       {"method":"POST","path":"/users?dry=true","headers":{"Authorization":["Bearer abc"],"Content-Type":["application/json"]}}
DATA     id=7 END   {"name":"Ada","email":"ada@example.com"}
```

The body travels as raw bytes in DATA frames, not inside the JSON header, so forms, uploads and `[]byte` bodies work without double encoding.

**A response** mirrors it:

```
RESPONSE id=7       {"status":201,"headers":{"Content-Type":["application/json"],"Location":["/users/1"]}}
DATA     id=7 END   {"id":1,"name":"Ada","email":"ada@example.com","role":"member"}
```

Application errors (404, 422, 500 ...) are ordinary responses with a `application/problem+json` body, byte-for-byte what HTTP sends. Only protocol errors use GOAWAY.

**Multiplexing.** Many streams can be in flight on one connection. Responses may finish in any order, and frames of different streams interleave. A stream id may be reused once both directions have ended.

**Streaming responses** (`io.Reader` and `server.SSE` bodies) are a RESPONSE frame followed by a DATA frame per flush, until `END`. An SSE stream is how a client subscribes to server events: send `GET /events` and keep reading DATA frames.

### 1.3 Routing

**Primary: method + path, exactly as on HTTP.** The REQUEST's `path` (with query string) goes through the same `http.ServeMux`, so patterns, `{id}`, `{path...}`, groups, prefixes, the `/` catch-all and "most specific pattern wins" all behave identically, and `r.PathValue` works. An unknown path gets the mux's 404 and a wrong method its 405 (both plain text today, same as HTTP).

**Optional (phase 2): by operation ID**, for RPC-style clients that don't want to build paths:

```
REQUEST  id=9 END   {"op":"get-users-by-id","params":{"id":"42"},"query":{"expand":["roles"]},"headers":{}}
```

The server looks up the operation's method and pattern, fills `{id}` with the URL-escaped value (for a catch-all `{path...}`, escape each segment), appends the query, and then routes the result normally, so middleware and validation see an ordinary request.

- Unknown op: 404. Missing path param: 400 naming it.
- `path` and `op` in the same frame: protocol error.
- Needs a small export from `server`, e.g. `api.Lookup(opID) (method, pattern string, ok bool)`, filled in `Register`. It can't be derived from `Spec.Paths`, because the spec drops the `...` of catch-all params.

Why path routing is primary and op IDs are only an alias: operation IDs default from the path, so renaming a path renames the op anyway, and `server.Handle` mounts (static files, proxy) have no op ID.

**What is reachable:** everything the HTTP side serves, including `/openapi.json`, `/docs` and `server.Handle` mounts. If TCP should expose only typed operations, add a `tcp.Config.Allow func(*http.Request) bool` filter (open question below).

### 1.4 How a request is built

- `Method`, `URL` (path + query), `Header` (canonicalized with `http.Header.Add`), `Host`: the listener address.
- `Body`: the buffered DATA bytes, with `ContentLength` set.
- `RemoteAddr`: `conn.RemoteAddr().String()`, so the logger and any rate limiting see the real peer.
- Context: one per stream, child of the connection's context. It is cancelled by a CANCEL frame, by the connection closing, or by the shutdown deadline.
- A context marker, e.g. `tcp.FromContext(ctx) bool`, so middleware can tell the transports apart. Don't fake `r.Proto`.
- Headers from HELLO (phase 2) are applied first; per-request headers override them. This gives "authenticate once per connection" without changing auth middleware.

**The in-memory `ResponseWriter`** must implement `Header`, `Write`, `WriteHeader` and `Flush`.

- `Flush` is required: `server.SSE`'s `send` returns the flush error, so without it every SSE route fails over TCP.
- The first `WriteHeader`/`Write` emits the RESPONSE frame. Writes are buffered and sent as a DATA frame on `Flush` or when the buffer reaches `MaxFrameBytes`. When the handler returns, the rest goes out with `END`.
- `Hijack` is not supported, so a WebSocket upgrade through `web.Proxy` fails over TCP. That's fine.

### 1.5 Concurrency and limits

- Per connection: one reader goroutine, one goroutine per stream, and one writer goroutine draining a bounded queue, so frames from concurrent streams never interleave mid-frame.
- `MaxStreams` (default 100 per connection): a REQUEST over the limit gets an immediate 503 problem response on that stream.
- `MaxFrameBytes` (default 1 MiB): a bigger frame is a protocol error.
- `MaxRequestBytes` (default 16 MiB): request bodies are buffered before dispatch; past this the stream gets a 413 and further DATA is discarded. The operation's own `MaxBodyBytes` still applies inside binding.
- `WriteTimeout` (default 30s): if the writer queue stays full that long (a client that doesn't read), close the connection, which cancels all its streams.
- `IdleTimeout` (default 2 min with no open streams and no frames): GOAWAY and close. Clients send PING more often than this to keep a connection open.
- `MaxConns` (phase 2, 0 = unlimited).

shortcut: no flow control and no request-body streaming. Uploads are buffered up to `MaxRequestBytes`. Add per-stream windows (like HTTP/2) only if large uploads over TCP become a real need.

### 1.6 Lifecycle and API

```go
type Config struct {
	MaxFrameBytes   int
	MaxRequestBytes int64
	MaxStreams      int
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration // how long in-flight streams get after ctx is done
}

err := tcp.Run(ctx, ":9000", api, tcp.Config{})    // like api.Run
err := tcp.Serve(ctx, ln, api, tcp.Config{})       // your own listener: TLS via tls.NewListener, unix sockets, tests
```

On `ctx` done: stop accepting, send GOAWAY with the last accepted stream id on every connection, let in-flight streams finish for up to `ShutdownTimeout`, then close. REQUEST frames after GOAWAY are ignored. Run it next to `api.Run` in a second goroutine sharing the same signal context; the example should show that.

### 1.7 Go client

`tcp.Dial(ctx, addr, cfg) (*tcp.Client, error)`, where `*tcp.Client` implements `http.RoundTripper`.

- `&http.Client{Transport: c}` then works over TCP, and so does any Go SDK built on `net/http`, including a takumi-generated one, without changes.
- The response body streams DATA frames as they arrive. Closing it early sends CANCEL.
- No automatic reconnect in v1: the caller redials.

Non-Go clients get the protocol description above plus a file of test vectors (handshake, one request, one response, one GOAWAY as hex).

### 1.8 OpenAPI

No change: same paths, bodies and statuses. Document the TCP port and protocol version in the README. Add an `x-transports` extension per operation only if routes ever get restricted to one transport.

### 1.9 Tests to write

- Handshake: wrong magic or version closes the connection.
- Typed route round trip: path params, query, headers reaching middleware (an auth middleware's 401 comes back as a RESPONSE).
- The 422 body for invalid input is identical over TCP and HTTP (compare bytes).
- Concurrent streams finish out of order (slow and fast handler on one connection).
- SSE over TCP: the first event arrives before the handler returns.
- CANCEL cancels the handler's ctx; closing the connection cancels all streams.
- Limits: `MaxStreams` gives 503, `MaxFrameBytes` gives GOAWAY, `MaxRequestBytes` gives 413.
- Graceful shutdown: in-flight streams finish, GOAWAY is sent, `Run` returns nil.
- A panic in a handler gives a 500 frame and the connection stays usable.
- Fuzz test for the frame parser (`go test -fuzz`).
- `tcp.Client` as an `http.Client` transport against a real `tcp.Serve`.

### 1.10 Phases

1. Protocol, server, path routing, buffered requests, streamed responses, CANCEL, PING, GOAWAY, Go client, tests.
2. Operation-ID routing, HELLO connection headers, `MaxConns`, `Allow` filter.
3. Only if needed: transport-neutral binding, for a protocol that can't carry method, path and headers.

### 1.11 Open questions

- Who are the TCP clients: a Go service, a game-server plugin (C++, Lua, C#), something embedded? This decides whether JSON frame headers are acceptable or need to be binary.
- Is h2c (1.0) enough, making the custom protocol unnecessary?
- Authenticate per connection (HELLO) or per request?
- Should TCP expose `/docs`, static files and the proxy, or only typed operations?
- Is server push outside any request needed, or is a long-lived SSE stream enough?

## 2. Gaps in what exists

- **Automatic 422 in the spec.** Validation 422s are only listed if you add them to `Operation.Errors` by hand.
- **Custom validators.** Cross-field checks (e.g. `end > start`) need an optional `Validate(ctx) []error` hook on input types. Missing tags: `exclusiveMinimum`, `multipleOf`, `uniqueItems`.
- **PATCH semantics.** No way to tell "field omitted" from "field set to null". `server.Optional[T]` and `Nullable[T]` types would fix it and need the `Schemer` hook.
- **Request body decoders.** `api.Formats` only encodes responses. The request-side counterpart would pick a decoder by `Content-Type`.
- **Multiple response types per route.** E.g. a 200 and a 202 with different bodies. Each route documents one success type today.
- **Offline docs.** `/docs` loads Stoplight from a CDN and breaks without internet. Embed the assets, or make the docs handler swappable.

## 3. Production basics (as `middleware` additions)

- **Request ID**, with `slog` context propagation. Makes the logger much more useful.
- **Bearer auth helper.** `Security` is documentation only. A `middleware.Bearer(verify func(token string) (context.Context, error))` gives a standard way to enforce it. Most users will write this themselves, so it's the most likely candidate for a built-in.
- **Rate limit.** Simple in-memory token bucket per IP or key. Only worth it for a single instance.
- **Health and metrics.** `/healthz` and `/readyz` helpers, and a Prometheus text endpoint (request count and latency). Can be written without a dependency.
- **ETag and conditional requests** for JSON responses (`If-None-Match` returns 304).

## 4. Developer experience

- **`/openapi.yaml`**, and an `openapi` command that writes the spec to a file.
- **Spec drift check in CI.** Regenerate the spec and fail if it differs from a committed `openapi.json`.
- **Client generation with takumi.** takumi already consumes OpenAPI. A CI step that generates a Go or TypeScript SDK from minato's spec would show the two fit together.
- **`servertest` package.** A typed in-process client, so tests don't build `httptest` requests by hand.
- **TLS and unix sockets in `Run`**, plus start and stop hooks.

## Not planned

- **Typed WebSockets.** The standard library has none, so this would force a dependency. The proxy already covers dev hot reload.
- **DI containers and ORMs.** Belong in app code.

## Suggested order

1. Raw TCP phase 1 (section 1), after deciding whether h2c (1.0) is enough.
2. Validator hook and `Optional[T]` (section 2).
3. Request ID and bearer helper (section 3).
4. Spec drift check and takumi CI step (section 4).

## Risks

Section 2 changes the public API: settle its shape before the first tagged release. Section 1 no longer touches binding, but its wire format becomes a contract as soon as a non-Go client exists: keep the version byte and settle the frame layout before then.
