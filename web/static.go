package web

import (
	"bytes"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"regexp"
	"runtime"
	"strings"

	"github.com/swiftly-headquarters/minato/server"
)

type Config struct {
	SPA bool

	Immutable string
}

func Static(fsys fs.FS, c Config) http.Handler {
	if c.Immutable != "" && !strings.HasSuffix(c.Immutable, "/") {
		c.Immutable += "/"
	}
	return &static{fsys, c}
}

type static struct {
	fs fs.FS
	c  Config
}

func (s *static) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		server.WriteError(w, server.NewError(http.StatusMethodNotAllowed, ""))
		return
	}
	clean := path.Clean("/" + r.URL.Path)
	name := strings.TrimPrefix(clean, "/")
	if name == "" {
		name = "."
	}
	if hidden(name) {
		notFound(w)
		return
	}
	info, err := fs.Stat(s.fs, name)
	if err == nil && info.IsDir() {
		if name != "." && !strings.HasSuffix(r.URL.Path, "/") {

			loc := path.Base(clean) + "/"
			if r.URL.RawQuery != "" {
				loc += "?" + r.URL.RawQuery
			}
			w.Header().Set("Location", loc)
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
		name = path.Join(name, "index.html")
		info, err = fs.Stat(s.fs, name)
	}
	if err != nil || info.IsDir() {
		if s.c.SPA && strings.Contains(r.Header.Get("Accept"), "text/html") && path.Ext(clean) == "" {
			s.serve(w, r, "index.html")
			return
		}
		notFound(w)
		return
	}
	s.serve(w, r, name)
}

func (s *static) serve(w http.ResponseWriter, r *http.Request, name string) {
	f, err := s.fs.Open(name)
	if err != nil {
		notFound(w)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		notFound(w)
		return
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		b, err := io.ReadAll(f)
		if err != nil {
			server.WriteError(w, err)
			return
		}
		rs = bytes.NewReader(b)
	}
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	switch {
	case strings.EqualFold(path.Base(name), "index.html"):
		h.Set("Cache-Control", "no-cache")
	case s.c.Immutable != "" && strings.HasPrefix("/"+name, s.c.Immutable):
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.ServeContent(w, r, name, info.ModTime(), rs)
}

var shortNameRE = regexp.MustCompile(`^[^.]{1,6}~[0-9]{1,6}(\.[^.]{0,3})?$`)

func hidden(name string) bool {
	for seg := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(seg, ".") && seg != "." && seg != ".well-known" {
			return true
		}
		if runtime.GOOS == "windows" && shortNameRE.MatchString(seg) {
			return true
		}
	}
	return false
}

type DirFS struct {
	fs.FS
	root *os.Root
}

func Dir(dir string) *DirFS {
	root, err := os.OpenRoot(dir)
	if err != nil {
		panic("minato: web.Dir: " + err.Error())
	}
	return &DirFS{root.FS(), root}
}

func (d *DirFS) Close() error { return d.root.Close() }

func notFound(w http.ResponseWriter) { server.WriteError(w, server.Error404("not found")) }
