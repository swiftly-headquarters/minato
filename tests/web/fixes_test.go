package web_test

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/swiftly-headquarters/minato/web"
)

func TestImmutablePrefixStopsAtAPathBoundary(t *testing.T) {
	const immutable = "public, max-age=31536000, immutable"
	h := web.Static(fstest.MapFS{
		"assets/app.js":      {Data: []byte("a")},
		"assets-config.json": {Data: []byte("c")},
	}, web.Config{Immutable: "/assets"})
	if got := getStatic(h, "/assets/app.js").Header().Get("Cache-Control"); got != immutable {
		t.Fatalf("files under the prefix are immutable: %q", got)
	}
	if got := getStatic(h, "/assets-config.json").Header().Get("Cache-Control"); got == immutable {
		t.Fatal("a sibling that only shares the prefix text must not be cached for a year")
	}
}

func TestStaticFilesSendNoSniff(t *testing.T) {
	h := web.Static(fstest.MapFS{"a.txt": {Data: []byte("x")}}, web.Config{})
	if got := getStatic(h, "/a.txt").Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("static files must not be sniffed: %q", got)
	}
}

func TestProxyRejectsOpaqueRequestTargets(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.RequestURI)
	}))
	defer up.Close()
	p, err := web.Proxy(up.URL + "/base")
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(p)
	defer front.Close()
	conn, err := net.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET A:A: HTTP/1.1\r\nHost: example\r\nConnection: close\r\n\r\n")
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "HTTP/1.1 400") {
		t.Fatalf("an opaque request target must not be forwarded: %q %v", line, err)
	}
}
