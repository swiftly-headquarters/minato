package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
)

var (
	readerType     = reflect.TypeFor[io.Reader]()
	readCloserType = reflect.TypeFor[io.ReadCloser]()
	sseType        = reflect.TypeFor[SSE]()
)

type SSEEvent struct {
	ID    string
	Event string
	Data  any
}

type SSE func(ctx context.Context, send func(SSEEvent) error) error

type flushWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		err = f.rc.Flush()
		if errors.Is(err, http.ErrNotSupported) {
			err = nil
		}
	}
	return n, err
}

func writeStream(w http.ResponseWriter, r *http.Request, bv reflect.Value, status int) {
	rd, _ := bv.Interface().(io.Reader)
	if rd == nil {
		w.WriteHeader(status)
		return
	}
	if c, ok := rd.(io.Closer); ok {
		defer c.Close()
	}
	setDefault(w, "Content-Type", "application/octet-stream")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(flushWriter{w, http.NewResponseController(w)}, rd); err != nil {
		slog.Warn("minato: stream ended early", "err", err, "path", r.URL.Path)
	}
}

func writeSSE(w http.ResponseWriter, r *http.Request, bv reflect.Value, status int) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(status)
	rc := http.NewResponseController(w)
	rc.Flush()
	fn, _ := bv.Interface().(SSE)
	if fn == nil || r.Method == http.MethodHead {
		return
	}
	send := func(e SSEEvent) error {
		msg, err := e.encode()
		if err != nil {
			return err
		}
		if _, err := io.WriteString(w, msg); err != nil {
			return err
		}
		return rc.Flush()
	}
	if err := fn(r.Context(), send); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("minato: event stream failed", "err", err, "path", r.URL.Path)
	}
}

var newlines = strings.NewReplacer("\r\n", "\n", "\r", "\n")

func (e SSEEvent) encode() (string, error) {
	if strings.ContainsAny(e.ID+e.Event, "\r\n") {
		return "", errors.New("minato: SSE id and event must not contain newlines")
	}
	var data string
	switch d := e.Data.(type) {
	case nil:
	case string:
		data = d
	case []byte:
		data = string(d)
	default:
		b, err := json.Marshal(d)
		if err != nil {
			return "", err
		}
		data = string(b)
	}
	var b strings.Builder
	if e.ID != "" {
		fmt.Fprintf(&b, "id: %s\n", e.ID)
	}
	if e.Event != "" {
		fmt.Fprintf(&b, "event: %s\n", e.Event)
	}
	for line := range strings.SplitSeq(newlines.Replace(data), "\n") {
		fmt.Fprintf(&b, "data: %s\n", line)
	}
	b.WriteString("\n")
	return b.String(), nil
}
