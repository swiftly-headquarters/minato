package server

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime/multipart"
	"net"
	"net/http"
	"reflect"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type Operation struct {
	Method        string
	Path          string
	OperationID   string
	Summary       string
	Description   string
	Tags          []string
	Deprecated    bool
	DefaultStatus int
	MaxBodyBytes  int64
	Errors        []int
	Responses     []Response
	Security      []map[string][]string
}

type Response struct {
	Status      int
	Description string
	Body        any
}

type Middleware func(http.Handler) http.Handler

type API struct {
	Spec              *OpenAPI
	MaxBodyBytes      int64
	ShutdownTimeout   time.Duration
	ShutdownDelay     time.Duration
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ETag              bool
	TLS               *tls.Config
	OnStart           func(net.Addr)
	OnStop            func(context.Context)

	Formats  map[string]Encoder
	Decoders map[string]Decoder
	Docs     http.Handler
	mux      *http.ServeMux
	reg      *registry
	root     *Group
	mws      []Middleware
	handler  http.Handler
	draining atomic.Bool
	schemes  map[string]Scheme
}

func (a *API) Draining() bool { return a.draining.Load() }

func New(title, version string) *API {
	reg := &registry{schemas: map[string]*Schema{}, names: map[reflect.Type]string{}}
	a := &API{
		Spec: &OpenAPI{
			OpenAPI:    "3.1.0",
			Info:       Info{Title: title, Version: version},
			Paths:      map[string]map[string]*OperationDoc{},
			Components: Components{Schemas: reg.schemas},
		},
		MaxBodyBytes:      1 << 20,
		ShutdownTimeout:   10 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		IdleTimeout:       2 * time.Minute,
		Formats:           map[string]Encoder{jsonMIME: json.Marshal},
		Decoders:          map[string]Decoder{jsonMIME: decodeJSON},
		schemes:           map[string]Scheme{},
		Docs: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, docsHTML)
		}),
		mux: http.NewServeMux(),
		reg: reg,
	}
	a.root = &Group{api: a}
	a.rebuild()
	serveSpec := func(mime string, render func() ([]byte, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, err := render()
			if err != nil {
				WriteError(w, err)
				return
			}
			w.Header().Set("Content-Type", mime)
			w.Write(b)
		}
	}
	a.mux.HandleFunc("GET /openapi.json", serveSpec("application/json", a.SpecJSON))
	a.mux.HandleFunc("GET /openapi.yaml", serveSpec("application/yaml", a.SpecYAML))
	a.mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) { a.Docs.ServeHTTP(w, r) })
	return a
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.handler.ServeHTTP(w, r) }

func (a *API) Use(mws ...Middleware) {
	a.mws = append(a.mws, mws...)
	a.rebuild()
}

func (a *API) rebuild() { a.handler = recoverer(chain(a.mws, recoverer(a.mux))) }

func chain(mws []Middleware, h http.Handler) http.Handler {
	for _, m := range slices.Backward(mws) {
		h = m(h)
	}
	return h
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			slog.ErrorContext(r.Context(), "minato: panic", "panic", v, "method", r.Method, "path", r.URL.Path, "stack", string(debug.Stack()))
			WriteError(w, NewError(http.StatusInternalServerError, ""))
		}()
		next.ServeHTTP(w, r)
	})
}

type Router interface{ scope() *Group }

type Group struct {
	Tags     []string
	Security []map[string][]string
	api      *API
	prefix   string
	mws      []Middleware
}

func (a *API) scope() *Group   { return a.root }
func (g *Group) scope() *Group { return g }

func (a *API) Group(prefix string, mws ...Middleware) *Group { return a.root.Group(prefix, mws...) }

func (g *Group) Group(prefix string, mws ...Middleware) *Group {
	return &Group{
		Tags:     slices.Clone(g.Tags),
		Security: slices.Clone(g.Security),
		api:      g.api,
		prefix:   g.prefix + prefix,
		mws:      append(slices.Clone(g.mws), mws...),
	}
}

func (g *Group) Use(mws ...Middleware) { g.mws = append(g.mws, mws...) }

func Register[I, O any](r Router, op Operation, handler func(context.Context, *I) (*O, error)) {
	g := r.scope()
	api := g.api
	route := op.Path
	op.Path = g.prefix + route
	op.Tags = append(slices.Clone(g.Tags), op.Tags...)
	if op.Security == nil {
		op.Security = g.Security
	}
	for _, code := range op.Errors {
		if code < 400 || code > 599 {
			panic(fmt.Sprintf("minato: %s %s: error status %d is not 4xx or 5xx", op.Method, op.Path, code))
		}
	}
	for _, rs := range op.Responses {
		if rs.Status < 100 || rs.Status > 599 {
			panic(fmt.Sprintf("minato: %s %s: response status %d is not a valid HTTP status", op.Method, op.Path, rs.Status))
		}
	}
	for _, req := range op.Security {
		for name := range req {
			if !api.knownScheme(name) {
				panic(fmt.Sprintf("minato: %s %s: security scheme %q is not registered (call AddScheme or BearerAuth first)", op.Method, op.Path, name))
			}
		}
	}
	in := api.reg.input(reflect.TypeFor[I](), op.Path)
	out := api.reg.output(reflect.TypeFor[O]())
	if op.OperationID == "" {
		op.OperationID = defaultOperationID(op.Method, op.Path)
	}
	api.document(op, in, out)
	pattern := op.Method + " " + op.Path
	if route == "/" {
		pattern += "{$}"
	}
	api.mux.Handle(pattern, routed(op.Method+" "+op.Path, chain(g.mws, api.enforce(op.Security, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var mime string
		var enc Encoder
		if out.negotiated() {
			var ok bool
			if mime, enc, ok = api.negotiate(r.Header.Get("Accept")); !ok {
				writeError(r.Context(), w, NewError(http.StatusNotAcceptable, "available: "+strings.Join(slices.Sorted(maps.Keys(api.Formats)), ", ")))
				return
			}
		}
		if in.form {
			defer func() {
				if r.MultipartForm != nil {
					r.MultipartForm.RemoveAll()
				}
			}()
		}
		var input I
		maxBody := cmp.Or(op.MaxBodyBytes, api.MaxBodyBytes)
		if err := in.bind(api, w, r, reflect.ValueOf(&input).Elem(), maxBody); err != nil {
			writeError(r.Context(), w, err)
			return
		}
		o, err := handler(r.Context(), &input)
		if err != nil {
			writeError(r.Context(), w, err)
			return
		}
		out.write(w, r, reflect.ValueOf(o), op.DefaultStatus, mime, enc, len(api.Formats) > 1, api.ETag)
	})))))
}

func defaultOperationID(method, path string) string {
	parts := []string{strings.ToLower(method)}
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, "{") {
			seg = "by-" + strings.Trim(seg, "{}.")
		}
		if seg != "" {
			parts = append(parts, seg)
		}
	}
	return strings.Join(parts, "-")
}

var bytesType = reflect.TypeFor[[]byte]()

type param struct {
	in, name string
	index    []int
	required bool
	def      string
	hasDef   bool
	file     bool
	schema   *Schema
}

type field struct {
	index    []int
	required bool
	raw      bool
	stream   bool
	sse      bool
	schema   *Schema
}

type input struct {
	params    []param
	body      *field
	form      bool
	multipart bool
	formDoc   *Schema
	hook      bool
	bodyHook  bool
}

func (r *registry) input(t reflect.Type, path string) *input {
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("minato: input %s must be a struct", t))
	}
	in := &input{hook: implementsValidator(t)}
	for _, f := range reflect.VisibleFields(t) {
		if f.Anonymous || !f.IsExported() {
			continue
		}
		if f.Name == "Body" {
			req := f.Type.Kind() != reflect.Pointer && f.Tag.Get("required") != "false"
			in.body = &field{index: f.Index, required: req, raw: f.Type == bytesType, schema: r.fieldSchema(f)}
			in.bodyHook = implementsValidator(f.Type)
			continue
		}
		if f.Name == "Form" {
			r.formParams(in, f)
			continue
		}
		for _, loc := range []string{"path", "query", "header", "cookie"} {
			name := f.Tag.Get(loc)
			if name == "" {
				continue
			}
			p := param{in: loc, name: name, index: f.Index, schema: r.fieldSchema(f)}
			p.required = loc == "path" || f.Tag.Get("required") == "true"
			p.def, p.hasDef = f.Tag.Lookup("default")
			typ := p.schema.Type
			if typ == "array" && p.schema.Items != nil {
				typ = p.schema.Items.Type
			}
			if !scalarTypes[typ] {
				panic(fmt.Sprintf("minato: param %s (%s) must be a scalar or slice of scalars", name, f.Type))
			}
			if loc == "path" && !strings.Contains(path, "{"+name+"}") && !strings.Contains(path, "{"+name+"...}") {
				panic(fmt.Sprintf("minato: path param %q not in path %s", name, path))
			}
			in.params = append(in.params, p)
		}
	}
	if in.form && in.body != nil {
		panic(fmt.Sprintf("minato: input %s has both Body and Form", t))
	}
	return in
}

var scalarTypes = map[string]bool{"string": true, "integer": true, "number": true, "boolean": true}

func (in *input) bind(api *API, w http.ResponseWriter, r *http.Request, v reflect.Value, maxBody int64) error {
	reg := api.reg
	var errs []*ErrorDetail
	query := r.URL.Query()
	if in.form {
		if err := parseForm(w, r, maxBody); err != nil {
			return err
		}
	}
	for _, p := range in.params {
		var raw []string
		if p.file {
			var hs []*multipart.FileHeader
			if r.MultipartForm != nil {
				hs = r.MultipartForm.File[p.name]
			}
			switch fv := v.FieldByIndex(p.index); {
			case len(hs) == 0:
				if p.required {
					errs = append(errs, &ErrorDetail{Location: "form." + p.name, Message: "required"})
				}
			case fv.Kind() == reflect.Slice:
				fv.Set(reflect.ValueOf(hs))
			default:
				fv.Set(reflect.ValueOf(hs[0]))
			}
			continue
		}
		switch p.in {
		case "form":
			raw = r.PostForm[p.name]
		case "path":
			if s := r.PathValue(p.name); s != "" {
				raw = []string{s}
			}
		case "query":
			raw = query[p.name]
		case "header":
			raw = r.Header.Values(p.name)
		case "cookie":
			if c, err := r.Cookie(p.name); err == nil {
				raw = []string{c.Value}
			}
		}
		loc := p.in + "." + p.name
		if len(raw) == 0 {
			if !p.hasDef {
				if p.required {
					errs = append(errs, &ErrorDetail{Location: loc, Message: "required"})
				}
				continue
			}
			raw = []string{p.def}
		}
		val, err := paramValue(p.schema, raw)
		if err != nil {
			errs = append(errs, &ErrorDetail{Location: loc, Message: err.Error(), Value: raw[0]})
			continue
		}
		n := len(errs)
		if errs = reg.validate(p.schema, val, loc, errs); len(errs) > n {
			continue
		}
		if err := setJSON(v.FieldByIndex(p.index), val); err != nil {
			errs = append(errs, &ErrorDetail{Location: loc, Message: err.Error(), Value: raw[0]})
		}
	}

	if in.body != nil {
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				return NewError(http.StatusRequestEntityTooLarge, "request body too large")
			}
			return NewError(http.StatusBadRequest, "reading body: "+err.Error())
		}
		fv := v.FieldByIndex(in.body.index)
		switch {
		case len(data) == 0:
			if in.body.required {
				errs = append(errs, &ErrorDetail{Location: "body", Message: "required"})
			}
		case in.body.raw:
			fv.SetBytes(data)
		default:
			doc, err := api.decode(r.Header.Get("Content-Type"), data)
			if err != nil {
				return NewError(http.StatusBadRequest, "invalid request body: "+err.Error())
			}
			n := len(errs)
			errs = reg.validate(in.body.schema, doc, "body", errs)
			if len(errs) == n {
				if err := setJSON(fv, doc); err != nil {
					errs = append(errs, &ErrorDetail{Location: "body", Message: err.Error()})
				}
			}
		}
	}
	if len(errs) == 0 {
		errs = in.runValidators(r.Context(), v, errs)
	}
	if len(errs) > 0 {
		return NewError(http.StatusUnprocessableEntity, "validation failed", errs...)
	}
	return nil
}

func paramValue(s *Schema, raw []string) (any, error) {
	if s.Type != "array" {
		return scalarValue(s.Type, raw[0])
	}
	var out []any
	for _, r := range raw {
		for _, x := range strings.Split(r, ",") {
			v, err := scalarValue(s.Items.Type, x)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
	}
	return out, nil
}

func scalarValue(typ, x string) (any, error) {
	switch typ {
	case "integer", "number":
		var f float64
		if json.Unmarshal([]byte(x), &f) != nil {
			return nil, fmt.Errorf("expected %s", typ)
		}
		return json.Number(x), nil
	case "boolean":
		b, err := strconv.ParseBool(x)
		if err != nil {
			return nil, errors.New("expected boolean")
		}
		return b, nil
	}
	return x, nil
}

func setJSON(fv reflect.Value, val any) error {
	b, err := json.Marshal(val)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, fv.Addr().Interface())
}

type header struct {
	index  []int
	name   string
	schema *Schema
}

type output struct {
	status  []int
	headers []header
	body    *field
}

func (r *registry) output(t reflect.Type) *output {
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("minato: output %s must be a struct", t))
	}
	out := &output{}
	for _, f := range reflect.VisibleFields(t) {
		switch {
		case f.Anonymous || !f.IsExported():
		case f.Name == "Status" && f.Type.Kind() == reflect.Int:
			out.status = f.Index
		case f.Name == "Body":
			out.body = r.typeField(f.Type)
			out.body.index = f.Index
			if out.body.schema != nil {
				out.body.schema = r.fieldSchema(f)
			}
		case f.Tag.Get("header") != "":
			out.headers = append(out.headers, header{f.Index, f.Tag.Get("header"), r.fieldSchema(f)})
		}
	}
	return out
}

func (r *registry) typeField(t reflect.Type) *field {
	switch t {
	case bytesType:
		return &field{raw: true}
	case readerType, readCloserType:
		return &field{stream: true}
	case sseType:
		return &field{sse: true}
	}
	return &field{schema: r.schema(t)}
}

func (o *output) defaultStatus(def int) int {
	if def != 0 {
		return def
	}
	if o.body == nil {
		return http.StatusNoContent
	}
	return http.StatusOK
}

func (o *output) negotiated() bool {
	return o.body != nil && !o.body.raw && !o.body.stream && !o.body.sse
}

func (o *output) write(w http.ResponseWriter, r *http.Request, v reflect.Value, def int, mime string, enc Encoder, vary, etag bool) {
	if v.IsNil() {
		w.WriteHeader(cmp.Or(def, http.StatusNoContent))
		return
	}
	v = v.Elem()
	status := o.defaultStatus(def)
	if o.status != nil {
		status = cmp.Or(int(v.FieldByIndex(o.status).Int()), status)
	}
	for _, h := range o.headers {
		hv := v.FieldByIndex(h.index)
		if hv.IsZero() {
			continue
		}
		for hv.Kind() == reflect.Pointer {
			hv = hv.Elem()
		}
		if hv.Kind() == reflect.Slice {
			for i := range hv.Len() {
				w.Header().Add(h.name, fmt.Sprint(hv.Index(i).Interface()))
			}
		} else {
			w.Header().Set(h.name, fmt.Sprint(hv.Interface()))
		}
	}
	if o.body == nil {
		w.WriteHeader(status)
		return
	}
	bv := v.FieldByIndex(o.body.index)
	switch {
	case o.body.raw:
		setDefault(w, "Content-Type", "application/octet-stream")
		w.WriteHeader(status)
		w.Write(bv.Bytes())
	case o.body.stream:
		writeStream(w, r, bv, status)
	case o.body.sse:
		writeSSE(w, r, bv, status)
	default:
		b, err := enc(bv.Interface())
		if err != nil {
			writeError(r.Context(), w, err)
			return
		}
		if vary {
			w.Header().Add("Vary", "Accept")
		}
		if (etag || w.Header().Get("ETag") != "") && status == http.StatusOK && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			tag := w.Header().Get("ETag")
			if tag == "" {
				tag = etagOf(b)
				w.Header().Set("ETag", tag)
			}
			if etagMatches(r.Header.Get("If-None-Match"), tag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		setDefault(w, "Content-Type", mime)
		w.WriteHeader(status)
		w.Write(b)
	}
}

func setDefault(w http.ResponseWriter, key, value string) {
	if w.Header().Get(key) == "" {
		w.Header().Set(key, value)
	}
}
