package middleware_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/swiftly-headquarters/minato/middleware"
)

func TestMemoryStoreEvictsFinishedEntriesOnly(t *testing.T) {
	ctx := context.Background()
	s := middleware.NewMemoryIdempotency()
	s.MaxEntries = 2
	_, ca, _ := s.Begin(ctx, "a", "fp", time.Minute)
	if _, _, err := s.Begin(ctx, "b", "fp", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Begin(ctx, "c", "fp", time.Minute); !errors.Is(err, middleware.ErrStoreFull) {
		t.Fatalf("a store full of unfinished requests refuses new keys: %v", err)
	}
	if err := s.Finish(ctx, "a", ca, middleware.IdempotencyEntry{Fingerprint: "fp", Status: 201}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, claim, err := s.Begin(ctx, "c", "fp", time.Minute); err != nil || claim == "" {
		t.Fatalf("the finished entry makes room: claim=%q err=%v", claim, err)
	}
	if e, claim, _ := s.Begin(ctx, "b", "fp", time.Minute); claim != "" || e == nil || e.Done {
		t.Fatalf("an unfinished claim is never evicted: claim=%q %+v", claim, e)
	}
}

func TestMemoryStoreByteCapEvictsOldestFinished(t *testing.T) {
	ctx := context.Background()
	s := middleware.NewMemoryIdempotency()
	s.MaxBytes = 100
	body := []byte(strings.Repeat("x", 60))
	for _, k := range []string{"a", "b"} {
		_, claim, _ := s.Begin(ctx, k, "fp", time.Minute)
		if err := s.Finish(ctx, k, claim, middleware.IdempotencyEntry{Fingerprint: "fp", Status: 200, Body: body}, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if _, claim, _ := s.Begin(ctx, "a", "fp", time.Minute); claim == "" {
		t.Fatal("the oldest finished entry is evicted to stay under the byte cap")
	}
	if e, _, _ := s.Begin(ctx, "b", "fp", time.Minute); e == nil || !e.Done {
		t.Fatal("the newest entry is kept")
	}
}

func TestMemoryStoreRefusesOneResponseOverTheByteCap(t *testing.T) {
	ctx := context.Background()
	s := middleware.NewMemoryIdempotency()
	s.MaxBytes = 10
	_, claim, _ := s.Begin(ctx, "k", "fp", time.Minute)
	err := s.Finish(ctx, "k", claim, middleware.IdempotencyEntry{Fingerprint: "fp", Status: 200, Body: []byte(strings.Repeat("x", 100))}, time.Minute)
	if !errors.Is(err, middleware.ErrTooLarge) {
		t.Fatalf("an oversized response is refused: %v", err)
	}
	if _, claim, _ := s.Begin(ctx, "k", "fp", time.Minute); claim == "" {
		t.Fatal("the claim is released, so the client can retry")
	}
}

func TestRateLimitLimitsRequestsWithoutAHost(t *testing.T) {
	h := limited(middleware.RateLimitConfig{Rate: 0.0001, Burst: 1})
	if rec := hit(h, ""); rec.Code != 204 {
		t.Fatalf("the first request without a host gets through: %d", rec.Code)
	}
	for _, addr := range []string{":", ":1234"} {
		if rec := hit(h, addr); rec.Code != 429 {
			t.Fatalf("RemoteAddr %q shares the limit of an unknown client: %d", addr, rec.Code)
		}
	}
}

func TestMemoryStoreTreatsAnEntryAtItsExpiryInstantAsGone(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	s := middleware.NewMemoryIdempotency()
	s.MaxEntries = 1
	s.Now = func() time.Time { return now }
	if _, claim, err := s.Begin(ctx, "a", "fp", time.Second); err != nil || claim == "" {
		t.Fatalf("setup: %q %v", claim, err)
	}
	now = now.Add(time.Second)
	if _, claim, err := s.Begin(ctx, "b", "fp", time.Second); err != nil || claim == "" {
		t.Fatalf("at the exact expiry instant Begin already treats key a as free, so it must not count against the limit: claim=%q err=%v", claim, err)
	}
}
