package middleware_test

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/swiftly-headquarters/minato/middleware"
	"github.com/swiftly-headquarters/minato/server"
)

func TestMetrics(t *testing.T) {
	m := middleware.NewMetrics()
	api := server.New("t", "1")
	api.Use(middleware.RequestID(middleware.RequestIDConfig{}), m.Middleware(), middleware.Timeout(time.Second))
	server.Get(api, "/text", okHandler)
	server.Get(api, "/users/{id}", func(ctx context.Context, in *struct {
		ID int `path:"id"`
	}) (*empty, error) {
		return nil, nil
	})
	server.Get(api.Group("/v1"), "/things", okHandler)
	server.Get(api, "/missing", func(ctx context.Context, _ *empty) (*empty, error) { return nil, server.Error404("nope") })
	server.Handle(api, "GET /metrics", m)

	for _, p := range []string{"/text", "/users/1", "/users/2", "/users/3", "/v1/things", "/missing", "/nope/zzz"} {
		do(api, "GET", p)
	}
	do(api, "FOOBAR", "/text")

	rec := do(api, "GET", "/metrics")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("scrape: %d %v", rec.Code, rec.Header())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`# TYPE http_requests_total counter`,
		`# TYPE http_request_duration_seconds histogram`,
		`# TYPE http_requests_in_flight gauge`,
		`http_requests_total{method="GET",route="GET /text",status="204"} 1`,
		`http_requests_total{method="GET",route="GET /users/{id}",status="204"} 3`,
		`http_requests_total{method="GET",route="GET /v1/things",status="204"} 1`,
		`http_requests_total{method="GET",route="GET /missing",status="404"} 1`,
		`http_requests_total{method="GET",route="unrouted",status="404"} 1`,
		`http_requests_total{method="OTHER",route="unrouted",status="405"} 1`,
		`http_request_duration_seconds_count{method="GET",route="GET /users/{id}"} 3`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("metrics lack %s\n%s", want, body)
		}
	}
	for _, leak := range []string{"/users/1", "/nope/zzz", "FOOBAR"} {
		if strings.Contains(body, leak) {
			t.Errorf("client-chosen value %q must not become a label", leak)
		}
	}

	re := regexp.MustCompile(`http_request_duration_seconds_bucket\{method="GET",route="GET /text",le="([^"]+)"\} (\d+)`)
	buckets := re.FindAllStringSubmatch(body, -1)
	if len(buckets) != 12 || buckets[11][1] != "+Inf" || buckets[0][1] != "0.005" {
		t.Fatalf("expected 11 buckets and +Inf: %v", buckets)
	}
	prev := -1
	for _, b := range buckets {
		n, _ := strconv.Atoi(b[2])
		if n < prev {
			t.Errorf("buckets are cumulative, got %v", buckets)
		}
		prev = n
	}
	if prev != 1 {
		t.Errorf("+Inf bucket must equal the count: %d", prev)
	}
	if !strings.Contains(body, `http_request_duration_seconds_sum{method="GET",route="GET /text"} `) {
		t.Error("histogram sum missing")
	}

	if head := do(api, "HEAD", "/metrics"); head.Code != 200 {
		t.Errorf("HEAD on the metrics endpoint: %d", head.Code)
	}
}

func TestMetricsInFlight(t *testing.T) {
	m := middleware.NewMetrics()
	api := server.New("t", "1")
	api.Use(m.Middleware())
	entered, release := make(chan struct{}), make(chan struct{})
	server.Get(api, "/block", func(ctx context.Context, _ *empty) (*empty, error) {
		close(entered)
		<-release
		return nil, nil
	})
	server.Handle(api, "GET /metrics", m)

	done := make(chan struct{})
	go func() {
		defer close(done)
		do(api, "GET", "/block")
	}()
	<-entered
	if body := do(api, "GET", "/metrics").Body.String(); !strings.Contains(body, "http_requests_in_flight 2\n") {
		t.Errorf("the blocked request and the scrape are both in flight:\n%s", body)
	}
	close(release)
	<-done
	body := do(api, "GET", "/metrics").Body.String()
	if !strings.Contains(body, "http_requests_in_flight 1\n") || !strings.Contains(body, `route="GET /block",status="204"} 1`) {
		t.Errorf("after it finished only the scrape is in flight and the request is counted:\n%s", body)
	}
}

func TestMetricsRejectedBeforeRouting(t *testing.T) {
	m := middleware.NewMetrics()
	api := server.New("t", "1")
	api.Use(m.Middleware(), middleware.RateLimit(middleware.RateLimitConfig{Rate: 0.001, Burst: 1, Now: newClock().Now}))
	server.Get(api, "/text", okHandler)
	server.Handle(api, "GET /metrics", m)

	do(api, "GET", "/text")
	if do(api, "GET", "/text").Code != 429 {
		t.Fatal("setup: the second request must be rejected")
	}
	scrapeLimit := do(api, "GET", "/metrics")
	if scrapeLimit.Code != 429 {
		t.Fatalf("the scrape shares the client's bucket here: %d", scrapeLimit.Code)
	}
	open := server.New("t", "1")
	open.Use(m.Middleware())
	server.Handle(open, "GET /metrics", m)
	body := do(open, "GET", "/metrics").Body.String()
	for _, want := range []string{
		`http_requests_total{method="GET",route="GET /text",status="204"} 1`,
		`http_requests_total{method="GET",route="unrouted",status="429"} 2`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("metrics lack %s\n%s", want, body)
		}
	}
}
