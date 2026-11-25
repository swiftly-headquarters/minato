package server_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swiftly-headquarters/minato/server"
)

type gItem struct {
	ID   int64    `json:"id"`
	Name string   `json:"name" minLength:"2"`
	Tags []string `json:"tags"`
}

type getItemIn struct {
	ID      int64  `path:"id"`
	Verbose bool   `query:"verbose"`
	Sid     string `cookie:"sid"`
}

type getItemOut struct {
	Rev  string `header:"X-Rev"`
	Body gItem
}

type createItemIn struct {
	Trace string `header:"X-Trace"`
	Body  gItem
}

type createItemOut struct {
	Body gItem
}

type echoIn struct {
	Word string `path:"word"`
}

type treeIn struct {
	Path string `path:"path"`
}

type blobOut struct {
	ContentType string `header:"Content-Type"`
	Body        []byte
}

type blobIn struct {
	ContentType string `header:"Content-Type"`
	Body        []byte
}

func TestGRPCRawBodies(t *testing.T) {
	srv := newGRPCEnv().start(t)
	res := grpcCall(t, srv, "GetBlobByWord", grpcJSON, []byte(`{"word":"x"}`))
	if res.status != "0" {
		t.Fatalf("%+v", res)
	}
	var out struct {
		ContentType string `json:"Content_Type"`
		Body        []byte `json:"body"`
	}
	json.Unmarshal(res.payload, &out)
	if out.ContentType != "text/plain" || string(out.Body) != "hi x" {
		t.Fatalf("a raw body is a bytes field: %s", res.payload)
	}
	res = grpcCall(t, srv, "GetBlobByWord", grpcProto, []byte("\x0a\x01x"))
	fields := parseWire(t, res.payload)
	if res.status != "0" || len(fields) != 2 || string(fields[0].b) != "text/plain" || string(fields[1].b) != "hi x" {
		t.Fatalf("%+v %x", res, res.payload)
	}
	res = grpcCall(t, srv, "PostBlob", grpcJSON, []byte(`{"Content_Type":"image/png","body":"AAEC"}`))
	if res.status != "0" || !strings.Contains(string(res.payload), `"name":"image/png:\u0000\u0001\u0002"`) {
		t.Fatalf("a raw request body is decoded from bytes and keeps its Content-Type: %+v %s", res, res.payload)
	}
}

func TestGRPCPathValuesAreEscapedIntoOneSegment(t *testing.T) {
	e := newGRPCEnv()
	srv := e.start(t)
	for word, want := range map[string]string{"a/b c": "a/b c", "100%": "100%", "x?y#z": "x?y#z", "é": "é", "..%2F": "..%2F"} {
		msg, _ := json.Marshal(map[string]string{"word": word})
		res := grpcCall(t, srv, "GetEchoByWord", grpcJSON, msg)
		var out struct{ Body gItem }
		json.Unmarshal(res.payload, &out)
		if res.status != "0" || out.Body.Name != want {
			t.Errorf("%q: %+v %s", word, res, res.payload)
		}
	}
	for _, word := range []string{"..", "."} {
		msg, _ := json.Marshal(map[string]string{"word": word})
		if res := grpcCall(t, srv, "GetEchoByWord", grpcJSON, msg); res.status == "0" {
			t.Errorf("%q must not reach a handler by path cleaning: %+v", word, res)
		}
	}
	res := grpcCall(t, srv, "GetTreeByPath", grpcJSON, []byte(`{"path":"x/y z/w"}`))
	if res.status != "0" || !strings.Contains(string(res.payload), `"name":"x/y z/w"`) {
		t.Errorf("a catch-all keeps its slashes: %+v %s", res, res.payload)
	}
	if res := grpcCall(t, srv, "GetEchoByWord", grpcJSON, []byte(`{"word":""}`)); res.status != "3" {
		t.Errorf("an empty path value: %+v", res)
	}
}

type seen struct {
	method, path, trace, cookie, auth, ctype, te, timeout, accept string
}

type grpcEnv struct {
	api  *server.API
	mu   sync.Mutex
	seen []seen
}

func (e *grpcEnv) last() seen {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.seen[len(e.seen)-1]
}

func newGRPCEnv() *grpcEnv {
	e := &grpcEnv{api: server.New("t", "1")}
	api := e.api
	api.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, _ := r.Cookie("sid")
			s := seen{method: r.Method, path: r.URL.Path, trace: r.Header.Get("X-Trace"), auth: r.Header.Get("Authorization"), ctype: r.Header.Get("Content-Type"), te: r.Header.Get("Te"), timeout: r.Header.Get("Grpc-Timeout"), accept: r.Header.Get("Accept")}
			if c != nil {
				s.cookie = c.Value
			}
			e.mu.Lock()
			e.seen = append(e.seen, s)
			e.mu.Unlock()
			next.ServeHTTP(w, r)
		})
	})
	api.AddScheme("key", server.Scheme{
		Spec: map[string]any{"type": "http", "scheme": "bearer"},
		Verify: func(ctx context.Context, r *http.Request, _ []string) (context.Context, error) {
			if r.Header.Get("Authorization") != "Bearer ok" {
				return nil, errors.New("no")
			}
			return ctx, nil
		},
	})
	server.Get(api, "/items/{id}", func(_ context.Context, in *getItemIn) (*getItemOut, error) {
		if in.ID == 404 {
			return nil, server.Error404("no such item")
		}
		out := &getItemOut{Rev: "r7"}
		out.Body = gItem{ID: in.ID, Name: "item-" + strconv.FormatInt(in.ID, 10), Tags: []string{}}
		if in.Verbose {
			out.Body.Tags = []string{"v"}
		}
		return out, nil
	})
	server.Post(api, "/items", func(_ context.Context, in *createItemIn) (*createItemOut, error) {
		return &createItemOut{Body: gItem{ID: 1, Name: in.Body.Name + in.Trace}}, nil
	}, func(o *server.Operation) { o.DefaultStatus = 201 })
	server.Get(api, "/echo/{word}", func(_ context.Context, in *echoIn) (*createItemOut, error) {
		return &createItemOut{Body: gItem{Name: in.Word}}, nil
	})
	server.Get(api, "/tree/{path...}", func(_ context.Context, in *treeIn) (*createItemOut, error) {
		return &createItemOut{Body: gItem{Name: in.Path}}, nil
	})
	server.Get(api, "/blob/{word}", func(_ context.Context, in *echoIn) (*blobOut, error) {
		return &blobOut{ContentType: "text/plain", Body: []byte("hi " + in.Word)}, nil
	})
	server.Post(api, "/blob", func(_ context.Context, in *blobIn) (*createItemOut, error) {
		return &createItemOut{Body: gItem{Name: in.ContentType + ":" + string(in.Body)}}, nil
	})
	server.Get(api, "/secret", func(_ context.Context, _ *empty) (*createItemOut, error) {
		return &createItemOut{Body: gItem{Name: "shh"}}, nil
	}, func(o *server.Operation) { o.Security = server.Secured("key") })
	server.Get(api, "/ctxcheck", func(ctx context.Context, _ *empty) (*empty, error) { return nil, ctx.Err() })
	server.Get(api, "/slow", func(ctx context.Context, _ *empty) (*empty, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return nil, nil
		}
	})
	server.Get(api, "/clock", func(_ context.Context, _ *empty) (*struct{ Body server.SSE }, error) {
		return &struct{ Body server.SSE }{func(ctx context.Context, send func(server.SSEEvent) error) error { return nil }}, nil
	})
	return e
}

func (e *grpcEnv) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(e.api)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

type grpcResult struct {
	proto   int
	status  string
	message string
	payload []byte
	header  http.Header
}

func frame(msg []byte, flag byte) []byte {
	f := make([]byte, 5, 5+len(msg))
	f[0] = flag
	binary.BigEndian.PutUint32(f[1:], uint32(len(msg)))
	return append(f, msg...)
}

func grpcRaw(t *testing.T, srv *httptest.Server, path, ct string, body []byte, hdr ...string) grpcResult {
	t.Helper()
	req, err := http.NewRequest("POST", srv.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Te", "trailers")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	res := grpcResult{proto: resp.ProtoMajor, header: resp.Header, status: resp.Trailer.Get("Grpc-Status"), message: resp.Trailer.Get("Grpc-Message")}
	if res.status == "" {
		res.status, res.message = resp.Header.Get("Grpc-Status"), resp.Header.Get("Grpc-Message")
	}
	res.message, _ = url.PathUnescape(res.message)
	if len(raw) >= 5 {
		n := binary.BigEndian.Uint32(raw[1:5])
		res.payload = raw[5 : 5+n]
	}
	return res
}

func grpcCall(t *testing.T, srv *httptest.Server, method, ct string, msg []byte, hdr ...string) grpcResult {
	t.Helper()
	return grpcRaw(t, srv, "/minato.API/"+method, ct, frame(msg, 0), hdr...)
}

const (
	grpcProto = "application/grpc"
	grpcJSON  = "application/grpc+json"
)

type wire struct {
	num, wt int
	v       uint64
	b       []byte
}

func parseWire(t *testing.T, b []byte) []wire {
	t.Helper()
	var out []wire
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			t.Fatalf("bad tag in %x", b)
		}
		b = b[n:]
		w := wire{num: int(tag >> 3), wt: int(tag & 7)}
		switch w.wt {
		case 0:
			w.v, n = binary.Uvarint(b)
			b = b[n:]
		case 2:
			l, n := binary.Uvarint(b)
			w.b, b = b[n:n+int(l)], b[n+int(l):]
		default:
			t.Fatalf("unexpected wire type %d", w.wt)
		}
		out = append(out, w)
	}
	return out
}

func TestGRPCJSONUnary(t *testing.T) {
	srv := newGRPCEnv().start(t)
	res := grpcCall(t, srv, "GetItemsById", grpcJSON, []byte(`{"id":5,"verbose":true}`))
	if res.proto != 2 || res.status != "0" || res.header.Get("Content-Type") != grpcJSON {
		t.Fatalf("%+v", res)
	}
	var got, want any
	json.Unmarshal(res.payload, &got)
	json.Unmarshal([]byte(`{"X_Rev":"r7","body":{"id":5,"name":"item-5","tags":["v"]}}`), &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s", res.payload)
	}
}

func TestGRPCJSONAcceptsProto3JSONConventions(t *testing.T) {
	srv := newGRPCEnv().start(t)
	res := grpcCall(t, srv, "GetItemsById", grpcJSON, []byte(`{"id":"5"}`))
	if res.status != "0" || !strings.Contains(string(res.payload), `"id":5`) {
		t.Fatalf("64-bit integers may be written as strings: %+v %s", res, res.payload)
	}
	res = grpcCall(t, srv, "PostItems", grpcJSON, []byte(`{"X_Trace":"-t","body":{"id":"7","name":"ab"}}`))
	if res.status != "0" || !strings.Contains(string(res.payload), `"name":"ab-t"`) {
		t.Fatalf("omitted zero values (here the tags list) count as zero, as in protobuf: %+v %s", res, res.payload)
	}
	res = grpcCall(t, srv, "PostItems", grpcJSON, []byte(`{"body":{"id":"abc","name":"ab"}}`))
	if res.status != "3" {
		t.Fatalf("a string that is not a number is still invalid: %+v", res)
	}
	res = grpcCall(t, srv, "PostItems", grpcJSON, []byte(`{"body":{"id":"NaN","name":"ab"}}`))
	if res.status != "3" {
		t.Fatalf("NaN is not a JSON number: %+v", res)
	}
}

func TestGRPCProtobufUnary(t *testing.T) {
	srv := newGRPCEnv().start(t)
	res := grpcCall(t, srv, "GetItemsById", grpcProto, []byte("\x08\x05\x10\x01"))
	if res.status != "0" || res.header.Get("Content-Type") != grpcProto {
		t.Fatalf("%+v", res)
	}
	top := parseWire(t, res.payload)
	if len(top) != 2 || top[0].num != 1 || string(top[0].b) != "r7" || top[1].num != 2 {
		t.Fatalf("response message: %x", res.payload)
	}
	body := parseWire(t, top[1].b)
	if len(body) != 3 || body[0].v != 5 || string(body[1].b) != "item-5" || string(body[2].b) != "v" {
		t.Fatalf("body message: %x", top[1].b)
	}
	if res := grpcCall(t, srv, "GetItemsById", "application/grpc+proto", []byte("\x08\x05")); res.status != "0" || res.header.Get("Content-Type") != "application/grpc+proto" {
		t.Fatalf("the +proto spelling is accepted and echoed: %+v", res)
	}
}

func TestGRPCStatusMapping(t *testing.T) {
	e := newGRPCEnv()
	srv := e.start(t)
	for name, c := range map[string]struct {
		method, ct, msg, status, mention string
	}{
		"handler error":      {"GetItemsById", grpcJSON, `{"id":404}`, "5", "no such item"},
		"missing path value": {"GetItemsById", grpcJSON, `{}`, "3", "id"},
		"validation":         {"PostItems", grpcJSON, `{"body":{"id":1,"name":"a","tags":[]}}`, "3", "body.name"},
		"bad json":           {"PostItems", grpcJSON, `{`, "3", "invalid request message"},
		"not an object":      {"PostItems", grpcJSON, `[]`, "3", "JSON object"},
		"unknown method":     {"Nope", grpcJSON, `{}`, "12", "unknown method"},
		"bad proto":          {"GetItemsById", grpcProto, "\x08", "3", "invalid request message"},
		"secured, no creds":  {"GetSecret", grpcJSON, `{}`, "16", ""},
	} {
		if res := grpcCall(t, srv, c.method, c.ct, []byte(c.msg)); res.status != c.status || !strings.Contains(res.message, c.mention) {
			t.Errorf("%s: %+v", name, res)
		}
	}
	if res := grpcRaw(t, srv, "/other.Service/GetItemsById", grpcJSON, frame([]byte(`{}`), 0)); res.status != "12" {
		t.Errorf("another service name: %+v", res)
	}
	if res := grpcCall(t, srv, "GetClock", grpcJSON, []byte(`{}`)); res.status != "12" {
		t.Errorf("streaming operations are not exposed: %+v", res)
	}
}

func TestGRPCMetadataBecomesHeadersAndParamsBecomeRequest(t *testing.T) {
	e := newGRPCEnv()
	srv := e.start(t)
	res := grpcCall(t, srv, "PostItems", grpcJSON, []byte(`{"X_Trace":"-t1","body":{"id":1,"name":"ab","tags":[]}}`), "Authorization", "Bearer ok")
	if res.status != "0" || !strings.Contains(string(res.payload), `"name":"ab-t1"`) {
		t.Fatalf("%+v %s", res, res.payload)
	}
	if got := e.last(); got.method != "POST" || got.path != "/items" || got.trace != "-t1" || got.auth != "Bearer ok" {
		t.Fatalf("middleware must see the request as a normal REST call: %+v", got)
	}
	grpcCall(t, srv, "GetItemsById", grpcJSON, []byte(`{"id":42,"sid":"s-9"}`))
	if got := e.last(); got.path != "/items/42" || got.cookie != "s-9" || got.method != "GET" {
		t.Fatalf("path and cookie parameters: %+v", got)
	}
}

func TestGRPCTransportHeadersDoNotLeakIntoTheRESTRequest(t *testing.T) {
	e := newGRPCEnv()
	srv := e.start(t)
	grpcCall(t, srv, "GetItemsById", grpcJSON, []byte(`{"id":1}`), "Grpc-Timeout", "5S", "Accept", "text/html")
	if got := e.last(); got.ctype != "" || got.te != "" || got.timeout != "" || got.accept != "application/json" {
		t.Fatalf("the handler chain sees a plain JSON REST request: %+v", got)
	}
	grpcCall(t, srv, "PostItems", grpcJSON, []byte(`{"body":{"id":1,"name":"ab","tags":[]}}`))
	if got := e.last(); got.ctype != "application/json" || got.te != "" {
		t.Fatalf("a body is sent as JSON: %+v", got)
	}
}

func TestGRPCSecurityIsEnforcedLikeREST(t *testing.T) {
	srv := newGRPCEnv().start(t)
	if res := grpcCall(t, srv, "GetSecret", grpcJSON, []byte(`{}`)); res.status != "16" {
		t.Fatalf("%+v", res)
	}
	if res := grpcCall(t, srv, "GetSecret", grpcJSON, []byte(`{}`), "Authorization", "Bearer wrong"); res.status != "16" {
		t.Fatalf("%+v", res)
	}
	if res := grpcCall(t, srv, "GetSecret", grpcJSON, []byte(`{}`), "Authorization", "Bearer ok"); res.status != "0" {
		t.Fatalf("%+v", res)
	}
}

func TestGRPCDeadline(t *testing.T) {
	srv := newGRPCEnv().start(t)
	start := time.Now()
	res := grpcCall(t, srv, "GetSlow", grpcJSON, []byte(`{}`), "Grpc-Timeout", "100m")
	if res.status != "4" || time.Since(start) > 3*time.Second {
		t.Fatalf("%+v after %v", res, time.Since(start))
	}
}

func TestGRPCCompressionAndLimits(t *testing.T) {
	e := newGRPCEnv()
	e.api.MaxBodyBytes = 200
	srv := e.start(t)
	msg := []byte(`{"id":5}`)
	var zb bytes.Buffer
	zw := gzip.NewWriter(&zb)
	zw.Write(msg)
	zw.Close()
	res := grpcRaw(t, srv, "/minato.API/GetItemsById", grpcJSON, frame(zb.Bytes(), 1), "Grpc-Encoding", "gzip")
	if res.status != "0" {
		t.Fatalf("a gzip message: %+v", res)
	}
	if res := grpcRaw(t, srv, "/minato.API/GetItemsById", grpcJSON, frame(zb.Bytes(), 1)); res.status != "13" {
		t.Fatalf("compressed flag without grpc-encoding: %+v", res)
	}
	if res := grpcRaw(t, srv, "/minato.API/GetItemsById", grpcJSON, frame(msg, 0), "Grpc-Encoding", "snappy"); res.status != "12" {
		t.Fatalf("unknown compression: %+v", res)
	}
	big := []byte(`{"id":5,"sid":"` + strings.Repeat("x", 300) + `"}`)
	if res := grpcCall(t, srv, "GetItemsById", grpcJSON, big); res.status != "8" {
		t.Fatalf("a message over MaxBodyBytes: %+v", res)
	}
	var bomb bytes.Buffer
	zw = gzip.NewWriter(&bomb)
	zw.Write(bytes.Repeat([]byte(" "), 100<<10))
	zw.Close()
	if res := grpcRaw(t, srv, "/minato.API/GetItemsById", grpcJSON, frame(bomb.Bytes(), 1), "Grpc-Encoding", "gzip"); res.status != "8" {
		t.Fatalf("a gzip bomb must be bounded by the limit after inflating: %+v", res)
	}
	if res := grpcRaw(t, srv, "/minato.API/GetItemsById", grpcJSON, append(frame(msg, 0), frame(msg, 0)...)); res.status != "3" {
		t.Fatalf("two messages in a unary call: %+v", res)
	}
	if res := grpcRaw(t, srv, "/minato.API/GetItemsById", grpcJSON, []byte{0, 0, 0}); res.status != "3" {
		t.Fatalf("a truncated prefix: %+v", res)
	}
	if res := grpcRaw(t, srv, "/minato.API/GetItemsById", grpcJSON, []byte{9, 0, 0, 0, 0}); res.status != "13" {
		t.Fatalf("an invalid flag: %+v", res)
	}
}

func TestGRPCOnlyForGRPCContentTypes(t *testing.T) {
	srv := newGRPCEnv().start(t)
	req, _ := http.NewRequest("POST", srv.URL+"/minato.API/PostItems", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/grpc-web+proto")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Grpc-Status") != "" || resp.StatusCode != 404 {
		t.Fatalf("grpc-web is not gRPC: %d %v", resp.StatusCode, resp.Header)
	}
	req, _ = http.NewRequest("POST", srv.URL+"/minato.API/PostItems", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/grpc+thrift")
	resp, _ = srv.Client().Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("an unknown gRPC codec is a 415: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", srv.URL+"/minato.API/PostItems", nil)
	req.Header.Set("Content-Type", grpcProto)
	resp, _ = srv.Client().Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("gRPC is POST only: %d", resp.StatusCode)
	}
}

func TestGRPCCanBeSwitchedOff(t *testing.T) {
	e := newGRPCEnv()
	e.api.DisableGRPC = true
	srv := e.start(t)
	req, _ := http.NewRequest("POST", srv.URL+"/minato.API/PostItems", strings.NewReader("{}"))
	req.Header.Set("Content-Type", grpcProto)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 || resp.Header.Get("Grpc-Status") != "" {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
}

func TestGRPCOverCleartextHTTP2(t *testing.T) {
	e := newGRPCEnv()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.api.Serve(ctx, ln) }()
	defer func() {
		stop()
		<-done
	}()
	tr := &http.Transport{Protocols: new(http.Protocols)}
	tr.Protocols.SetUnencryptedHTTP2(true)
	req, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/minato.API/GetItemsById", bytes.NewReader(frame([]byte(`{"id":3}`), 0)))
	req.Header.Set("Content-Type", grpcJSON)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.ProtoMajor != 2 || resp.Trailer.Get("Grpc-Status") != "0" {
		t.Fatalf("cleartext HTTP/2: proto %d, trailers %v", resp.ProtoMajor, resp.Trailer)
	}
	plain, err := http.Get("http://" + ln.Addr().String() + "/openapi.json")
	if err != nil || plain.StatusCode != 200 || plain.ProtoMajor != 1 {
		t.Fatalf("HTTP/1.1 keeps working on the same port: %v %v", plain, err)
	}
	plain.Body.Close()
}

func TestGRPCMethodNamesAreUnique(t *testing.T) {
	api := server.New("t", "1")
	noop := func(ctx context.Context, _ *empty) (*empty, error) { return nil, nil }
	server.Get(api, "/a", noop, func(o *server.Operation) { o.OperationID = "do-it" })
	mustPanic(t, "two operations that become the same gRPC method", func() {
		server.Get(api, "/b", noop, func(o *server.Operation) { o.OperationID = "do_it" })
	})
}

func TestGRPCUnsupportedProtobufShapesStillWorkAsJSON(t *testing.T) {
	api := server.New("t", "1")
	server.Post(api, "/grid", func(_ context.Context, in *pbNestedIO) (*pbNestedIO, error) { return in, nil })
	srv := httptest.NewUnstartedServer(api)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	if res := grpcCall(t, srv, "PostGrid", grpcJSON, []byte(`{"body":{"grid":[[1,2]]}}`)); res.status != "0" || !strings.Contains(string(res.payload), `[[1,2]]`) {
		t.Fatalf("%+v %s", res, res.payload)
	}
	if res := grpcCall(t, srv, "PostGrid", grpcProto, nil); res.status != "12" || !strings.Contains(res.message, "grpc+json") {
		t.Fatalf("%+v", res)
	}
	if strings.Contains(api.ProtoFile(), "rpc PostGrid") || !strings.Contains(api.ProtoFile(), "only available as application/grpc+json") {
		t.Fatalf("the proto file must not declare a method that protobuf cannot carry:\n%s", api.ProtoFile())
	}
}

func TestProtoFileSkipsStreamsAndKeepsRoutes(t *testing.T) {
	file := newGRPCEnv().api.ProtoFile()
	for _, want := range []string{"rpc GetItemsById(", "rpc PostItems(", "rpc GetSecret(", "rpc GetSlow(", "string X_Rev = 1 [json_name = \"X_Rev\"];", "message gItem {", "repeated string tags = 3;"} {
		if !strings.Contains(file, want) {
			t.Errorf("missing %q in:\n%s", want, file)
		}
	}
	if strings.Contains(file, "GetClock") {
		t.Errorf("an SSE route cannot be a unary rpc:\n%s", file)
	}
}

func TestGRPCTimeoutThatOverflowsIsIgnored(t *testing.T) {
	srv := newGRPCEnv().start(t)
	for _, v := range []string{"99999999H", "99999999M", "5S"} {
		if res := grpcCall(t, srv, "GetCtxcheck", grpcJSON, []byte(`{}`), "Grpc-Timeout", v); res.status != "0" {
			t.Errorf("grpc-timeout %s: %+v", v, res)
		}
	}
}
