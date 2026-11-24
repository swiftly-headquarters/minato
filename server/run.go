package server

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func (a *API) Run(ctx context.Context, addr string) error {
	ln, err := listen(addr)
	if err != nil {
		return err
	}
	return a.Serve(ctx, ln)
}

func (a *API) Serve(ctx context.Context, ln net.Listener) error {
	if a.OnStart != nil {
		a.OnStart(ln.Addr())
	}
	if a.OnStop != nil {
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), a.ShutdownTimeout)
			defer cancel()
			a.OnStop(sctx)
		}()
	}
	srv := &http.Server{Handler: a, TLSConfig: a.TLS, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		if a.TLS != nil {
			errc <- srv.ServeTLS(ln, "", "")
			return
		}
		errc <- srv.Serve(ln)
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), a.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		srv.Close()
		return err
	}
	return nil
}

func listen(addr string) (net.Listener, error) {
	path, isUnix := strings.CutPrefix(addr, "unix:")
	if !isUnix {
		return net.Listen("tcp", addr)
	}
	ln, err := net.Listen("unix", path)
	if err == nil {
		return ln, nil
	}
	if c, derr := net.Dial("unix", path); derr == nil {
		c.Close()
		return nil, err
	}
	if fi, serr := os.Lstat(path); serr != nil || fi.Mode()&os.ModeSocket == 0 || os.Remove(path) != nil {
		return nil, err
	}
	return net.Listen("unix", path)
}
