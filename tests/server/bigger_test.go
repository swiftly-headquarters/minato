package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/swiftly-headquarters/minato/server"
)

func mustPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: expected a panic at registration", what)
		}
	}()
	f()
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

type formIn struct {
	Form struct {
		Name   string                  `form:"name" minLength:"2"`
		Age    int                     `form:"age" default:"18"`
		Tags   []string                `form:"tag"`
		Avatar *multipart.FileHeader   `form:"avatar"`
		Docs   []*multipart.FileHeader `form:"doc"`
	}
}

type formOut struct {
	Body struct {
		Name   string   `json:"name"`
		Age    int      `json:"age"`
		Tags   []string `json:"tags"`
		Avatar string   `json:"avatar"`
		Docs   []string `json:"docs"`
	}
}

func multipartBody(t *testing.T, fields [][2]string, files [][3]string) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		mw.WriteField(f[0], f[1])
	}
	for _, f := range files {
		fw, err := mw.CreateFormFile(f[0], f[1])
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(fw, f[2])
	}
	mw.Close()
	return buf.String(), mw.FormDataContentType()
}

func TestForms(t *testing.T) {
	api := server.New("t", "1")
	server.Post(api, "/form", func(ctx context.Context, in *formIn) (*formOut, error) {
		out := &formOut{}
		out.Body.Name, out.Body.Age, out.Body.Tags = in.Form.Name, in.Form.Age, in.Form.Tags
		if a := in.Form.Avatar; a != nil {
			f, err := a.Open()
			if err != nil {
				return nil, err
			}
			b, _ := io.ReadAll(f)
			f.Close()
			out.Body.Avatar = a.Filename + ":" + string(b)
		}
		for _, d := range in.Form.Docs {
			out.Body.Docs = append(out.Body.Docs, d.Filename)
		}
		return out, nil
	})
	server.Post(api, "/login", func(ctx context.Context, in *struct {
		Form struct {
			User string `form:"user" required:"true"`
		}
	}) (*empty, error) {
		return nil, nil
	})
	server.Post(api, "/need-file", func(ctx context.Context, in *struct {
		Form struct {
			File *multipart.FileHeader `form:"file" required:"true"`
		}
	}) (*empty, error) {
		return nil, nil
	})

	decode := func(rec *httptest.ResponseRecorder) map[string]any {
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("%v: %s", err, rec.Body)
		}
		return m
	}

	rec := call(api, "POST", "/form", "name=Ada&tag=a&tag=b", "Content-Type", "application/x-www-form-urlencoded")
	if m := decode(rec); rec.Code != 200 || m["name"] != "Ada" || m["age"] != 18.0 || len(m["tags"].([]any)) != 2 {
		t.Fatalf("urlencoded: %d %s", rec.Code, rec.Body)
	}

	body, ct := multipartBody(t, [][2]string{{"name", "Grace"}, {"age", "30"}},
		[][3]string{{"avatar", "me.png", "PNGDATA"}, {"doc", "a.txt", "A"}, {"doc", "b.txt", "B"}})
	rec = call(api, "POST", "/form", body, "Content-Type", ct)
	m := decode(rec)
	if rec.Code != 200 || m["name"] != "Grace" || m["age"] != 30.0 || m["avatar"] != "me.png:PNGDATA" || len(m["docs"].([]any)) != 2 {
		t.Fatalf("multipart: %d %s", rec.Code, rec.Body)
	}

	rec = call(api, "POST", "/form", "name=A", "Content-Type", "application/x-www-form-urlencoded")
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"form.name"`) {
		t.Fatalf("short name: %d %s", rec.Code, rec.Body)
	}
	if rec := call(api, "POST", "/form", `{"name":"Ada"}`, "Content-Type", "application/json"); rec.Code != 415 {
		t.Fatalf("json to a form route: got %d", rec.Code)
	}
	if rec := call(api, "POST", "/login", "", "Content-Type", "application/x-www-form-urlencoded"); rec.Code != 422 || !strings.Contains(rec.Body.String(), `"form.user"`) {
		t.Fatalf("required form field: %d %s", rec.Code, rec.Body)
	}
	if rec := call(api, "POST", "/login", ""); rec.Code != 422 {
		t.Fatalf("empty request without content type: %d", rec.Code)
	}
	body, ct = multipartBody(t, [][2]string{{"x", "y"}}, nil)
	if rec := call(api, "POST", "/need-file", body, "Content-Type", ct); rec.Code != 422 || !strings.Contains(rec.Body.String(), `"form.file"`) {
		t.Fatalf("required file: %d %s", rec.Code, rec.Body)
	}

	api.MaxBodyBytes = 200
	body, ct = multipartBody(t, nil, [][3]string{{"avatar", "big.bin", strings.Repeat("x", 4096)}})
	if rec := call(api, "POST", "/form", body, "Content-Type", ct); rec.Code != 413 {
		t.Fatalf("oversized upload: %d %s", rec.Code, rec.Body)
	}

	form := func(path, method string) map[string]*server.MediaType {
		return api.Spec.Paths[path][method].RequestBody.Content
	}
	if got := keys(form("/form", "post")); !slices.Equal(got, []string{"multipart/form-data"}) {
		t.Errorf("upload route must be multipart only, got %v", got)
	}
	if got := keys(form("/login", "post")); !slices.Equal(got, []string{"application/x-www-form-urlencoded", "multipart/form-data"}) {
		t.Errorf("plain form route must accept both, got %v", got)
	}
	s := form("/form", "post")["multipart/form-data"].Schema
	if s.Properties["avatar"].Format != "binary" || s.Properties["doc"].Items.Format != "binary" || s.Properties["age"].Type != "integer" {
		t.Errorf("bad form schema: %+v", s.Properties)
	}
	if rb := api.Spec.Paths["/login"]["post"].RequestBody; !rb.Required || strings.Join(rb.Content["multipart/form-data"].Schema.Required, ",") != "user" {
		t.Errorf("required form field not documented: %+v", rb)
	}
	if len(api.Spec.Paths["/form"]["post"].Parameters) != 0 {
		t.Error("form fields must not be listed as parameters")
	}

	mustPanic(t, "Body and Form", func() {
		server.Post(api, "/bad1", func(ctx context.Context, in *struct {
			Body struct {
				A int `json:"a"`
			}
			Form struct {
				B string `form:"b"`
			}
		}) (*empty, error) {
			return nil, nil
		})
	})
	mustPanic(t, "non-pointer file slice", func() {
		server.Post(api, "/bad2", func(ctx context.Context, in *struct {
			Form struct {
				F []multipart.FileHeader `form:"f"`
			}
		}) (*empty, error) {
			return nil, nil
		})
	})
	mustPanic(t, "object form field", func() {
		server.Post(api, "/bad3", func(ctx context.Context, in *struct {
			Form struct {
				F struct{ X int } `form:"f"`
			}
		}) (*empty, error) {
			return nil, nil
		})
	})
}

type streamOut struct {
	ContentType string `header:"Content-Type"`
	Body        io.Reader
}

type trackedReader struct {
	io.Reader
	closed bool
}

func (c *trackedReader) Close() error { c.closed = true; return nil }

type sseOut struct{ Body server.SSE }

func TestStreaming(t *testing.T) {
	api := server.New("t", "1")
	tr := &trackedReader{Reader: strings.NewReader("hello stream")}
	server.Get(api, "/stream", func(ctx context.Context, _ *empty) (*streamOut, error) {
		return &streamOut{ContentType: "text/plain", Body: tr}, nil
	})
	server.Get(api, "/events", func(ctx context.Context, _ *empty) (*sseOut, error) {
		return &sseOut{Body: func(ctx context.Context, send func(server.SSEEvent) error) error {
			send(server.SSEEvent{ID: "1", Event: "tick", Data: map[string]int{"n": 1}})
			send(server.SSEEvent{Data: "a\r\nb"})
			return send(server.SSEEvent{Event: "bad\nevent", Data: "x"})
		}}, nil
	})

	rec := call(api, "GET", "/stream", "")
	if rec.Code != 200 || rec.Body.String() != "hello stream" || rec.Header().Get("Content-Type") != "text/plain" || !tr.closed {
		t.Fatalf("stream: %d %q closed=%v", rec.Code, rec.Body, tr.closed)
	}
	tr = &trackedReader{Reader: strings.NewReader("never sent")}
	if rec := call(api, "HEAD", "/stream", ""); rec.Code != 200 || rec.Body.Len() != 0 || !tr.closed {
		t.Fatalf("HEAD must not send the body but must close the reader: %d %q closed=%v", rec.Code, rec.Body, tr.closed)
	}

	rec = call(api, "GET", "/events", "")
	want := "id: 1\nevent: tick\ndata: {\"n\":1}\n\ndata: a\ndata: b\n\n"
	if rec.Code != 200 || rec.Body.String() != want || rec.Header().Get("Content-Type") != "text/event-stream" || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("sse: %d %v\n got %q\nwant %q", rec.Code, rec.Header(), rec.Body, want)
	}

	if got := keys(api.Spec.Paths["/stream"]["get"].Responses["200"].Content); !slices.Equal(got, []string{"application/octet-stream"}) {
		t.Errorf("stream content types: %v", got)
	}
	if got := keys(api.Spec.Paths["/events"]["get"].Responses["200"].Content); !slices.Equal(got, []string{"text/event-stream"}) {
		t.Errorf("sse content types: %v", got)
	}
}

type greeting struct {
	Name string `json:"name" xml:"name"`
}

func TestContentNegotiation(t *testing.T) {
	api := server.New("t", "1")
	api.Formats["application/xml"] = xml.Marshal
	calls := 0
	server.Post(api, "/hi", func(ctx context.Context, _ *empty) (*struct{ Body greeting }, error) {
		calls++
		return &struct{ Body greeting }{greeting{"Ada"}}, nil
	})
	server.Delete(api, "/hi", noop)

	const js, xm = `{"name":"Ada"}`, `<greeting><name>Ada</name></greeting>`
	for _, c := range []struct {
		accept, body, ctype string
		code                int
	}{
		{"", js, "application/json", 200},
		{"*/*", js, "application/json", 200},
		{"application/*", js, "application/json", 200},
		{"application/xml", xm, "application/xml", 200},
		{"application/xml;q=0.5, application/json", js, "application/json", 200},
		{"application/json;q=0.1, application/xml;q=0.9", xm, "application/xml", 200},
		{"application/json;q=0, */*;q=0.1", xm, "application/xml", 200},
		{"text/html, */*;q=0.1", js, "application/json", 200},
		{"text/html", "", "application/problem+json", 406},
		{"application/xml;q=0", "", "application/problem+json", 406},
	} {
		calls = 0
		hdr := []string{}
		if c.accept != "" {
			hdr = []string{"Accept", c.accept}
		}
		rec := call(api, "POST", "/hi", "", hdr...)
		got := strings.TrimSpace(rec.Body.String())
		if rec.Code != c.code || rec.Header().Get("Content-Type") != c.ctype || (c.body != "" && got != c.body) {
			t.Errorf("Accept %q: got %d %s %q", c.accept, rec.Code, rec.Header().Get("Content-Type"), got)
		}
		if c.code == 406 && calls != 0 {
			t.Errorf("Accept %q: handler ran despite 406", c.accept)
		}
		if c.code == 200 && rec.Header().Get("Vary") != "Accept" {
			t.Errorf("Accept %q: missing Vary: Accept", c.accept)
		}
	}
	if rec := call(api, "DELETE", "/hi", "", "Accept", "text/html"); rec.Code != 204 {
		t.Errorf("a route without a body ignores Accept, got %d", rec.Code)
	}
	if got := keys(api.Spec.Paths["/hi"]["post"].Responses["200"].Content); !slices.Equal(got, []string{"application/json", "application/xml"}) {
		t.Errorf("spec content types: %v", got)
	}
}

type shade struct{ r, g, b uint8 }

func (shade) Schema() *server.Schema { return &server.Schema{Type: "string", Format: "color"} }

type opaque struct{ v int }

func (opaque) MarshalJSON() ([]byte, error) { return []byte("1"), nil }

type probe struct {
	Hooked    shade         `json:"hooked"`
	HookedPtr *shade        `json:"hooked_ptr"`
	Tagged    shade         `json:"tagged" doc:"has a doc"`
	Addr      netip.Addr    `json:"addr"`
	Opaque    opaque        `json:"opaque"`
	Dur       time.Duration `json:"dur"`
}

func TestSchemaHook(t *testing.T) {
	api := server.New("t", "1")
	server.Get(api, "/probe", func(ctx context.Context, _ *empty) (*struct{ Body probe }, error) { return nil, nil })
	server.Get(api, "/ip", func(ctx context.Context, in *struct {
		IP netip.Addr `query:"ip"`
	}) (*struct {
		Body struct {
			IP netip.Addr `json:"ip"`
		}
	}, error) {
		out := &struct {
			Body struct {
				IP netip.Addr `json:"ip"`
			}
		}{}
		out.Body.IP = in.IP
		return out, nil
	})

	p := api.Spec.Components.Schemas["probe"].Properties
	for name, want := range map[string][2]string{
		"hooked": {"string", "color"}, "hooked_ptr": {"string", "color"}, "tagged": {"string", "color"},
		"addr": {"string", ""}, "opaque": {"", ""}, "dur": {"integer", "int64"},
	} {
		if p[name].Type != want[0] || p[name].Format != want[1] {
			t.Errorf("%s: got type %q format %q, want %v", name, p[name].Type, p[name].Format, want)
		}
	}
	if p["tagged"].Description != "has a doc" || p["hooked"].Description != "" {
		t.Errorf("field tags must apply to a copy: tagged=%q hooked=%q", p["tagged"].Description, p["hooked"].Description)
	}

	rec := call(api, "GET", "/ip?ip=10.0.0.1", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"ip":"10.0.0.1"}` {
		t.Fatalf("text-marshaler param: %d %s", rec.Code, rec.Body)
	}
	if rec := call(api, "GET", "/ip?ip=nope", ""); rec.Code != 422 {
		t.Fatalf("invalid text value: got %d %s", rec.Code, rec.Body)
	}
}

func TestHeadAndOptionsHelpers(t *testing.T) {
	api := server.New("t", "1")
	type out = struct {
		Which string `header:"X-Which"`
	}
	handler := func(which string) func(context.Context, *empty) (*out, error) {
		return func(context.Context, *empty) (*out, error) { return &out{Which: which}, nil }
	}
	server.Get(api, "/r", handler("get"))
	server.Head(api, "/r", handler("head"))
	server.Options(api, "/r", handler("options"))
	server.Get(api, "/only", handler("get"))

	for method, want := range map[string]string{"GET": "get", "HEAD": "head", "OPTIONS": "options"} {
		if rec := call(api, method, "/r", ""); rec.Header().Get("X-Which") != want {
			t.Errorf("%s /r handled by %q, want %q", method, rec.Header().Get("X-Which"), want)
		}
	}
	if rec := call(api, "HEAD", "/only", ""); rec.Header().Get("X-Which") != "get" {
		t.Errorf("HEAD must fall back to the GET route, got %d", rec.Code)
	}
	if api.Spec.Paths["/r"]["head"] == nil || api.Spec.Paths["/r"]["options"] == nil {
		t.Error("head and options operations missing from the spec")
	}
}
