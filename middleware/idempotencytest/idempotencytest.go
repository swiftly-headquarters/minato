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
	begin := func(t *testing.T, s middleware.IdempotencyStore, key, fp string, ttl time.Duration) (*middleware.IdempotencyEntry, string) {
		t.Helper()
		e, claim, err := s.Begin(ctx, key, fp, ttl)
		if err != nil {
			t.Fatalf("Begin(%q): %v", key, err)
		}
		if claim != "" && e != nil {
			t.Fatalf("Begin(%q) claimed the key and also returned an entry", key)
		}
		if claim == "" && e == nil {
			t.Fatalf("Begin(%q) neither claimed the key nor returned an entry", key)
		}
		return e, claim
	}

	t.Run("first Begin claims, second sees an unfinished entry", func(t *testing.T) {
		s := newStore(t)
		if _, claim := begin(t, s, "k", "fp1", long); claim == "" {
			t.Fatal("a new key must be claimed")
		}
		e, claim := begin(t, s, "k", "fp2", long)
		if claim != "" || e.Done || e.Fingerprint != "fp1" {
			t.Fatalf("a claimed key reports the first fingerprint and Done=false: claim=%q %+v", claim, e)
		}
	})

	t.Run("Finish stores the response for later Begin calls", func(t *testing.T) {
		s := newStore(t)
		_, claim := begin(t, s, "k", "fp", long)
		want := middleware.IdempotencyEntry{
			Fingerprint: "fp", Status: 201,
			Header: http.Header{"Content-Type": {"application/json"}, "X-Multi": {"a", "b"}},
			Body:   []byte(`{"id":1}`),
		}
		if err := s.Finish(ctx, "k", claim, want, long); err != nil {
			t.Fatal(err)
		}
		e, again := begin(t, s, "k", "fp", long)
		if again != "" || !e.Done || e.Status != 201 || e.Fingerprint != "fp" || string(e.Body) != `{"id":1}` ||
			!slices.Equal(e.Header["X-Multi"], []string{"a", "b"}) || e.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("a finished entry round-trips intact: claim=%q %+v", again, e)
		}
	})

	t.Run("an empty body and header survive", func(t *testing.T) {
		s := newStore(t)
		_, claim := begin(t, s, "k", "fp", long)
		if err := s.Finish(ctx, "k", claim, middleware.IdempotencyEntry{Fingerprint: "fp", Status: 204}, long); err != nil {
			t.Fatal(err)
		}
		if e, _ := begin(t, s, "k", "fp", long); !e.Done || e.Status != 204 || len(e.Body) != 0 {
			t.Fatalf("%+v", e)
		}
	})

	t.Run("Release frees the key", func(t *testing.T) {
		s := newStore(t)
		_, claim := begin(t, s, "k", "fp", long)
		if err := s.Release(ctx, "k", claim); err != nil {
			t.Fatal(err)
		}
		if _, again := begin(t, s, "k", "fp", long); again == "" {
			t.Fatal("a released key can be claimed again")
		}
	})

	t.Run("Release of an unknown key is not an error", func(t *testing.T) {
		if err := newStore(t).Release(ctx, "never-claimed", "no-such-claim"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("keys are independent", func(t *testing.T) {
		s := newStore(t)
		_, claimA := begin(t, s, "a", "fp", long)
		if _, claimB := begin(t, s, "b", "fp", long); claimB == "" {
			t.Fatal("another key is not affected")
		}
		if err := s.Release(ctx, "a", claimA); err != nil {
			t.Fatal(err)
		}
		if _, claimB := begin(t, s, "b", "fp", long); claimB != "" {
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
				e, claim, err := s.Begin(ctx, "race", "fp", long)
				switch {
				case err != nil:
					errs <- err
				case claim != "":
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
			if _, claim := begin(t, s, "k", "fp", long); claim != "" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("a claim whose owner never finished must expire, or a crash blocks the key forever")
			}
			time.Sleep(25 * time.Millisecond)
		}
	})

	t.Run("a stale claim cannot finish or release a newer claim", func(t *testing.T) {
		s := newStore(t)
		_, stale := begin(t, s, "k", "fp", 100*time.Millisecond)
		var fresh string
		deadline := time.Now().Add(5 * time.Second)
		for fresh == "" {
			if time.Now().After(deadline) {
				t.Fatal("the expired claim was never handed to a new caller")
			}
			_, fresh = begin(t, s, "k", "fp", long)
			time.Sleep(25 * time.Millisecond)
		}
		if err := s.Finish(ctx, "k", stale, middleware.IdempotencyEntry{Fingerprint: "fp", Status: 500}, long); err != nil {
			t.Fatal(err)
		}
		if err := s.Release(ctx, "k", stale); err != nil {
			t.Fatal(err)
		}
		e, claim := begin(t, s, "k", "fp", long)
		if claim != "" || e.Done {
			t.Fatalf("a stale owner must not change the newer claim: claim=%q %+v", claim, e)
		}
		if err := s.Finish(ctx, "k", fresh, middleware.IdempotencyEntry{Fingerprint: "fp", Status: 201}, long); err != nil {
			t.Fatal(err)
		}
		if e, _ := begin(t, s, "k", "fp", long); !e.Done || e.Status != 201 {
			t.Fatalf("the current owner's result is stored: %+v", e)
		}
	})

	t.Run("a finished entry expires after the ttl given to Finish", func(t *testing.T) {
		s := newStore(t)
		_, claim := begin(t, s, "k", "fp", long)
		if err := s.Finish(ctx, "k", claim, middleware.IdempotencyEntry{Fingerprint: "fp", Status: 200}, 100*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if e, again := begin(t, s, "k", "fp", long); again != "" || !e.Done {
			t.Fatalf("still valid right after Finish: claim=%q %+v", again, e)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, again := begin(t, s, "k", "fp", long); again != "" {
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
		_, claim := begin(t, s, "k", "fp", long)
		if err := s.Finish(ctx, "k", claim, middleware.IdempotencyEntry{Fingerprint: "fp", Status: 200, Body: []byte("abc"), Header: http.Header{"X": {"1"}}}, long); err != nil {
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

	t.Run("a cancelled context does not break Release", func(t *testing.T) {
		s := newStore(t)
		_, claim := begin(t, s, "k", "fp", long)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if err := s.Release(context.WithoutCancel(cancelled), "k", claim); err != nil {
			t.Fatal(err)
		}
		if _, again := begin(t, s, "k", "fp", long); again == "" {
			t.Fatal("the release did not go through, so the key is still held")
		}
	})
}
