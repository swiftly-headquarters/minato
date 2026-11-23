package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/swiftly-headquarters/minato/middleware"
	"github.com/swiftly-headquarters/minato/server"
)

func main() {
	api := server.New("Minato Example", "1.0.0")
	api.Spec.Servers = []server.Server{{URL: "http://localhost:8080"}}
	api.BearerAuth("bearer")
	api.Use(middleware.Logger(nil), middleware.CORS(middleware.CORSConfig{}), middleware.Gzip())

	server.Get(api, "/hello", hello, summary("Greet the caller"))
	server.Get(api, "/files/{path...}", getFile, summary("Echo the requested path (catch-all demo)"))
	registerUsers(api)
	registerAdmin(api)
	registerExtras(api)
	registerWeb(api)

	if len(os.Args) > 1 && os.Args[1] == "openapi" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(api.Spec)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Println("listening on :8080, docs at http://localhost:8080/docs")
	if err := api.Run(ctx, ":8080"); err != nil {
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
