package servertest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type Client struct {
	Header  http.Header
	t       testing.TB
	handler http.Handler
}

func New(t testing.TB, h http.Handler) *Client {
	return &Client{Header: http.Header{}, t: t, handler: h}
}

type Response struct {
	*httptest.ResponseRecorder
	t testing.TB
}

func (c *Client) Do(method, path string, body any, headers ...string) *Response {
	c.t.Helper()
	if len(headers)%2 != 0 {
		c.t.Fatalf("servertest: headers must be key, value pairs, got %d strings", len(headers))
	}
	var rd io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	case io.Reader:
		rd = b
	default:
		data, err := json.Marshal(b)
		if err != nil {
			c.t.Fatalf("servertest: encoding request body %T: %v", body, err)
		}
		rd, contentType = bytes.NewReader(data), "application/json"
	}
	req := httptest.NewRequest(method, path, rd)
	for k, vs := range c.Header {
		req.Header[k] = append([]string(nil), vs...)
	}
	if contentType != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	return &Response{ResponseRecorder: rec, t: c.t}
}

func (c *Client) Get(path string, headers ...string) *Response {
	c.t.Helper()
	return c.Do(http.MethodGet, path, nil, headers...)
}

func (c *Client) Delete(path string, headers ...string) *Response {
	c.t.Helper()
	return c.Do(http.MethodDelete, path, nil, headers...)
}

func (c *Client) Post(path string, body any, headers ...string) *Response {
	c.t.Helper()
	return c.Do(http.MethodPost, path, body, headers...)
}

func (c *Client) Put(path string, body any, headers ...string) *Response {
	c.t.Helper()
	return c.Do(http.MethodPut, path, body, headers...)
}

func (c *Client) Patch(path string, body any, headers ...string) *Response {
	c.t.Helper()
	return c.Do(http.MethodPatch, path, body, headers...)
}

func (r *Response) Status(code int) *Response {
	r.t.Helper()
	if r.Code != code {
		r.t.Fatalf("servertest: status %d, want %d: %s", r.Code, code, r.Body)
	}
	return r
}

func Decode[T any](r *Response) T {
	r.t.Helper()
	var v T
	if err := json.Unmarshal(r.Body.Bytes(), &v); err != nil {
		r.t.Fatalf("servertest: decoding %s into %T: %v", r.Body, v, err)
	}
	return v
}
