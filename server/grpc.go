package server

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	grpcCancelled        = 1
	grpcUnknown          = 2
	grpcInvalidArgument  = 3
	grpcDeadlineExceeded = 4
	grpcNotFound         = 5
	grpcPermissionDenied = 7
	grpcExhausted        = 8
	grpcPrecondition     = 9
	grpcAborted          = 10
	grpcUnimplemented    = 12
	grpcInternal         = 13
	grpcUnavailable      = 14
	grpcUnauthenticated  = 16
)

type msgField struct {
	index  []int
	num    int
	name   string
	schema *Schema
}

type grpcMethod struct {
	name      string
	op        Operation
	in        *input
	out       *output
	req       *Schema
	resp      *Schema
	reqNames  []string
	respNames []string
	protoErr  error
}

func protoIdent(name string) string {
	var b strings.Builder
	for _, c := range name {
		if c < unicode.MaxASCII && (unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_') {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" || unicode.IsDigit(rune(s[0])) {
		s = "_" + s
	}
	return s
}

func camel(name string) string {
	var b strings.Builder
	up := true
	for _, c := range name {
		switch {
		case c <= unicode.MaxASCII && (unicode.IsLetter(c) || unicode.IsDigit(c)):
			if up {
				c = unicode.ToUpper(c)
			}
			b.WriteRune(c)
			up = false
		default:
			up = true
		}
	}
	s := b.String()
	if s == "" || unicode.IsDigit(rune(s[0])) {
		s = "M" + s
	}
	return s
}

func buildMessage(fields []msgField) (*Schema, []string) {
	names := make([]string, len(fields))
	used := map[string]bool{}
	for i, f := range fields {
		names[i] = uniqueIdent(protoIdent(f.name), used)
	}
	idx := make([]int, len(fields))
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(a, b int) int { return slices.Compare(fields[a].index, fields[b].index) })
	s := &Schema{Type: "object", Properties: map[string]*Schema{}}
	explicit := map[string]int{}
	for _, i := range idx {
		s.order = append(s.order, names[i])
		s.Properties[names[i]] = fields[i].schema
		if fields[i].num != 0 {
			explicit[names[i]] = fields[i].num
		}
	}
	s.number(explicit)
	return s, names
}

func bodySchema(f *field) *Schema {
	if f.schema == nil {
		return &Schema{Type: "string", Format: "byte"}
	}
	return f.schema
}

func (a *API) addGRPC(op Operation, in *input, out *output) {
	if in.form || (out.body != nil && (out.body.stream || out.body.sse)) {
		return
	}
	m := &grpcMethod{name: camel(op.OperationID), op: op, in: in, out: out}
	if other, dup := a.grpcMethods[m.name]; dup {
		panic(fmt.Sprintf("minato: %s %s and %s %s both become the gRPC method %s, give one a different OperationID", op.Method, op.Path, other.op.Method, other.op.Path, m.name))
	}
	var req, resp []msgField
	for _, p := range in.params {
		req = append(req, msgField{p.index, p.num, p.name, p.schema})
	}
	if in.body != nil {
		req = append(req, msgField{in.body.index, in.body.num, "body", bodySchema(in.body)})
	}
	for _, h := range out.headers {
		resp = append(resp, msgField{h.index, h.num, h.name, h.schema})
	}
	if out.body != nil {
		resp = append(resp, msgField{out.body.index, out.body.num, "body", bodySchema(out.body)})
	}
	m.req, m.reqNames = buildMessage(req)
	m.resp, m.respNames = buildMessage(resp)
	m.protoErr = cmp.Or(a.reg.protoSupport(m.req, "request"), a.reg.protoSupport(m.resp, "response"))
	a.grpcMethods[m.name] = m
}

func isGRPC(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if len(ct) < len("application/grpc") || !strings.EqualFold(ct[:len("application/grpc")], "application/grpc") {
		return false
	}
	mt, _, _ := mime.ParseMediaType(ct)
	return mt == "application/grpc" || strings.HasPrefix(mt, "application/grpc+")
}

func grpcEscape(msg string) string {
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	var b strings.Builder
	for i := range len(msg) {
		c := msg[i]
		if c >= 0x20 && c <= 0x7e && c != '%' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func grpcFail(w http.ResponseWriter, ct string, code int, msg string, meta http.Header) {
	h := w.Header()
	h.Set("Content-Type", ct)
	if id := meta.Get("X-Request-Id"); id != "" {
		h.Set("X-Request-Id", id)
	}
	h.Set("Grpc-Status", strconv.Itoa(code))
	if msg != "" {
		h.Set("Grpc-Message", grpcEscape(msg))
	}
	w.WriteHeader(http.StatusOK)
}

func grpcOK(w http.ResponseWriter, ct string, payload []byte, meta http.Header) {
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Grpc-Accept-Encoding", "identity,gzip")
	if id := meta.Get("X-Request-Id"); id != "" {
		h.Set("X-Request-Id", id)
	}
	w.WriteHeader(http.StatusOK)
	frame := make([]byte, 5, 5+len(payload))
	binary.BigEndian.PutUint32(frame[1:], uint32(len(payload)))
	w.Write(append(frame, payload...))
	h.Set(http.TrailerPrefix+"Grpc-Status", "0")
}

func httpToGRPC(status int) int {
	switch status {
	case http.StatusUnauthorized:
		return grpcUnauthenticated
	case http.StatusForbidden:
		return grpcPermissionDenied
	case http.StatusNotFound:
		return grpcNotFound
	case http.StatusConflict:
		return grpcAborted
	case http.StatusPreconditionFailed:
		return grpcPrecondition
	case http.StatusRequestEntityTooLarge, http.StatusTooManyRequests:
		return grpcExhausted
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return grpcDeadlineExceeded
	case 499:
		return grpcCancelled
	case http.StatusNotImplemented:
		return grpcUnimplemented
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return grpcUnavailable
	case http.StatusBadRequest, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity, http.StatusNotAcceptable:
		return grpcInvalidArgument
	}
	switch {
	case status >= 500:
		return grpcInternal
	case status >= 400:
		return grpcPrecondition
	}
	return grpcUnknown
}

func problemMessage(body []byte, status int) string {
	var em ErrorModel
	if json.Unmarshal(body, &em) != nil {
		return http.StatusText(status)
	}
	msg := cmp.Or(em.Detail, em.Title, http.StatusText(status))
	for _, e := range em.Errors {
		msg += "; " + e.Location + ": " + e.Message
	}
	return msg
}

func parseGRPCTimeout(v string) (time.Duration, bool) {
	if len(v) < 2 || len(v) > 9 {
		return 0, false
	}
	n, err := strconv.ParseUint(v[:len(v)-1], 10, 32)
	if err != nil {
		return 0, false
	}
	unit := map[byte]time.Duration{'H': time.Hour, 'M': time.Minute, 'S': time.Second, 'm': time.Millisecond, 'u': time.Microsecond, 'n': time.Nanosecond}[v[len(v)-1]]
	if unit == 0 || n > uint64(math.MaxInt64/int64(unit)) {
		return 0, false
	}
	return time.Duration(n) * unit, true
}

func readGRPCMessage(body io.Reader, limit int64, encoding string) ([]byte, int, error) {
	var prefix [5]byte
	if _, err := io.ReadFull(body, prefix[:]); err != nil {
		if err == io.EOF {
			return nil, 0, nil
		}
		return nil, grpcInvalidArgument, errors.New("truncated message")
	}
	n := binary.BigEndian.Uint32(prefix[1:])
	if int64(n) > limit {
		return nil, grpcExhausted, fmt.Errorf("message is larger than the %d byte limit", limit)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(body, payload); err != nil {
		return nil, grpcInvalidArgument, errors.New("truncated message")
	}
	var extra [1]byte
	if m, _ := io.ReadFull(body, extra[:]); m > 0 {
		return nil, grpcInvalidArgument, errors.New("a unary call takes exactly one message")
	}
	switch prefix[0] {
	case 0:
	case 1:
		if encoding != "gzip" {
			return nil, grpcInternal, errors.New("compressed message without a supported grpc-encoding")
		}
		zr, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, grpcInvalidArgument, errors.New("invalid gzip message")
		}
		out, err := io.ReadAll(io.LimitReader(zr, limit+1))
		if err != nil {
			return nil, grpcInvalidArgument, errors.New("invalid gzip message")
		}
		if int64(len(out)) > limit {
			return nil, grpcExhausted, fmt.Errorf("message is larger than the %d byte limit", limit)
		}
		payload = out
	default:
		return nil, grpcInternal, errors.New("invalid message flag")
	}
	return payload, 0, nil
}

type grpcRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *grpcRecorder) Header() http.Header { return r.header }

func (r *grpcRecorder) WriteHeader(code int) {
	if r.status == 0 && code >= 200 {
		r.status = code
	}
}

func (r *grpcRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(p)
}

var grpcSkipHeaders = map[string]bool{
	"Content-Type": true, "Content-Length": true, "Te": true, "Accept": true, "Accept-Encoding": true,
	"Grpc-Timeout": true, "Grpc-Encoding": true, "Grpc-Accept-Encoding": true, "Grpc-Message-Type": true,
	"Transfer-Encoding": true, "Trailer": true, "Connection": true,
}

func scalarString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case json.Number:
		return x.String(), true
	case bool:
		return strconv.FormatBool(x), true
	}
	return "", false
}

func values(v any) ([]string, error) {
	items := []any{v}
	if a, ok := v.([]any); ok {
		items = a
	}
	out := make([]string, 0, len(items))
	for _, x := range items {
		s, ok := scalarString(x)
		if !ok {
			return nil, errors.New("expected a scalar value")
		}
		out = append(out, s)
	}
	return out, nil
}

func (m *grpcMethod) newRequest(ctx context.Context, r *http.Request, doc map[string]any) (*http.Request, error) {
	byName := map[string]string{}
	for i, p := range m.in.params {
		byName[p.in+"\x00"+p.name] = m.reqNames[i]
	}
	var decoded, escaped []string
	for _, seg := range strings.Split(m.op.Path, "/") {
		name, isWild := strings.CutPrefix(seg, "{")
		name, isWild = strings.CutSuffix(name, "}")
		if !isWild || name == "$" {
			decoded, escaped = append(decoded, seg), append(escaped, seg)
			continue
		}
		catchAll := strings.HasSuffix(name, "...")
		name = strings.TrimSuffix(name, "...")
		v, ok := doc[byName["path\x00"+name]]
		s, isScalar := scalarString(v)
		if !ok || !isScalar || s == "" {
			return nil, fmt.Errorf("path parameter %q is required", name)
		}
		decoded = append(decoded, s)
		if catchAll {
			parts := strings.Split(s, "/")
			for i := range parts {
				parts[i] = url.PathEscape(parts[i])
			}
			escaped = append(escaped, strings.Join(parts, "/"))
		} else {
			escaped = append(escaped, url.PathEscape(s))
		}
	}
	hdr := http.Header{}
	for k, vs := range r.Header {
		if !grpcSkipHeaders[k] && !strings.HasSuffix(strings.ToLower(k), "-bin") {
			hdr[k] = slices.Clone(vs)
		}
	}
	hdr.Set("Accept", jsonMIME)
	query := url.Values{}
	for i, p := range m.in.params {
		v, ok := doc[m.reqNames[i]]
		if !ok || v == nil || p.in == "path" {
			continue
		}
		vs, err := values(v)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", p.in, p.name, err)
		}
		for _, s := range vs {
			switch p.in {
			case "query":
				query.Add(p.name, s)
			case "header":
				hdr.Add(p.name, s)
			case "cookie":
				hdr.Add("Cookie", (&http.Cookie{Name: p.name, Value: s}).String())
			}
		}
	}
	var body io.ReadCloser = http.NoBody
	var length int64
	var bodyDoc any
	if m.in.body != nil {
		bodyDoc = doc[m.reqNames[len(m.reqNames)-1]]
	}
	if v := bodyDoc; v != nil {
		var b []byte
		if m.in.body.raw {
			s, _ := v.(string)
			raw, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				return nil, errors.New("body: invalid base64")
			}
			b = raw
			if hdr.Get("Content-Type") == "" {
				hdr.Set("Content-Type", "application/octet-stream")
			}
		} else {
			var err error
			if b, err = json.Marshal(v); err != nil {
				return nil, fmt.Errorf("body: %w", err)
			}
			hdr.Set("Content-Type", jsonMIME)
		}
		body, length = io.NopCloser(bytes.NewReader(b)), int64(len(b))
	}
	u := &url.URL{Path: strings.Join(decoded, "/"), RawPath: strings.Join(escaped, "/"), RawQuery: query.Encode()}
	req := &http.Request{
		Method: m.op.Method, URL: u, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: hdr, Body: body, ContentLength: length, Host: r.Host,
		RemoteAddr: r.RemoteAddr, TLS: r.TLS, RequestURI: u.RequestURI(),
	}
	return req.WithContext(ctx), nil
}

func (m *grpcMethod) responseDoc(rec *grpcRecorder) (map[string]any, error) {
	doc := map[string]any{}
	for i, h := range m.out.headers {
		raw := rec.header.Values(h.name)
		if len(raw) == 0 {
			continue
		}
		v, err := paramValue(h.schema, raw)
		if err != nil {
			return nil, fmt.Errorf("header %s: %w", h.name, err)
		}
		doc[m.respNames[i]] = v
	}
	if m.out.body == nil || rec.body.Len() == 0 {
		return doc, nil
	}
	name := m.respNames[len(m.respNames)-1]
	if m.out.body.raw {
		doc[name] = base64.StdEncoding.EncodeToString(rec.body.Bytes())
		return doc, nil
	}
	v, err := decodeJSON(rec.body.Bytes())
	if err != nil {
		return nil, fmt.Errorf("body: %w", err)
	}
	doc[name] = v
	return doc, nil
}

func (a *API) serveGRPC(w http.ResponseWriter, r *http.Request) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var jsonCodec bool
	switch mt {
	case "application/grpc", "application/grpc+proto":
	case "application/grpc+json":
		jsonCodec = true
	default:
		w.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	service, method, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	m := a.grpcMethods[method]
	if service != a.GRPCService || m == nil {
		grpcFail(w, mt, grpcUnimplemented, "unknown method "+r.URL.Path, nil)
		return
	}
	if m.protoErr != nil && !jsonCodec {
		grpcFail(w, mt, grpcUnimplemented, "protobuf is not available for this method, use application/grpc+json: "+m.protoErr.Error(), nil)
		return
	}
	encoding := strings.ToLower(r.Header.Get("Grpc-Encoding"))
	if encoding != "" && encoding != "identity" && encoding != "gzip" {
		w.Header().Set("Grpc-Accept-Encoding", "identity,gzip")
		grpcFail(w, mt, grpcUnimplemented, "unsupported grpc-encoding "+encoding, nil)
		return
	}
	payload, code, err := readGRPCMessage(r.Body, cmp.Or(m.op.MaxBodyBytes, a.MaxBodyBytes), encoding)
	if err != nil {
		grpcFail(w, mt, code, err.Error(), nil)
		return
	}
	doc := map[string]any{}
	if len(payload) > 0 {
		if jsonCodec {
			var v any
			if v, err = decodeJSON(payload); err == nil {
				var ok bool
				if doc, ok = v.(map[string]any); !ok {
					err = errors.New("expected a JSON object")
				} else {
					doc = a.reg.fromProtoJSON(m.req, doc, 0).(map[string]any)
				}
			}
		} else {
			doc, err = a.reg.decodeMessage(m.req, payload, 0)
		}
		if err != nil {
			grpcFail(w, mt, grpcInvalidArgument, "invalid request message: "+err.Error(), nil)
			return
		}
	}
	ctx := r.Context()
	if d, ok := parseGRPCTimeout(r.Header.Get("Grpc-Timeout")); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	req, err := m.newRequest(ctx, r, doc)
	if err != nil {
		grpcFail(w, mt, grpcInvalidArgument, "invalid request message: "+err.Error(), nil)
		return
	}
	rec := &grpcRecorder{header: http.Header{}}
	a.handler.ServeHTTP(rec, req)
	status := cmp.Or(rec.status, http.StatusOK)
	if status < 200 || status > 299 {
		code := httpToGRPC(status)
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			code = grpcDeadlineExceeded
		case errors.Is(ctx.Err(), context.Canceled):
			code = grpcCancelled
		}
		grpcFail(w, mt, code, problemMessage(rec.body.Bytes(), status), rec.header)
		return
	}
	resp, err := m.responseDoc(rec)
	if err == nil {
		if jsonCodec {
			payload, err = json.Marshal(resp)
		} else {
			payload, err = a.reg.encodeMessage(nil, m.resp, resp, 0)
		}
	}
	if err != nil {
		slog.ErrorContext(ctx, "minato: gRPC response encoding failed", "method", m.name, "err", err)
		grpcFail(w, mt, grpcInternal, "could not encode the response", rec.header)
		return
	}
	grpcOK(w, mt, payload, rec.header)
}
