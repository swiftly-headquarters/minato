package app

import (
	"context"
	"crypto/tls"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/swiftly-headquarters/minato/middleware"
	"github.com/swiftly-headquarters/minato/server"
)

func New() (*server.API, *middleware.Metrics) {
	api := server.New("Minato Example", "1.0.0")
	api.Spec.Servers = []server.Server{{URL: "http://localhost:8080"}}
	api.GRPCService = "minato.example.v1.Example"
	slog.SetDefault(slog.New(middleware.SlogContext(slog.NewTextHandler(os.Stderr, nil))))
	ids := middleware.RequestIDConfig{}
	if prefix := os.Getenv("REQUEST_ID_PREFIX"); prefix != "" {
		ids.Generate = func(*http.Request) string { return prefix + "-" + middleware.NewRequestID() }
	}
	metrics := middleware.NewMetrics()
	api.ETag = true
	api.Use(
		middleware.RequestID(ids),
		middleware.Logger(nil),
		metrics.Middleware(),
		middleware.CORS(middleware.CORSConfig{}),
		middleware.RateLimit(middleware.RateLimitConfig{Rate: 50, Burst: 100}),
		middleware.SecureHeaders(middleware.SecureHeadersConfig{}),
		middleware.Idempotency(middleware.IdempotencyConfig{}),
		middleware.Gzip(),
	)

	server.Get(api, "/hello", hello, summary("Greet the caller"))
	server.Get(api, "/files/{path...}", getFile, summary("Echo the requested path (catch-all demo)"))
	registerUsers(api)
	registerAdmin(api)
	return api, metrics
}

func Run(api *server.API) {
	if cert, key := os.Getenv("TLS_CERT"), os.Getenv("TLS_KEY"); cert != "" {
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			log.Fatal(err)
		}
		api.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	}
	api.OnStart = func(addr net.Addr) { log.Printf("listening on %s", addr) }
	api.OnStop = func(context.Context) { log.Println("stopped") }
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if v := os.Getenv("SHUTDOWN_DELAY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("SHUTDOWN_DELAY: %v", err)
		}
		api.ShutdownDelay = d
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := api.Run(ctx, addr); err != nil {
		log.Fatal(err)
	}
}

func summary(s string) func(*server.Operation) {
	return func(o *server.Operation) { o.Summary = s }
}

func errs(codes ...int) func(*server.Operation) {
	return func(o *server.Operation) { o.Errors = codes }
}

type HelloInput struct {
	Name string `query:"name" default:"world" maxLength:"32" doc:"Who to greet"`
}

type HelloOutput struct {
	Body struct {
		Message string `json:"message" example:"Hello, world!"`
	}
}

func hello(ctx context.Context, in *HelloInput) (*HelloOutput, error) {
	out := &HelloOutput{}
	out.Body.Message = "Hello, " + in.Name + "!"
	return out, nil
}

type FileInput struct {
	Path string `path:"path" doc:"Everything after /files/"`
}

type FileOutput struct {
	ContentType string `header:"Content-Type"`
	Body        []byte
}

func getFile(ctx context.Context, in *FileInput) (*FileOutput, error) {
	return &FileOutput{ContentType: "text/plain; charset=utf-8", Body: []byte(in.Path)}, nil
}
