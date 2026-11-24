package middleware

import (
	"cmp"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/swiftly-headquarters/minato/server"
)

var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true, http.MethodPatch: true,
	http.MethodDelete: true, http.MethodOptions: true, http.MethodConnect: true, http.MethodTrace: true,
}

type countKey struct {
	method, route string
	status        int
}

type seriesKey struct{ method, route string }

type histogram struct {
	buckets []uint64
	sum     float64
	count   uint64
}

type Metrics struct {
	inFlight atomic.Int64
	mu       sync.Mutex
	counts   map[countKey]uint64
	hists    map[seriesKey]*histogram
}

func NewMetrics() *Metrics {
	return &Metrics{counts: map[countKey]uint64{}, hists: map[seriesKey]*histogram{}}
}

func (m *Metrics) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.inFlight.Add(1)
			defer m.inFlight.Add(-1)
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			r, route := server.CaptureRoute(r)
			next.ServeHTTP(sw, r)
			method := "OTHER"
			if knownMethods[r.Method] {
				method = r.Method
			}
			m.observe(method, cmp.Or(route(), "unrouted"), cmp.Or(sw.status, http.StatusOK), time.Since(start))
		})
	}
}

func (m *Metrics) observe(method, route string, status int, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[countKey{method, route, status}]++
	h := m.hists[seriesKey{method, route}]
	if h == nil {
		h = &histogram{buckets: make([]uint64, len(durationBuckets))}
		m.hists[seriesKey{method, route}] = h
	}
	secs := d.Seconds()
	if i, _ := slices.BinarySearch(durationBuckets, secs); i < len(h.buckets) {
		h.buckets[i]++
	}
	h.sum += secs
	h.count++
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func labels(kv ...string) string {
	var b strings.Builder
	for i := 0; i < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `%s="%s"`, kv[i], labelEscaper.Replace(kv[i+1]))
	}
	return b.String()
}

func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	m.write(w)
}

func (m *Metrics) write(w io.Writer) {
	m.mu.Lock()
	counts := make(map[countKey]uint64, len(m.counts))
	for k, v := range m.counts {
		counts[k] = v
	}
	hists := make(map[seriesKey]histogram, len(m.hists))
	for k, h := range m.hists {
		hists[k] = histogram{buckets: slices.Clone(h.buckets), sum: h.sum, count: h.count}
	}
	m.mu.Unlock()

	fmt.Fprint(w, "# HELP http_requests_total Total HTTP requests handled.\n# TYPE http_requests_total counter\n")
	for _, k := range slices.SortedFunc(func(yield func(countKey) bool) {
		for k := range counts {
			if !yield(k) {
				return
			}
		}
	}, func(a, b countKey) int {
		return cmp.Or(cmp.Compare(a.route, b.route), cmp.Compare(a.method, b.method), cmp.Compare(a.status, b.status))
	}) {
		fmt.Fprintf(w, "http_requests_total{%s} %d\n", labels("method", k.method, "route", k.route, "status", strconv.Itoa(k.status)), counts[k])
	}

	fmt.Fprint(w, "# HELP http_request_duration_seconds HTTP request latency.\n# TYPE http_request_duration_seconds histogram\n")
	for _, k := range slices.SortedFunc(func(yield func(seriesKey) bool) {
		for k := range hists {
			if !yield(k) {
				return
			}
		}
	}, func(a, b seriesKey) int {
		return cmp.Or(cmp.Compare(a.route, b.route), cmp.Compare(a.method, b.method))
	}) {
		h, base := hists[k], labels("method", k.method, "route", k.route)
		var cumulative uint64
		for i, le := range durationBuckets {
			cumulative += h.buckets[i]
			fmt.Fprintf(w, "http_request_duration_seconds_bucket{%s,le=\"%s\"} %d\n", base, strconv.FormatFloat(le, 'g', -1, 64), cumulative)
		}
		fmt.Fprintf(w, "http_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", base, h.count)
		fmt.Fprintf(w, "http_request_duration_seconds_sum{%s} %s\n", base, strconv.FormatFloat(h.sum, 'g', -1, 64))
		fmt.Fprintf(w, "http_request_duration_seconds_count{%s} %d\n", base, h.count)
	}

	fmt.Fprintf(w, "# HELP http_requests_in_flight Requests being handled right now.\n# TYPE http_requests_in_flight gauge\nhttp_requests_in_flight %d\n", m.inFlight.Load())
}
