package server

import (
	"context"
	"net/http"
	"strings"
)

func Get[I, O any](r Router, path string, h func(context.Context, *I) (*O, error), opts ...func(*Operation)) {
	register(r, http.MethodGet, path, h, opts)
}

func Post[I, O any](r Router, path string, h func(context.Context, *I) (*O, error), opts ...func(*Operation)) {
	register(r, http.MethodPost, path, h, opts)
}

func Put[I, O any](r Router, path string, h func(context.Context, *I) (*O, error), opts ...func(*Operation)) {
	register(r, http.MethodPut, path, h, opts)
}

func Patch[I, O any](r Router, path string, h func(context.Context, *I) (*O, error), opts ...func(*Operation)) {
	register(r, http.MethodPatch, path, h, opts)
}

func Delete[I, O any](r Router, path string, h func(context.Context, *I) (*O, error), opts ...func(*Operation)) {
	register(r, http.MethodDelete, path, h, opts)
}

func Head[I, O any](r Router, path string, h func(context.Context, *I) (*O, error), opts ...func(*Operation)) {
	register(r, http.MethodHead, path, h, opts)
}

func Options[I, O any](r Router, path string, h func(context.Context, *I) (*O, error), opts ...func(*Operation)) {
	register(r, http.MethodOptions, path, h, opts)
}

func Handle(r Router, pattern string, h http.Handler) {
	g := r.scope()
	method, path, hasMethod := strings.Cut(pattern, " ")
	if !hasMethod {
		method, path = "", pattern
	}
	p := g.prefix + path
	if method != "" {
		p = method + " " + p
	}
	g.api.mux.Handle(p, routed(p, chain(g.mws, h)))
}

func register[I, O any](r Router, method, path string, h func(context.Context, *I) (*O, error), opts []func(*Operation)) {
	op := Operation{Method: method, Path: path}
	for _, o := range opts {
		o(&op)
	}
	Register(r, op, h)
}

func Secured(scheme string, scopes ...string) []map[string][]string {
	return []map[string][]string{{scheme: append([]string{}, scopes...)}}
}

func (a *API) BearerAuth(name string) {
	if a.Spec.Components.SecuritySchemes == nil {
		a.Spec.Components.SecuritySchemes = map[string]any{}
	}
	a.Spec.Components.SecuritySchemes[name] = map[string]any{"type": "http", "scheme": "bearer"}
}
