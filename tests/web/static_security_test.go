package web_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/swiftly-headquarters/minato/web"
)

func getStatic(h http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
	return rec
}

func TestDirDoesNotFollowLinksOutOfTheRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(root, "assets"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "assets", "app.js"), []byte("app"), 0o644)
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("OUTSIDE-SECRET"), 0o644)
	link := filepath.Join(root, "assets", "linked")
	if err := os.Symlink(outside, link); err != nil {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
			t.Skipf("cannot create a link here: %v %s", err, out)
		}
	}
	dir := web.Dir(root)
	t.Cleanup(func() { dir.Close() })
	h := web.Static(dir, web.Config{})
	if rec := getStatic(h, "/assets/app.js"); rec.Code != 200 || rec.Body.String() != "app" {
		t.Fatalf("regular files are served: %d %q", rec.Code, rec.Body)
	}
	if rec := getStatic(h, "/assets/linked/secret.txt"); rec.Code != 404 || strings.Contains(rec.Body.String(), "OUTSIDE-SECRET") {
		t.Fatalf("a link inside the root must not expose files outside it: %d %q", rec.Code, rec.Body)
	}
}

func TestDirOfAMissingFolderPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("web.Dir of a folder that does not exist must panic at setup")
		}
	}()
	web.Dir(filepath.Join(t.TempDir(), "nope"))
}

func TestNamesWithATildeAreStillServed(t *testing.T) {
	h := web.Static(fstest.MapFS{
		"vendors~main.js":  {Data: []byte("v")},
		"a~b.txt":          {Data: []byte("b")},
		"report~final.pdf": {Data: []byte("r")},
	}, web.Config{})
	for _, p := range []string{"/vendors~main.js", "/a~b.txt", "/report~final.pdf"} {
		if rec := getStatic(h, p); rec.Code != 200 {
			t.Fatalf("%s: %d", p, rec.Code)
		}
	}
}
