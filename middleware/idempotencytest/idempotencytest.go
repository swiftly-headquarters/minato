package idempotencytest

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swiftly-headquarters/minato/middleware"
)

func Run(t *testing.T, newStore func(t *testing.T) middleware.IdempotencyStore) {
	t.Helper()
	ctx := context.Background()
	long := time.Minute
	begin := func(t *testing.T, s middleware.IdempotencyStore, key, fp string, ttl time.Duration) (*middleware.IdempotencyEntry, bool) {
		t.Helper()
		e, claimed, err := s.Begin(ctx, key, fp, ttl)
		if err != nil {
			t.Fatalf("Begin(%q): %v", key, err)
		}
		if claimed && e != nil {
			t.Fatalf("Begin(%q) claimed the key and also returned an entry", key)
		}
		if !claimed && e == nil {
			t.Fatalf("Begin(%q) did not claim the key and returned no entry", key)
		}
		return e, claimed
	}

	t.Run("first Begin claims, second sees an unfinished entry", func(t *testing.T) {
		s := newStore(t)
		if _, claimed := begin(t, s, "k", "fp1", long); !claimed {
			t.Fatal("a new key must be claimed")
		}
		e, claimed := begin(t, s, "k", "fp2", long)
		if claimed || e.Done || e.Fingerprint != "fp1" {
			t.Fatalf("a claimed key reports the first fingerprint and Done=false: claimed=%v %+v", claimed, e)
		}
	})

	t.Run("Finish stores the response for later Begin calls", func(t *testing.T) {
		s := newStore(t)
		begin(t, s, "k", "fp", long)
		want := middleware.IdempotencyEntry{
			Fingerprint: "fp", Status: 201,
			Header: http.Header{"Content-Type": {"application/json"}, "X-Multi": {"a", "b"}},
			Body:   []byte(`{"id":1}`),
		}
		if err := s.Finish(ctx, "k", want, long); err != nil {
			t.Fatal(err)
		}
		e, claimed := begin(t, s, "k", "fp", long)
		if claimed || !e.Done || e.Status != 201 || e.Fingerprint != "fp" || string(e.Body) != `{"id":1}` ||
			!slices.Equal(e.Header["X-Multi"], []string{"a", "b"}) || e.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("a finished entry round-trips intact: claimed=%v %+v", claimed, e)
		}
	})

	t.Run("an empty body and header survive", func(t *testing.T) {
		s := newStore(t)
		begin(t, s, "k", "fp", long)
		if err := s.Finish(ctx, "k", middleware.IdempotencyEntry{Fingerprint: "fp", Status: 204}, long); err != nil {
			t.Fatal(err)
		}
		if e, _ := begin(t, s, "k", "fp", long); !e.Done || e.Status != 204 || len(e.Body) != 0 {
			t.Fatalf("%+v", e)
		}
	})

	t.Run("Release frees the key", func(t *testing.T) {
		s := newStore(t)
		begin(t, s, "k", "fp", long)
		if err := s.Release(ctx, "k"); err != nil {
			t.Fatal(err)
		}
		if _, claimed := begin(t, s, "k", "fp", long); !claimed {
			t.Fatal("a released key can be claimed again")
		}
	})

	t.Run("Release of an unknown key is not an error", func(t *testing.T) {
		if err := newStore(t).Release(ctx, "never-claimed"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("keys are independent", func(t *testing.T) {
		s := newStore(t)
		begin(t, s, "a", "fp", long)
		if _, claimed := begin(t, s, "b", "fp", long); !claimed {
			t.Fatal("another key is not affected")
		}
		if err := s.Release(ctx, "a"); err != nil {
			t.Fatal(err)
		}
		if _, claimed := begin(t, s, "b", "fp", long); claimed {
			t.Fatal("releasing one key must not release another")
		}
	})

	t.Run("exactly one of many concurrent Begin calls claims the key", func(t *testing.T) {
		s := newStore(t)
		var claims atomic.Int32
		var wg sync.WaitGroup
		errs := make(chan error, 64)
		for range 64 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				e, claimed, err := s.Begin(ctx, "race", "fp", long)
				switch {
				case err != nil:
					errs <- err
				case claimed:
					claims.Add(1)
				case e == nil:
					errs <- fmt.Errorf("not claimed and no entry")
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		if claims.Load() != 1 {
			t.Fatalf("%d callers claimed the same key", claims.Load())
		}
	})

	t.Run("an unfinished claim expires after its ttl", func(t *testing.T) {
		s := newStore(t)
		begin(t, s, "k", "fp", 100*time.Millisecond)
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, claimed := begin(t, s, "k", "fp", long); claimed {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("a claim whose owner never finished must expire, or a crash blocks the key forever")
			}
			time.Sleep(25 * time.Millisecond)
		}
	})

	t.Run("a finished entry expires after the ttl given to Finish", func(t *testing.T) {
		s := newStore(t)
		begin(t, s, "k", "fp", long)
		if err := s.Finish(ctx, "k", middleware.IdempotencyEntry{Fingerprint: "fp", Status: 200}, 100*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if e, claimed := begin(t, s, "k", "fp", long); claimed || !e.Done {
			t.Fatalf("still valid right after Finish: claimed=%v %+v", claimed, e)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, claimed := begin(t, s, "k", "fp", long); claimed {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("a finished entry must expire after its ttl, not keep the claim ttl")
			}
			time.Sleep(25 * time.Millisecond)
		}
	})

	t.Run("a returned entry is a copy", func(t *testing.T) {
		s := newStore(t)
		begin(t, s, "k", "fp", long)
		if err := s.Finish(ctx, "k", middleware.IdempotencyEntry{Fingerprint: "fp", Status: 200, Body: []byte("abc"), Header: http.Header{"X": {"1"}}}, long); err != nil {
			t.Fatal(err)
		}
		e, _ := begin(t, s, "k", "fp", long)
		e.Body[0] = 'Z'
		e.Header["X"][0] = "changed"
		again, _ := begin(t, s, "k", "fp", long)
		if string(again.Body) != "abc" || again.Header.Get("X") != "1" {
			t.Fatalf("callers can modify what they receive without corrupting the store: %q %v", again.Body, again.Header)
		}
	})

	t.Run("a cancelled context does not break Finish and Release", func(t *testing.T) {
		s := newStore(t)
		begin(t, s, "k", "fp", long)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if err := s.Release(context.WithoutCancel(cancelled), "k"); err != nil {
			t.Fatal(err)
		}
	})
}
