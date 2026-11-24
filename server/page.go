package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
)

type Page struct {
	Limit  int    `query:"limit" default:"20" minimum:"1" maximum:"100" doc:"Maximum number of items to return"`
	Cursor string `query:"cursor" doc:"Opaque cursor from the previous page's next_cursor; omit for the first page"`
}

type PageBody[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty" doc:"Pass as cursor to get the next page; absent on the last page"`
}

func EncodeCursor(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("minato: EncodeCursor: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func DecodeCursor(s string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return errors.New("not a valid cursor")
	}
	if err := json.Unmarshal(b, v); err != nil {
		return errors.New("not a valid cursor")
	}
	return nil
}

func InvalidCursor(err error) error {
	return Error422("invalid cursor", &ErrorDetail{Location: "query.cursor", Message: err.Error()})
}

func Paginate[T any](rows []T, limit int, cursorOf func(T) any) PageBody[T] {
	if limit < 1 {
		limit = 1
	}
	if len(rows) > limit {
		items := rows[:limit]
		return PageBody[T]{Items: items, NextCursor: EncodeCursor(cursorOf(items[limit-1]))}
	}
	if rows == nil {
		rows = []T{}
	}
	return PageBody[T]{Items: rows}
}

func schemaName(t reflect.Type) string {
	base, args, generic := strings.Cut(t.Name(), "[")
	if !generic {
		return base
	}
	args = strings.TrimSuffix(args, "]")
	out := base
	for _, a := range strings.Split(args, ",") {
		a = strings.TrimSpace(a)
		if i := strings.LastIndexAny(a, "/."); i >= 0 {
			a = a[i+1:]
		}
		out += a
	}
	return out
}
