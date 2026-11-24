package web_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/swiftly-headquarters/minato/server"
	"github.com/swiftly-headquarters/minato/web"
)

func TestReadyForFlipsBeforeShutdown(t *testing.T) {
	api := server.New("t", "1")
	api.ShutdownDelay = time.Second
	server.Handle(api, "GET /readyz", web.ReadyFor(api, time.Second, nil))
	server.Get(api, "/ping", func(ctx context.Context, _ *struct{}) (*struct{ Body string }, error) {
		return &struct{ Body string }{"pong"}, nil
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- api.Serve(ctx, ln) }()
	base := "http://" + ln.Addr().String()

	status, body := httpGet(t, base+"/readyz")
	if status != 200 || !strings.Contains(body, `"ok"`) {
		t.Fatalf("before shutdown: %d %s", status, body)
	}

	cancel()
	deadline := time.Now().Add(900 * time.Millisecond)
	for {
		status, body = httpGet(t, base+"/readyz")
		if status == 503 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("readiness never flipped during the shutdown delay: %d %s", status, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(body, "draining") || !api.Draining() {
		t.Fatalf("draining body: %s", body)
	}
	if status, _ := httpGet(t, base+"/ping"); status != 200 {
		t.Fatalf("the server keeps serving while it drains: %d", status)
	}
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}

func TestReadyForWithoutShutdownBehavesLikeReady(t *testing.T) {
	api := server.New("t", "1")
	h := web.ReadyFor(api, time.Second, map[string]web.Check{
		"db": func(context.Context) error { return nil },
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"db":"ok"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}
