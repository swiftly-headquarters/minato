package server_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/swiftly-headquarters/minato/server"
)

type multiIn struct {
	Limit int `query:"limit" minimum:"1" maximum:"100"`
	Body  struct {
		Name  string `json:"name" minLength:"3"`
		Email string `json:"email" format:"email"`
		Addr  struct {
			City string `json:"city" minLength:"2"`
		} `json:"addr"`
		Items []struct {
			Qty int `json:"qty" minimum:"1"`
		} `json:"items"`
	}
}

func TestEveryProblemIsListedAndNestedByField(t *testing.T) {
	api := server.New("t", "1")
	server.Post(api, "/m", func(_ context.Context, _ *multiIn) (*empty, error) { return nil, nil })
	rec := call(api, "POST", "/m?limit=0", `{"name":"a","email":"nope","addr":{"city":"x"},"items":[{"qty":5},{"qty":0}]}`, "Content-Type", "application/json")
	if rec.Code != 422 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var got struct {
		Errors []struct{ Location string } `json:"errors"`
		Fields map[string]any              `json:"fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Errors) != 5 {
		t.Fatalf("all five problems in the flat list: %s", rec.Body)
	}
	msgs := func(v any) int {
		l, _ := v.(map[string]any)["_errors"].([]any)
		return len(l)
	}
	body := got.Fields["body"].(map[string]any)
	query := got.Fields["query"].(map[string]any)
	item1 := body["items"].(map[string]any)["1"].(map[string]any)["qty"]
	if msgs(query["limit"]) != 1 || msgs(body["name"]) != 1 || msgs(body["email"]) != 1 ||
		msgs(body["addr"].(map[string]any)["city"]) != 1 || msgs(item1) != 1 {
		t.Fatalf("one entry per failing field, nested like the input: %s", rec.Body)
	}
	if _, ok := body["items"].(map[string]any)["0"]; ok {
		t.Fatalf("a valid array item has no entry: %s", rec.Body)
	}
}

func TestFieldsKeepSeveralMessagesAndRootProblems(t *testing.T) {
	err := server.NewError(422, "bad",
		&server.ErrorDetail{Location: "body.a", Message: "one"},
		&server.ErrorDetail{Location: "body.a", Message: "two"},
		&server.ErrorDetail{Message: "whole request"},
		&server.ErrorDetail{Location: "body.l[0][1]", Message: "deep"})
	b, _ := json.Marshal(err)
	var got struct{ Fields map[string]any }
	json.Unmarshal(b, &got)
	want := map[string]any{
		"_errors": []any{map[string]any{"message": "whole request"}},
		"body": map[string]any{
			"a": map[string]any{"_errors": []any{map[string]any{"message": "one"}, map[string]any{"message": "two"}}},
			"l": map[string]any{"0": map[string]any{"1": map[string]any{"_errors": []any{map[string]any{"message": "deep"}}}}},
		},
	}
	if !reflect.DeepEqual(got.Fields, want) {
		t.Fatalf("%s", b)
	}
	b, _ = json.Marshal(server.NewError(404, "gone"))
	if string(b) != `{"title":"Not Found","status":404,"detail":"gone"}` {
		t.Fatalf("errors without field problems have no fields key: %s", b)
	}
}

func TestAFieldNamedErrorsCannotCorruptTheNesting(t *testing.T) {
	err := server.NewError(422, "bad",
		&server.ErrorDetail{Location: "body.m", Message: "on the map"},
		&server.ErrorDetail{Location: "body.m._errors", Message: "on a key called _errors"},
		&server.ErrorDetail{Location: "body.m.__errors.x", Message: "deeper"})
	b, _ := json.Marshal(err)
	var got struct{ Fields map[string]any }
	json.Unmarshal(b, &got)
	m := got.Fields["body"].(map[string]any)["m"].(map[string]any)
	list, ok := m["_errors"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("_errors always holds the messages of that exact field: %s", b)
	}
	if _, ok := m["__errors"].(map[string]any)["_errors"].([]any); !ok {
		t.Fatalf("a key named _errors moves to __errors: %s", b)
	}
	if _, ok := m["___errors"].(map[string]any)["x"]; !ok {
		t.Fatalf("keys made only of underscores and errors stay distinct: %s", b)
	}
}
