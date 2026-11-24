package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/swiftly-headquarters/minato/server"
)

func specJSON(t *testing.T, api *server.API) map[string]any {
	t.Helper()
	b, err := json.Marshal(api.Spec)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func dig(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

func errorLocations(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var em server.ErrorModel
	if err := json.Unmarshal(rec.Body.Bytes(), &em); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	var locs []string
	for _, e := range em.Errors {
		locs = append(locs, e.Location)
	}
	return locs
}

type hookOnly struct{}

func (*hookOnly) Validate(context.Context) []error { return nil }

func TestAutomatic422(t *testing.T) {
	api := server.New("t", "1")
	server.Get(api, "/plain", noop)
	server.Get(api, "/query", func(ctx context.Context, in *struct {
		Q string `query:"q"`
	}) (*empty, error) {
		return nil, nil
	})
	server.Post(api, "/body", func(ctx context.Context, in *struct {
		Body struct {
			A int `json:"a"`
		}
	}) (*empty, error) {
		return nil, nil
	})
	server.Get(api, "/hook", func(ctx context.Context, in *hookOnly) (*empty, error) { return nil, nil })

	for path, want := range map[string]bool{"/plain": false, "/query": true, "/body": true, "/hook": true} {
		method := "get"
		if path == "/body" {
			method = "post"
		}
		r := api.Spec.Paths[path][method].Responses["422"]
		if (r != nil) != want {
			t.Errorf("%s: 422 documented = %v, want %v", path, r != nil, want)
			continue
		}
		if r != nil && (r.Description != "Unprocessable Entity" || r.Content["application/problem+json"] == nil) {
			t.Errorf("%s: bad 422 response %+v", path, r)
		}
	}
}

var hookCalls int

type rangeBody struct {
	Start int `json:"start"`
	End   int `json:"end" default:"100"`
}

func (b *rangeBody) Validate(ctx context.Context) []error {
	hookCalls++
	if b.End < b.Start {
		return []error{server.FieldError("body.end", "must not be before start")}
	}
	return nil
}

type guardIn struct {
	Mode string `query:"mode" default:"loose"`
	Body rangeBody
}

func (g *guardIn) Validate(ctx context.Context) []error {
	hookCalls++
	if g.Mode == "strict" && g.Body.End > 10 {
		return []error{errors.New("strict mode caps end at 10")}
	}
	return nil
}

func TestBodyDefaultsOfEveryType(t *testing.T) {
	api := server.New("t", "1")
	type body struct {
		N    int       `json:"n" default:"7" minimum:"1"`
		F    float64   `json:"f" default:"1.5"`
		U    uint      `json:"u" default:"3"`
		B    bool      `json:"b" default:"true"`
		S    string    `json:"s" default:"x"`
		List []int     `json:"list" default:"1,2"`
		Fs   []float64 `json:"fs" default:"0.5"`
	}
	var seen []*body
	server.Post(api, "/d", func(ctx context.Context, in *struct{ Body body }) (*empty, error) {
		seen = append(seen, &in.Body)
		in.Body.List[0] = 99
		return nil, nil
	})
	for range 2 {
		if rec := call(api, "POST", "/d", `{}`); rec.Code != 204 {
			t.Fatalf("defaults must validate: %d %s", rec.Code, rec.Body)
		}
	}
	want := body{N: 7, F: 1.5, U: 3, B: true, S: "x", List: []int{99, 2}, Fs: []float64{0.5}}
	if seen[0].N != 7 || seen[0].F != 1.5 || seen[0].U != 3 || !seen[0].B || seen[0].S != "x" || !slices.Equal(seen[0].Fs, want.Fs) {
		t.Errorf("defaults not applied: %+v", seen[0])
	}
	if !slices.Equal(seen[1].List, []int{99, 2}) || seen[1].List[0] != 99 {
		t.Errorf("each request decodes its own copy: %+v", seen[1])
	}
	if rec := call(api, "POST", "/d", `{"n":0}`); rec.Code != 422 {
		t.Errorf("a supplied value must still be validated: %d", rec.Code)
	}
}

func TestValidatorHook(t *testing.T) {
	api := server.New("t", "1")
	type out = struct{ Body string }
	server.Post(api, "/range", func(ctx context.Context, in *guardIn) (*out, error) {
		return &out{Body: fmt.Sprintf("%d-%d", in.Body.Start, in.Body.End)}, nil
	})
	send := func(query, body string) *httptest.ResponseRecorder {
		hookCalls = 0
		return call(api, "POST", "/range"+query, body)
	}

	rec := send("", `{"start":1}`)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `"1-100"` || hookCalls != 2 {
		t.Fatalf("valid: %d %s calls=%d", rec.Code, rec.Body, hookCalls)
	}

	rec = send("", `{"start":500}`)
	if rec.Code != 422 || !slices.Equal(errorLocations(t, rec), []string{"body.end"}) || !strings.Contains(rec.Body.String(), "must not be before start") {
		t.Fatalf("body hook: %d %s", rec.Code, rec.Body)
	}

	rec = send("?mode=strict", `{"start":1}`)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "strict mode caps end at 10") || !slices.Equal(errorLocations(t, rec), []string{""}) {
		t.Fatalf("input hook with a plain error: %d %s", rec.Code, rec.Body)
	}

	rec = send("?mode=strict", `{"start":500}`)
	var em server.ErrorModel
	json.Unmarshal(rec.Body.Bytes(), &em)
	if rec.Code != 422 || len(em.Errors) != 2 || em.Errors[0].Message != "strict mode caps end at 10" || em.Errors[1].Location != "body.end" {
		t.Fatalf("both hooks must report, input first: %d %s", rec.Code, rec.Body)
	}

	rec = send("", `{"start":"x"}`)
	if rec.Code != 422 || hookCalls != 0 {
		t.Fatalf("hooks must not run when binding failed: %d calls=%d", rec.Code, hookCalls)
	}
}

type numBody struct {
	Score float64   `json:"score" exclusiveMinimum:"0" exclusiveMaximum:"10"`
	Step  int       `json:"step" multipleOf:"5"`
	Price float64   `json:"price" multipleOf:"0.01"`
	Tags  []string  `json:"tags" uniqueItems:"true"`
	Nums  []float64 `json:"nums" uniqueItems:"true"`
}

func TestNumericAndArrayTags(t *testing.T) {
	api := server.New("t", "1")
	server.Post(api, "/n", func(ctx context.Context, in *struct{ Body numBody }) (*empty, error) { return nil, nil })
	server.Get(api, "/q", func(ctx context.Context, in *struct {
		Qty int      `query:"qty" multipleOf:"2" exclusiveMinimum:"0"`
		Ids []string `query:"id" uniqueItems:"true"`
	}) (*empty, error) {
		return nil, nil
	})

	base := map[string]any{"score": 5, "step": 10, "price": 19.99, "tags": []string{"a", "b"}, "nums": []float64{1, 2}}
	send := func(field string, value any) *httptest.ResponseRecorder {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		if field != "" {
			m[field] = value
		}
		b, _ := json.Marshal(m)
		return call(api, "POST", "/n", string(b))
	}
	if rec := send("", nil); rec.Code != 204 {
		t.Fatalf("valid body rejected: %d %s", rec.Code, rec.Body)
	}
	for _, c := range []struct {
		field string
		value any
		ok    bool
		loc   string
	}{
		{"score", 0, false, "body.score"}, {"score", 10, false, "body.score"}, {"score", 9.99, true, ""},
		{"step", 12, false, "body.step"}, {"step", 0, true, ""}, {"step", -5, true, ""},
		{"price", 19.999, false, "body.price"}, {"price", 0.07, true, ""}, {"price", 1e12, true, ""},
		{"tags", []string{"a", "a"}, false, "body.tags[1]"}, {"tags", []string{"1", "01"}, true, ""},
		{"nums", []float64{1, 2, 1}, false, "body.nums[2]"},
	} {
		rec := send(c.field, c.value)
		switch {
		case c.ok && rec.Code != 204:
			t.Errorf("%s=%v rejected: %d %s", c.field, c.value, rec.Code, rec.Body)
		case !c.ok && (rec.Code != 422 || !slices.Equal(errorLocations(t, rec), []string{c.loc})):
			t.Errorf("%s=%v: got %d %s, want 422 at %s", c.field, c.value, rec.Code, rec.Body, c.loc)
		}
	}
	rec := call(api, "POST", "/n", `{"score":5,"step":10,"price":1,"tags":[],"nums":[1,1.0]}`)
	if rec.Code != 422 || !slices.Equal(errorLocations(t, rec), []string{"body.nums[1]"}) {
		t.Errorf("1 and 1.0 are the same number: %d %s", rec.Code, rec.Body)
	}

	for url, want := range map[string]int{"/q?qty=4": 204, "/q?qty=3": 422, "/q?qty=0": 422, "/q?id=a&id=b": 204, "/q?id=a&id=a": 422} {
		if rec := call(api, "GET", url, ""); rec.Code != want {
			t.Errorf("%s: got %d, want %d (%s)", url, rec.Code, want, rec.Body)
		}
	}

	step := dig(specJSON(t, api), "components", "schemas", "numBody", "properties", "step", "multipleOf")
	if step != 5.0 || dig(specJSON(t, api), "components", "schemas", "numBody", "properties", "tags", "uniqueItems") != true ||
		dig(specJSON(t, api), "components", "schemas", "numBody", "properties", "score", "exclusiveMaximum") != 10.0 {
		t.Errorf("tags missing from the spec: %v", dig(specJSON(t, api), "components", "schemas", "numBody"))
	}

	mustPanic(t, "multipleOf 0", func() {
		server.Post(api, "/bad", func(ctx context.Context, in *struct {
			Body struct {
				N int `json:"n" multipleOf:"0"`
			}
		}) (*empty, error) {
			return nil, nil
		})
	})
}

type addr struct {
	City string `json:"city"`
}

type patchBody struct {
	Name server.Optional[string]   `json:"name" minLength:"2"`
	Age  server.Optional[int]      `json:"age" minimum:"0"`
	Tags server.Optional[[]string] `json:"tags" enum:"a,b"`
	Nick server.Nullable[string]   `json:"nick" maxLength:"5" enum:"x,yy"`
	Home server.Nullable[addr]     `json:"home"`
}

func state[T any](set, null bool, v T) string {
	switch {
	case !set:
		return "unset"
	case null:
		return "null"
	}
	return fmt.Sprint(v)
}

func TestOptionalAndNullable(t *testing.T) {
	api := server.New("t", "1")
	server.Patch(api, "/p", func(ctx context.Context, in *struct{ Body patchBody }) (*struct{ Body map[string]string }, error) {
		b := in.Body
		return &struct{ Body map[string]string }{map[string]string{
			"name": state(b.Name.Set, false, b.Name.Value), "age": state(b.Age.Set, false, b.Age.Value),
			"tags": state(b.Tags.Set, false, b.Tags.Value), "nick": state(b.Nick.Set, b.Nick.Null, b.Nick.Value),
			"home": state(b.Home.Set, b.Home.Null, b.Home.Value.City),
		}}, nil
	})
	server.Get(api, "/q", func(ctx context.Context, in *struct {
		Limit server.Optional[int] `query:"limit" minimum:"0"`
	}) (*struct{ Body string }, error) {
		return &struct{ Body string }{state(in.Limit.Set, false, in.Limit.Value)}, nil
	})

	got := func(rec *httptest.ResponseRecorder) string {
		var m map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			return fmt.Sprintf("%d %s", rec.Code, rec.Body)
		}
		return fmt.Sprintf("name=%s age=%s tags=%s nick=%s home=%s", m["name"], m["age"], m["tags"], m["nick"], m["home"])
	}
	for body, want := range map[string]string{
		`{}`:                                  "name=unset age=unset tags=unset nick=unset home=unset",
		`{"name":"Ada","age":0}`:              "name=Ada age=0 tags=unset nick=unset home=unset",
		`{"nick":null,"home":null}`:           "name=unset age=unset tags=unset nick=null home=null",
		`{"nick":"x","home":{"city":"Oslo"}}`: "name=unset age=unset tags=unset nick=x home=Oslo",
		`{"tags":["a","b"]}`:                  "name=unset age=unset tags=[a b] nick=unset home=unset",
	} {
		if g := got(call(api, "PATCH", "/p", body)); g != want {
			t.Errorf("%s\n got %s\nwant %s", body, g, want)
		}
	}

	for body, loc := range map[string]string{
		`{"name":null}`:       "body.name",
		`{"age":null}`:        "body.age",
		`{"name":"A"}`:        "body.name",
		`{"age":-1}`:          "body.age",
		`{"tags":["c"]}`:      "body.tags[0]",
		`{"nick":"zz"}`:       "body.nick",
		`{"nick":"toolong"}`:  "body.nick",
		`{"home":{"city":5}}`: "body.home.city",
	} {
		rec := call(api, "PATCH", "/p", body)
		if rec.Code != 422 || !slices.Contains(errorLocations(t, rec), loc) {
			t.Errorf("%s: got %d %s, want 422 at %s", body, rec.Code, rec.Body, loc)
		}
	}

	for url, want := range map[string]string{"/q": `"unset"`, "/q?limit=0": `"0"`, "/q?limit=7": `"7"`} {
		if rec := call(api, "GET", url, ""); strings.TrimSpace(rec.Body.String()) != want {
			t.Errorf("%s: got %s, want %s", url, rec.Body, want)
		}
	}
	if rec := call(api, "GET", "/q?limit=-1", ""); rec.Code != 422 {
		t.Errorf("limit=-1: %d", rec.Code)
	}

	schema := dig(specJSON(t, api), "components", "schemas", "patchBody")
	if dig(schema, "required") != nil {
		t.Errorf("wrapper fields must be optional: %v", dig(schema, "required"))
	}
	props := func(name string, path ...string) any {
		return dig(schema, append([]string{"properties", name}, path...)...)
	}
	if props("name", "type") != "string" || props("age", "type") != "integer" || props("tags", "items", "enum") == nil {
		t.Errorf("optional fields must keep their schema and tags: %v", dig(schema, "properties"))
	}
	if fmt.Sprint(props("nick", "type")) != "[string null]" || fmt.Sprint(props("nick", "enum")) != "[x yy <nil>]" {
		t.Errorf("nullable string: %v", props("nick"))
	}
	if ref := fmt.Sprint(props("home", "anyOf")); !strings.Contains(ref, "#/components/schemas/addr") || !strings.Contains(ref, "null") {
		t.Errorf("nullable object must be anyOf ref+null: %v", props("home"))
	}
	if q := dig(specJSON(t, api), "paths", "/q", "get", "parameters"); fmt.Sprint(dig(q.([]any)[0], "required")) != "<nil>" {
		t.Errorf("optional query param must not be required: %v", q)
	}

	b, _ := json.Marshal(struct {
		A server.Optional[int] `json:"a,omitzero"`
		B server.Nullable[int] `json:"b,omitzero"`
		C server.Nullable[int] `json:"c"`
		D server.Nullable[int] `json:"d"`
		E server.Optional[int] `json:"e,omitzero"`
	}{C: server.Nullable[int]{Set: true, Null: true}, D: server.Nullable[int]{Set: true, Value: 3}, E: server.Optional[int]{Set: true}})
	if string(b) != `{"c":null,"d":3,"e":0}` {
		t.Errorf("marshal: %s", b)
	}
}

func TestRequestDecoders(t *testing.T) {
	api := server.New("t", "1")
	api.Decoders["application/x-kv"] = func(b []byte) (any, error) {
		m := map[string]any{}
		for _, kv := range strings.Split(string(b), ";") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return nil, errors.New("bad pair " + kv)
			}
			if n, err := strconv.Atoi(v); err == nil {
				m[k] = n
			} else {
				m[k] = v
			}
		}
		return m, nil
	}
	type body struct {
		Name  string `json:"name" minLength:"2"`
		Count int    `json:"count" minimum:"1"`
	}
	server.Post(api, "/k", func(ctx context.Context, in *struct{ Body body }) (*struct{ Body string }, error) {
		return &struct{ Body string }{fmt.Sprintf("%s:%d", in.Body.Name, in.Body.Count)}, nil
	})

	for _, c := range []struct {
		ctype, body string
		code        int
		want        string
	}{
		{"application/x-kv", "name=Ada;count=3", 200, `"Ada:3"`},
		{"application/x-kv; charset=utf-8", "name=Ada;count=3", 200, `"Ada:3"`},
		{"APPLICATION/X-KV", "name=Ada;count=3", 200, `"Ada:3"`},
		{"application/x-kv", "name=Ada;count=0", 422, "body.count"},
		{"application/x-kv", "name=Ada;count", 400, "bad pair"},
		{"application/x-kv", `{"name":"Ada","count":3}`, 400, "bad pair"},
		{"application/json", `{"name":"Ada","count":3}`, 200, `"Ada:3"`},
		{"application/merge-patch+json", `{"name":"Ada","count":3}`, 200, `"Ada:3"`},
		{"text/plain", `{"name":"Ada","count":3}`, 200, `"Ada:3"`},
		{"", `{"name":"Ada","count":3}`, 200, `"Ada:3"`},
	} {
		var hdr []string
		if c.ctype != "" {
			hdr = []string{"Content-Type", c.ctype}
		}
		rec := call(api, "POST", "/k", c.body, hdr...)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%q %q: got %d %s, want %d containing %s", c.ctype, c.body, rec.Code, rec.Body, c.code, c.want)
		}
	}
	content := dig(specJSON(t, api), "paths", "/k", "post", "requestBody", "content").(map[string]any)
	if _, ok := content["application/json"]; !ok || content["application/x-kv"] == nil || len(content) != 2 {
		t.Errorf("request media types in spec: %v", content)
	}
}

type accepted struct {
	JobID string `json:"job_id"`
}

type created struct {
	ID int `json:"id"`
}

type multiOut struct {
	Status   int
	Location string `header:"Location"`
	Body     any
}

func TestMultipleResponses(t *testing.T) {
	api := server.New("t", "1")
	server.Post(api, "/jobs", func(ctx context.Context, in *struct {
		Async bool `query:"async"`
	}) (*multiOut, error) {
		if in.Async {
			return &multiOut{Status: 202, Body: accepted{JobID: "j1"}}, nil
		}
		return &multiOut{Location: "/jobs/1", Body: created{ID: 1}}, nil
	}, func(o *server.Operation) {
		o.DefaultStatus = 201
		o.Responses = []server.Response{
			{Status: 201, Description: "Created now", Body: created{}},
			{Status: 202, Body: accepted{}},
			{Status: 204, Description: "Nothing to do"},
		}
	})

	rec := call(api, "POST", "/jobs?async=true", "")
	if rec.Code != 202 || strings.TrimSpace(rec.Body.String()) != `{"job_id":"j1"}` {
		t.Fatalf("async: %d %s", rec.Code, rec.Body)
	}
	rec = call(api, "POST", "/jobs", "")
	if rec.Code != 201 || rec.Header().Get("Location") != "/jobs/1" || strings.TrimSpace(rec.Body.String()) != `{"id":1}` {
		t.Fatalf("sync: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}

	spec := specJSON(t, api)
	resp := func(code string, path ...string) any {
		return dig(spec, append([]string{"paths", "/jobs", "post", "responses", code}, path...)...)
	}
	if resp("201", "description") != "Created now" ||
		fmt.Sprint(resp("201", "content", "application/json", "schema", "$ref")) != "#/components/schemas/created" ||
		resp("201", "headers", "Location") == nil {
		t.Errorf("201: %v", resp("201"))
	}
	if resp("202", "description") != "Accepted" || fmt.Sprint(resp("202", "content", "application/json", "schema", "$ref")) != "#/components/schemas/accepted" {
		t.Errorf("202: %v", resp("202"))
	}
	if resp("204", "description") != "Nothing to do" || resp("204", "content") != nil || resp("422") == nil {
		t.Errorf("204 without content, and 422 for the query param: %v %v", resp("204"), resp("422"))
	}
	for _, name := range []string{"created", "accepted"} {
		if dig(spec, "components", "schemas", name) == nil {
			t.Errorf("component %s missing", name)
		}
	}

	mustPanic(t, "status 99", func() {
		server.Get(api, "/bad", noop, func(o *server.Operation) { o.Responses = []server.Response{{Status: 99}} })
	})
}

type thing struct {
	Name string                  `json:"name" minLength:"2" doc:"Display name"`
	Nick server.Nullable[string] `json:"nick"`
}

func TestOfflineDocs(t *testing.T) {
	api := server.New("Docs Test", "1.2")
	server.Post(api, "/things/{id}", func(ctx context.Context, in *struct {
		ID   int `path:"id" minimum:"1"`
		Body thing
	}) (*empty, error) {
		return nil, nil
	}, func(o *server.Operation) { o.Summary = "<script>alert(1)</script>" })

	rec := call(api, "GET", "/docs", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "unpkg.com") {
		t.Fatalf("default docs use the CDN: %d", rec.Code)
	}

	api.Docs = api.OfflineDocs()
	rec = call(api, "GET", "/docs", "")
	page := rec.Body.String()
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("offline docs: %d %v", rec.Code, rec.Header())
	}
	for _, want := range []string{
		"Docs Test", "1.2", "POST", "/things/{id}", "min 1", "min length 2", "Display name", "string | null",
		"Unprocessable Entity", `id="schema-thing"`, "&lt;script&gt;alert(1)&lt;/script&gt;",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("docs lack %q", want)
		}
	}
	for _, bad := range []string{"unpkg.com", "<script"} {
		if strings.Contains(page, bad) {
			t.Errorf("docs must be self-contained and escaped, found %q", bad)
		}
	}
}
