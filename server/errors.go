package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

type ErrorModel struct {
	Type   string         `json:"type,omitempty" doc:"URI identifying the problem type"`
	Title  string         `json:"title,omitempty" doc:"Short summary of the problem type"`
	Status int            `json:"status,omitempty" doc:"HTTP status code"`
	Detail string         `json:"detail,omitempty" doc:"Explanation specific to this occurrence"`
	Errors []*ErrorDetail `json:"errors,omitempty" doc:"Individual validation problems"`
}

type ErrorDetail struct {
	Location string `json:"location,omitempty"`
	Message  string `json:"message"`
	Value    any    `json:"value,omitempty"`
}

func (e *ErrorModel) Error() string { return cmp.Or(e.Detail, e.Title) }

func NewError(status int, detail string, errs ...*ErrorDetail) *ErrorModel {
	return &ErrorModel{Title: http.StatusText(status), Status: status, Detail: detail, Errors: errs}
}

func Error400(detail string, errs ...*ErrorDetail) error { return NewError(400, detail, errs...) }
func Error401(detail string) error                       { return NewError(401, detail) }
func Error403(detail string) error                       { return NewError(403, detail) }
func Error404(detail string) error                       { return NewError(404, detail) }
func Error409(detail string) error                       { return NewError(409, detail) }
func Error422(detail string, errs ...*ErrorDetail) error { return NewError(422, detail, errs...) }

func WriteError(w http.ResponseWriter, err error) { writeError(context.Background(), w, err) }

func writeError(ctx context.Context, w http.ResponseWriter, err error) {
	var em *ErrorModel
	switch {
	case errors.As(err, &em):
	case errors.Is(err, context.DeadlineExceeded):
		em = NewError(http.StatusServiceUnavailable, "request timed out")
	default:
		slog.ErrorContext(ctx, "minato: unhandled error", "err", err)
		em = NewError(http.StatusInternalServerError, "")
	}
	b, err := json.Marshal(em)
	if err != nil {
		em = NewError(http.StatusInternalServerError, "")
		b, _ = json.Marshal(em)
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(cmp.Or(em.Status, http.StatusInternalServerError))
	w.Write(b)
}
