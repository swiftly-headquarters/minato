package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

type Scheme struct {
	Spec      map[string]any
	Challenge string
	Verify    func(ctx context.Context, r *http.Request, scopes []string) (context.Context, error)
}

func (a *API) AddScheme(name string, s Scheme) {
	if s.Verify == nil {
		panic(fmt.Sprintf("minato: AddScheme %q needs a Verify function (use BearerAuth for a scheme that is only documented)", name))
	}
	a.schemes[name] = s
	if s.Spec != nil {
		if a.Spec.Components.SecuritySchemes == nil {
			a.Spec.Components.SecuritySchemes = map[string]any{}
		}
		a.Spec.Components.SecuritySchemes[name] = s.Spec
	}
}

func BearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < len("Bearer ") || !strings.EqualFold(h[:len("Bearer ")], "Bearer ") {
		return "", false
	}
	token := strings.TrimSpace(h[len("Bearer "):])
	return token, token != ""
}

func (a *API) knownScheme(name string) bool {
	if _, ok := a.schemes[name]; ok {
		return true
	}
	_, ok := a.Spec.Components.SecuritySchemes[name]
	return ok
}

func (a *API) checkSecurity(where string, reqs []map[string][]string) {
	enforced, open := false, false
	for _, req := range reqs {
		alt := false
		for name := range req {
			if !a.knownScheme(name) {
				panic(fmt.Sprintf("minato: %s: security scheme %q is not registered (call AddScheme or BearerAuth first)", where, name))
			}
			if _, ok := a.schemes[name]; ok {
				alt = true
			}
		}
		if alt {
			enforced = true
		} else {
			open = true
		}
	}
	if enforced && open {
		panic(fmt.Sprintf("minato: %s: an alternative made only of documentation-only schemes (or empty) would let every request through next to an enforced one", where))
	}
}

func (a *API) enforce(reqs []map[string][]string, next http.Handler) http.Handler {
	if len(reqs) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var best error
		var challenge string
		for _, req := range reqs {
			ctx, err := a.verifyAll(r, req, &challenge)
			if err == nil {
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			if best == nil || isForbidden(err) {
				best = err
			}
		}
		if challenge != "" {
			w.Header().Set("WWW-Authenticate", challenge)
		}
		var em *ErrorModel
		if errors.As(best, &em) {
			WriteError(w, em)
			return
		}
		WriteError(w, NewError(http.StatusUnauthorized, "missing or invalid credentials"))
	})
}

func isForbidden(err error) bool {
	var em *ErrorModel
	return errors.As(err, &em) && em.Status == http.StatusForbidden
}

func (a *API) verifyAll(r *http.Request, req map[string][]string, challenge *string) (context.Context, error) {
	ctx := r.Context()
	for name, scopes := range req {
		s, known := a.schemes[name]
		if !known || s.Verify == nil {
			continue
		}
		next, err := s.Verify(ctx, r.WithContext(ctx), scopes)
		if err != nil {
			if *challenge == "" && s.Challenge != "" {
				*challenge = s.Challenge
			}
			return nil, fmt.Errorf("scheme %s: %w", name, err)
		}
		if next != nil {
			ctx = next
		}
	}
	return ctx, nil
}
