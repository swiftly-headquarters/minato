package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

type ErrorModel struct {
	Type   string         `json:"type,omitempty" doc:"URI identifying the problem type"`
	Title  string         `json:"title,omitempty" doc:"Short summary of the problem type"`
	Status int            `json:"status,omitempty" doc:"HTTP status code"`
	Detail string         `json:"detail,omitempty" doc:"Explanation specific to this occurrence"`
	Errors []*ErrorDetail `json:"errors,omitempty" doc:"Individual validation problems"`
	Fields map[string]any `json:"fields,omitempty" doc:"The same problems nested by field name, with the messages of each field under _errors"`
}

type ErrorDetail struct {
	Location string `json:"location,omitempty"`
	Message  string `json:"message"`
	Value    any    `json:"value,omitempty"`
}

func (e *ErrorModel) Error() string { return cmp.Or(e.Detail, e.Title) }

func NewError(status int, detail string, errs ...*ErrorDetail) *ErrorModel {
	return &ErrorModel{Title: http.StatusText(status), Status: status, Detail: detail, Errors: errs, Fields: nestErrors(errs)}
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

type fieldError struct {
	Message string `json:"message"`
}

func splitLocation(loc string) []string {
	var parts []string
	for _, seg := range strings.Split(loc, ".") {
		name, rest, _ := strings.Cut(seg, "[")
		if name != "" {
			parts = append(parts, name)
		}
		for rest != "" {
			idx, after, ok := strings.Cut(rest, "]")
			if !ok {
				break
			}
			parts = append(parts, idx)
			rest = strings.TrimPrefix(after, "[")
		}
	}
	return parts
}

func escapeKey(part string) string {
	if strings.Trim(part, "_") == "errors" && strings.HasSuffix(part, "errors") && strings.HasPrefix(part, "_") {
		return "_" + part
	}
	return part
}

func nestErrors(errs []*ErrorDetail) map[string]any {
	if len(errs) == 0 {
		return nil
	}
	root := map[string]any{}
	for _, e := range errs {
		node := root
		for _, part := range splitLocation(e.Location) {
			part = escapeKey(part)
			next, ok := node[part].(map[string]any)
			if !ok {
				next = map[string]any{}
				node[part] = next
			}
			node = next
		}
		list, _ := node["_errors"].([]fieldError)
		node["_errors"] = append(list, fieldError{Message: e.Message})
	}
	return root
}
