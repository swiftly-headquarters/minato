package server_test

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swiftly-headquarters/minato/server"
)

type tricky struct {
	On    bool    `json:"on"`
	Y     bool    `json:"y"`
	Null  string  `json:"Null" enum:"yes,No,true,null,~,1.5,a: b,#c,  lead"`
	Num   int     `json:"200" doc:"line1\nline2"`
	Space string  `json:"a b"`
	Any   any     `json:"any"`
	U     uint    `json:"u"`
	F     float64 `json:"f" default:"1.5"`
	B     bool    `json:"b" default:"true"`
}

func TestSpecYAML(t *testing.T) {
	api := server.New("Tricky", "1")
	api.Spec.Info.Description = "Tom & \"Jerry\" <ok> — ü\ttab"
	api.BearerAuth("bearer")
	server.Post(api, "/t", func(ctx context.Context, in *struct{ Body tricky }) (*empty, error) { return nil, nil },
		func(o *server.Operation) { o.Security = server.Secured("bearer") })

	rec := call(api, "GET", "/openapi.yaml", "")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/yaml" {
		t.Fatalf("yaml endpoint: %d %v", rec.Code, rec.Header())
	}
	doc := rec.Body.String()
	if !strings.HasPrefix(doc, "openapi: \"3.1.0\"\n") || !strings.HasSuffix(doc, "\n") {
		t.Fatalf("document must start with the version and end with a newline:\n%.80s", doc)
	}
	for _, want := range []string{
		`  description: "Tom & \"Jerry\" <ok> — ü\ttab"`,
		`"on":`, `"y":`, `"Null":`, `"200":`, `"a b":`, `any: {}`,
		`- "yes"`, `- "No"`, `- "true"`, `- "null"`, `- "~"`, `- "1.5"`, `- "a: b"`, `- "#c"`, `- "  lead"`,
		`description: "line1\nline2"`, `minimum: 0`, `default: 1.5`, `default: true`,
		`- bearer: []`, `scheme: "bearer"`, `"$ref": "#/components/schemas/ErrorModel"`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("yaml lacks %s", want)
		}
	}
	if regexp.MustCompile(`(?m)^\s+(on|y|Null|200):`).MatchString(doc) {
		t.Error("keys that YAML reads as booleans, null or numbers must be quoted")
	}
}

func TestSpecEndpointsAndWriteSpec(t *testing.T) {
	api := server.New("t", "1")
	server.Get(api, "/x", noop)

	wantJSON, err := api.SpecJSON()
	if err != nil {
		t.Fatal(err)
	}
	wantYAML, err := api.SpecYAML()
	if err != nil {
		t.Fatal(err)
	}
	if rec := call(api, "GET", "/openapi.json", ""); rec.Body.String() != string(wantJSON) || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("/openapi.json differs from SpecJSON: %v", rec.Header())
	}
	if rec := call(api, "GET", "/openapi.yaml", ""); rec.Body.String() != string(wantYAML) {
		t.Error("/openapi.yaml differs from SpecYAML")
	}

	dir := t.TempDir()
	for name, want := range map[string][]byte{"spec.json": wantJSON, "spec.yaml": wantYAML, "spec.yml": wantYAML, "spec.txt": wantJSON} {
		path := filepath.Join(dir, name)
		if err := api.WriteSpec(path); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got, _ := os.ReadFile(path); string(got) != string(want) {
			t.Errorf("%s: wrong content", name)
		}
	}
	if err := api.WriteSpec(filepath.Join(dir, "missing", "spec.json")); err == nil {
		t.Error("an unwritable path must return an error")
	}
}

func TestServeTLSAndHooks(t *testing.T) {
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	defer ts.Close()

	api := server.New("t", "1")
	var mu sync.Mutex
	var events []string
	note := func(e string) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}
	started := make(chan net.Addr, 1)
	api.TLS = &tls.Config{Certificates: ts.TLS.Certificates}
	api.OnStart = func(a net.Addr) { note("start"); started <- a }
	api.OnStop = func(ctx context.Context) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("OnStop context must carry the shutdown deadline")
		}
		note("stop")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	server.Get(api, "/slow", func(ctx context.Context, _ *empty) (*struct{ Body string }, error) {
		close(entered)
		<-release
		note("handler done")
		return &struct{ Body string }{"ok"}, nil
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- api.Serve(ctx, ln) }()

	var addr net.Addr
	select {
	case addr = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("OnStart never ran")
	}
	if addr.String() != ln.Addr().String() {
		t.Errorf("OnStart got %s, want the bound address %s", addr, ln.Addr())
	}

	type result struct {
		code int
		tls  bool
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := ts.Client().Get("https://" + addr.String() + "/slow")
		if err != nil {
			got <- result{err: err}
			return
		}
		resp.Body.Close()
		got <- result{code: resp.StatusCode, tls: resp.TLS != nil}
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		t.Fatalf("Serve returned (%v) with a request in flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if r := <-got; r.err != nil || r.code != 200 || !r.tls {
		t.Fatalf("in-flight request over TLS: %+v", r)
	}
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(events, []string{"start", "handler done", "stop"}) {
		t.Errorf("hook order: %v", events)
	}
}

func TestServeTLSWithoutCertificate(t *testing.T) {
	api := server.New("t", "1")
	api.TLS = &tls.Config{}
	stopped := false
	api.OnStop = func(context.Context) { stopped = true }
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	res := make(chan error, 1)
	go func() { res <- api.Serve(context.Background(), ln) }()
	select {
	case err := <-res:
		if err == nil || !stopped {
			t.Fatalf("a TLS config without a certificate must fail and still run OnStop: err=%v stopped=%v", err, stopped)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve hung instead of failing")
	}
}

func unixClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
}

func getEventually(t *testing.T, c *http.Client, url string) *http.Response {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := c.Get(url)
		if err == nil {
			return resp
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never answered: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runUnix(t *testing.T, path string) (cancel context.CancelFunc, done chan error) {
	t.Helper()
	api := server.New("t", "1")
	server.Get(api, "/ping", noop)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done = make(chan error, 1)
	go func() { done <- api.Run(ctx, "unix:"+path) }()
	return cancel, done
}

func runResult(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run kept serving instead of failing")
		return nil
	}
}

func TestRunUnixSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	cancel, done := runUnix(t, path)
	resp := getEventually(t, unixClient(path), "http://unix/ping")
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("ping over the socket: %d", resp.StatusCode)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Lstat(path); err == nil {
		t.Error("the socket file must be removed on shutdown")
	}
}

func TestRunUnixSocketReplacesStaleFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	old, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	old.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("setup: a crashed server leaves its socket behind: %v", err)
	}
	cancel, done := runUnix(t, path)
	getEventually(t, unixClient(path), "http://unix/ping").Body.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunUnixSocketRefusesToTakeOver(t *testing.T) {
	dir := t.TempDir()

	live := filepath.Join(dir, "live.sock")
	ln, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, done := runUnix(t, live)
	if err := runResult(t, done); err == nil {
		t.Error("a socket with a live server on it must not be replaced")
	}
	if c, err := net.Dial("unix", live); err != nil {
		t.Errorf("the live server lost its socket: %v", err)
	} else {
		c.Close()
	}

	file := filepath.Join(dir, "precious.txt")
	if err := os.WriteFile(file, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, done = runUnix(t, file)
	if err := runResult(t, done); err == nil {
		t.Error("a regular file must not be treated as a stale socket")
	}
	if b, _ := os.ReadFile(file); string(b) != "data" {
		t.Errorf("the regular file was touched: %q", b)
	}
}
