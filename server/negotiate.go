package server

import (
	"maps"
	"slices"
	"strconv"
	"strings"
)

const jsonMIME = "application/json"

type Encoder func(v any) ([]byte, error)

func (a *API) negotiate(accept string) (string, Encoder, bool) {
	if strings.TrimSpace(accept) == "" {
		accept = "*/*"
	}
	best, bestQ := "", 0.0
	for _, name := range slices.Sorted(maps.Keys(a.Formats)) {
		if q := acceptQ(accept, name); q > bestQ || (q == bestQ && q > 0 && rank(name) < rank(best)) {
			best, bestQ = name, q
		}
	}
	return best, a.Formats[best], bestQ > 0
}

func rank(name string) int {
	switch name {
	case jsonMIME:
		return 0
	case protoMIME:
		return 2
	}
	return 1
}

func acceptQ(accept, mt string) float64 {
	q, spec := 0.0, -1
	typ, _, _ := strings.Cut(mt, "/")
	for _, part := range strings.Split(accept, ",") {
		rng, params, _ := strings.Cut(part, ";")
		rng = strings.ToLower(strings.TrimSpace(rng))
		s := -1
		switch rng {
		case mt:
			s = 2
		case typ + "/*":
			s = 1
		case "*/*":
			s = 0
		}
		if s <= spec {
			continue
		}
		v := 1.0
		for _, p := range strings.Split(params, ";") {
			if k, val, ok := strings.Cut(strings.TrimSpace(p), "="); ok && strings.EqualFold(k, "q") {
				if f, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
					v = f
				}
			}
		}
		q, spec = v, s
	}
	return q
}
