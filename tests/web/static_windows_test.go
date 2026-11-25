package web_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/swiftly-headquarters/minato/web"
)

func shortNameOf(t *testing.T, long string) string {
	p, _ := syscall.UTF16PtrFromString(long)
	buf := make([]uint16, 1024)
	n, err := syscall.GetShortPathName(p, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 {
		t.Skipf("GetShortPathName: %v", err)
	}
	s := filepath.Base(syscall.UTF16ToString(buf[:n]))
	if s == filepath.Base(long) {
		t.Skip("8.3 short names are disabled on this volume")
	}
	return s
}

func TestShortNamesDoNotBypassTheDotfileFilter(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, ".git"), 0o755)
	os.WriteFile(filepath.Join(root, ".git", "config"), []byte("GIT-SECRET"), 0o644)
	os.WriteFile(filepath.Join(root, ".env"), []byte("ENV-SECRET"), 0o644)
	dir := web.Dir(root)
	t.Cleanup(func() { dir.Close() })
	filesystems := map[string]fs.FS{"DirFS": os.DirFS(root), "Dir": dir}
	for _, c := range []struct{ long, suffix string }{{".env", ""}, {".git", "/config"}} {
		short := shortNameOf(t, filepath.Join(root, c.long))
		for label, fsys := range filesystems {
			rec := getStatic(web.Static(fsys, web.Config{}), "/"+short+c.suffix)
			if rec.Code != 404 || strings.Contains(rec.Body.String(), "SECRET") {
				t.Fatalf("%s: /%s%s exposed a dotfile through its 8.3 name: %d %q", label, short, c.suffix, rec.Code, rec.Body)
			}
		}
	}
}
