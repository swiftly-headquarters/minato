package main

import (
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/swiftly-headquarters/minato/server"
)

func registerExtras(api *server.API) {
	server.Get(api, "/clock", clock, summary("Stream the time as server-sent events"))

	server.Post(api, "/upload", upload, summary("Upload a file with a title"), func(o *server.Operation) {
		o.MaxBodyBytes = 10 << 20
		o.Errors = []int{413, 415}
	})
}

type ClockOutput struct {
	Body server.SSE
}

func clock(ctx context.Context, _ *struct{}) (*ClockOutput, error) {
	return &ClockOutput{Body: func(ctx context.Context, send func(server.SSEEvent) error) error {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case t := <-tick.C:
				if err := send(server.SSEEvent{Event: "tick", Data: t.Format(time.RFC3339)}); err != nil {
					return err
				}
			}
		}
	}}, nil
}

type UploadInput struct {
	Form struct {
		Title string                `form:"title" minLength:"1" doc:"Name for the file"`
		File  *multipart.FileHeader `form:"file" required:"true"`
	}
}

type UploadOutput struct {
	Body struct {
		Title string `json:"title"`
		Name  string `json:"name"`
		Bytes int64  `json:"bytes"`
	}
}

func upload(ctx context.Context, in *UploadInput) (*UploadOutput, error) {
	f, err := in.Form.File.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	n, err := io.Copy(io.Discard, f)
	if err != nil {
		return nil, server.NewError(http.StatusBadRequest, "reading upload: "+err.Error())
	}
	out := &UploadOutput{}
	out.Body.Title, out.Body.Name, out.Body.Bytes = in.Form.Title, in.Form.File.Filename, n
	return out, nil
}
