package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/swiftly-headquarters/minato/server"
)

type IdempotencyEntry struct {
	Fingerprint string
	Done        bool
	Status      int
	Header      http.Header
	Body        []byte
}

type IdempotencyStore interface {
	Begin(ctx context.Context, key, fingerprint string, ttl time.Duration) (existing *IdempotencyEntry, claimed bool, err error)
	Finish(ctx context.Context, key string, e IdempotencyEntry, ttl time.Duration) error
	Release(ctx context.Context, key string) error
}

type memEntry struct {
	IdempotencyEntry
	expires time.Time
}

type MemoryIdempotency struct {
	mu        sync.Mutex
	entries   map[string]*memEntry
	lastSweep time.Time
	now       func() time.Time
}

func NewMemoryIdempotency() *MemoryIdempotency {
	return &MemoryIdempotency{entries: map[string]*memEntry{}, now: time.Now}
}

func (m *MemoryIdempotency) sweep(now time.Time) {
	if now.Sub(m.lastSweep) < time.Minute {
		return
	}
	m.lastSweep = now
	for k, e := range m.entries {
		if now.After(e.expires) {
			delete(m.entries, k)
		}
	}
}

func (m *MemoryIdempotency) Begin(_ context.Context, key, fingerprint string, ttl time.Duration) (*IdempotencyEntry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.sweep(now)
	if e, ok := m.entries[key]; ok && now.Before(e.expires) {
		cp := e.IdempotencyEntry.clone()
		return &cp, false, nil
	}
	m.entries[key] = &memEntry{IdempotencyEntry: IdempotencyEntry{Fingerprint: fingerprint}, expires: now.Add(ttl)}
	return nil, true, nil
}

func (m *MemoryIdempotency) Finish(_ context.Context, key string, e IdempotencyEntry, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e = e.clone()
	e.Done = true
	m.entries[key] = &memEntry{IdempotencyEntry: e, expires: m.now().Add(ttl)}
	return nil
}

func (m *MemoryIdempotency) Release(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key)
	return nil
}

type IdempotencyConfig struct {
	Store   IdempotencyStore
	Header  string
	TTL     time.Duration
	LockTTL time.Duration
	MaxBody int64
	Methods []string
}

const maxCachedBody = 1 << 20

func Idempotency(c IdempotencyConfig) func(http.Handler) http.Handler {
	if c.Store == nil {
		c.Store = NewMemoryIdempotency()
	}
	if c.Header == "" {
		c.Header = "Idempotency-Key"
	}
	if c.TTL <= 0 {
		c.TTL = 24 * time.Hour
	}
	if c.LockTTL <= 0 {
		c.LockTTL = 5 * time.Minute
	}
	if c.MaxBody <= 0 {
		c.MaxBody = 1 << 20
	}
	if len(c.Methods) == 0 {
		c.Methods = []string{http.MethodPost, http.MethodPatch}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(c.Header)
			if key == "" || !slices.Contains(c.Methods, r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			if len(key) > 255 {
				server.WriteError(w, server.Error400("idempotency key is longer than 255 characters"))
				return
			}
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, c.MaxBody))
			if err != nil {
				var tooBig *http.MaxBytesError
				if errors.As(err, &tooBig) {
					server.WriteError(w, server.NewError(http.StatusRequestEntityTooLarge, "request body too large"))
					return
				}
				server.WriteError(w, server.Error400("reading body: "+err.Error()))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			sum := sha256.New()
			sum.Write([]byte(r.Method + " " + r.URL.RequestURI() + "\n" + r.Header.Get("Authorization") + "\n"))
			sum.Write(body)
			fp := hex.EncodeToString(sum.Sum(nil))

			existing, claimed, err := c.Store.Begin(r.Context(), key, fp, c.LockTTL)
			if err != nil {
				slog.ErrorContext(r.Context(), "minato: idempotency store failed", "op", "begin", "err", err)
				server.WriteError(w, server.NewError(http.StatusServiceUnavailable, "idempotency store unavailable"))
				return
			}
			if !claimed {
				switch {
				case existing.Fingerprint != fp:
					server.WriteError(w, server.Error422("idempotency key was already used for a different request"))
				case !existing.Done:
					server.WriteError(w, server.NewError(http.StatusConflict, "a request with this idempotency key is still in progress"))
				default:
					replay(w, existing)
				}
				return
			}

			ctx := context.WithoutCancel(r.Context())
			release := func() {
				if err := c.Store.Release(ctx, key); err != nil {
					slog.ErrorContext(ctx, "minato: idempotency store failed", "op", "release", "err", err)
				}
			}
			rec := &recorder{ResponseWriter: w}
			finished := false
			defer func() {
				if !finished {
					release()
				}
			}()
			next.ServeHTTP(rec, r)
			finished = true
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			if !cacheable(status) || rec.overflow {
				release()
				return
			}
			entry := IdempotencyEntry{Fingerprint: fp, Status: status, Header: rec.header, Body: rec.body.Bytes()}
			if err := c.Store.Finish(ctx, key, entry, c.TTL); err != nil {
				slog.ErrorContext(ctx, "minato: idempotency store failed", "op", "finish", "err", err)
			}
		})
	}
}

func cacheable(status int) bool {
	if status >= 500 {
		return false
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return false
	}
	return true
}

func replay(w http.ResponseWriter, e *IdempotencyEntry) {
	h := w.Header()
	for k, vs := range e.Header {
		h[k] = slices.Clone(vs)
	}
	h.Set("Idempotent-Replayed", "true")
	w.WriteHeader(e.Status)
	w.Write(e.Body)
}

type recorder struct {
	http.ResponseWriter
	status   int
	header   http.Header
	body     bytes.Buffer
	overflow bool
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 && code >= 200 {
		r.status = code
		r.header = r.Header().Clone()
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	if !r.overflow {
		if r.body.Len()+len(p) > maxCachedBody {
			r.overflow = true
			r.body.Reset()
		} else {
			r.body.Write(p)
		}
	}
	return r.ResponseWriter.Write(p)
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (e IdempotencyEntry) clone() IdempotencyEntry {
	e.Header = e.Header.Clone()
	e.Body = slices.Clone(e.Body)
	return e
}
