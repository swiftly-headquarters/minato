package middleware

import (
	"bytes"
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
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

var (
	ErrStoreFull = errors.New("idempotency store is full of unfinished requests")
	ErrTooLarge  = errors.New("response is larger than the idempotency store's byte limit")
)

type IdempotencyStore interface {
	Begin(ctx context.Context, key, fingerprint string, ttl time.Duration) (existing *IdempotencyEntry, claim string, err error)
	Finish(ctx context.Context, key, claim string, e IdempotencyEntry, ttl time.Duration) error
	Release(ctx context.Context, key, claim string) error
}

type memEntry struct {
	IdempotencyEntry
	claim   string
	expires time.Time
	size    int64
	elem    *list.Element
}

type MemoryIdempotency struct {
	MaxEntries int
	MaxBytes   int64

	mu        sync.Mutex
	entries   map[string]*memEntry
	order     *list.List
	bytes     int64
	lastSweep time.Time
	Now       func() time.Time
}

func NewMemoryIdempotency() *MemoryIdempotency {
	return &MemoryIdempotency{MaxEntries: 10000, MaxBytes: 32 << 20, entries: map[string]*memEntry{}, order: list.New(), Now: time.Now}
}

func (m *MemoryIdempotency) remove(key string) {
	e, ok := m.entries[key]
	if !ok {
		return
	}
	m.order.Remove(e.elem)
	m.bytes -= e.size
	delete(m.entries, key)
}

func (m *MemoryIdempotency) put(key string, e *memEntry) {
	m.remove(key)
	e.elem = m.order.PushBack(key)
	m.entries[key] = e
	m.bytes += e.size
}

func (m *MemoryIdempotency) evictOldest() bool {
	for el := m.order.Front(); el != nil; el = el.Next() {
		key := el.Value.(string)
		if m.entries[key].claim == "" {
			m.remove(key)
			return true
		}
	}
	return false
}

func (m *MemoryIdempotency) expire(now time.Time) {
	for k, e := range m.entries {
		if !now.Before(e.expires) {
			m.remove(k)
		}
	}
}

func (m *MemoryIdempotency) Begin(_ context.Context, key, fingerprint string, ttl time.Duration) (*IdempotencyEntry, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.Now()
	if now.Sub(m.lastSweep) >= time.Minute {
		m.lastSweep = now
		m.expire(now)
	}
	if e, ok := m.entries[key]; ok && now.Before(e.expires) {
		cp := e.IdempotencyEntry.clone()
		return &cp, "", nil
	}
	if len(m.entries) >= m.MaxEntries {
		m.expire(now)
	}
	for len(m.entries) >= m.MaxEntries {
		if !m.evictOldest() {
			return nil, "", ErrStoreFull
		}
	}
	claim, err := newClaim()
	if err != nil {
		return nil, "", err
	}
	m.put(key, &memEntry{IdempotencyEntry: IdempotencyEntry{Fingerprint: fingerprint}, claim: claim, expires: now.Add(ttl)})
	return nil, claim, nil
}

func (m *MemoryIdempotency) Finish(_ context.Context, key, claim string, e IdempotencyEntry, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.entries[key]; !ok || claim == "" || cur.claim != claim {
		return nil
	}
	e = e.clone()
	e.Done = true
	ne := &memEntry{IdempotencyEntry: e, expires: m.Now().Add(ttl), size: entrySize(e)}
	m.remove(key)
	if ne.size > m.MaxBytes {
		return ErrTooLarge
	}
	for m.bytes+ne.size > m.MaxBytes {
		if !m.evictOldest() {
			return ErrStoreFull
		}
	}
	m.put(key, ne)
	return nil
}

func (m *MemoryIdempotency) Release(_ context.Context, key, claim string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.entries[key]; ok && claim != "" && cur.claim == claim {
		m.remove(key)
	}
	return nil
}

func newClaim() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func entrySize(e IdempotencyEntry) int64 {
	n := int64(len(e.Fingerprint) + len(e.Body))
	for k, vs := range e.Header {
		for _, v := range vs {
			n += int64(len(k) + len(v))
		}
	}
	return n
}

type IdempotencyConfig struct {
	Store   IdempotencyStore
	Header  string
	TTL     time.Duration
	LockTTL time.Duration
	MaxBody int64
	Methods []string
	Scope   func(*http.Request) string
}

const maxCachedBody = 1 << 20

func defaultScope(r *http.Request) string {
	return digest(r.Header.Get("Authorization"), r.Header.Get("Cookie"), r.Host)
}

func digest(parts ...string) string {
	sum := sha256.New()
	var n [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(n[:], uint64(len(p)))
		sum.Write(n[:])
		sum.Write([]byte(p))
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func notReplayed(k string) bool {
	switch k {
	case "Set-Cookie", "Vary", "X-Request-Id", "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	}
	return strings.HasPrefix(k, "Access-Control-")
}

func ownHeaders(before, after http.Header) http.Header {
	own := http.Header{}
	for k, vs := range after {
		if notReplayed(k) || slices.Equal(before[k], vs) {
			continue
		}
		own[k] = slices.Clone(vs)
	}
	return own
}

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
	if c.Scope == nil {
		c.Scope = defaultScope
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
			fp := digest(r.Method, r.URL.RequestURI(), r.Header.Get("Accept-Encoding"), string(body))
			storeKey := digest(c.Scope(r)) + ":" + key
			before := w.Header().Clone()

			existing, claim, err := c.Store.Begin(r.Context(), storeKey, fp, c.LockTTL)
			if err == nil && existing == nil && claim == "" {
				err = errors.New("store returned neither a claim nor an entry")
			}
			if err != nil {
				slog.ErrorContext(r.Context(), "minato: idempotency store failed", "op", "begin", "err", err)
				server.WriteError(w, server.NewError(http.StatusServiceUnavailable, "idempotency store unavailable"))
				return
			}
			if existing != nil {
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
				if err := c.Store.Release(ctx, storeKey, claim); err != nil {
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
			entry := IdempotencyEntry{Fingerprint: fp, Status: status, Header: ownHeaders(before, rec.header), Body: rec.body.Bytes()}
			if err := c.Store.Finish(ctx, storeKey, claim, entry, c.TTL); err != nil {
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
