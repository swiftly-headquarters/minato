package server

import (
	"encoding"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"mime/multipart"
	"net/mail"
	"net/netip"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const refPrefix = "#/components/schemas/"

type Schema struct {
	Ref                  string             `json:"$ref,omitempty"`
	Type                 string             `json:"type,omitempty"`
	Format               string             `json:"format,omitempty"`
	Description          string             `json:"description,omitempty"`
	Items                *Schema            `json:"items,omitempty"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	AdditionalProperties *Schema            `json:"additionalProperties,omitempty"`
	Required             []string           `json:"required,omitempty"`
	Enum                 []any              `json:"enum,omitempty"`
	Default              any                `json:"default,omitempty"`
	Examples             []any              `json:"examples,omitempty"`
	Minimum              *float64           `json:"minimum,omitempty"`
	Maximum              *float64           `json:"maximum,omitempty"`
	MinLength            *int               `json:"minLength,omitempty"`
	MaxLength            *int               `json:"maxLength,omitempty"`
	MinItems             *int               `json:"minItems,omitempty"`
	MaxItems             *int               `json:"maxItems,omitempty"`
	Pattern              string             `json:"pattern,omitempty"`

	re *regexp.Regexp
}

type registry struct {
	schemas map[string]*Schema
	names   map[reflect.Type]string
}

type Schemer interface{ Schema() *Schema }

var (
	timeType          = reflect.TypeFor[time.Time]()
	rawType           = reflect.TypeFor[json.RawMessage]()
	fileHeaderType    = reflect.TypeFor[multipart.FileHeader]()
	schemerType       = reflect.TypeFor[Schemer]()
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

func custom(t reflect.Type) *Schema {
	pt := reflect.PointerTo(t)
	switch {
	case t == fileHeaderType:
		return &Schema{Type: "string", Format: "binary"}
	case pt.Implements(schemerType):
		s := *reflect.New(t).Interface().(Schemer).Schema()
		return &s
	case pt.Implements(jsonMarshalerType):
		return &Schema{}
	case pt.Implements(textMarshalerType):
		return &Schema{Type: "string"}
	}
	return nil
}

func (r *registry) schema(t reflect.Type) *Schema {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t {
	case timeType:
		return &Schema{Type: "string", Format: "date-time"}
	case rawType:
		return &Schema{}
	}
	if s := custom(t); s != nil {
		return s
	}
	zero := 0.0
	switch t.Kind() {
	case reflect.Bool:
		return &Schema{Type: "boolean"}
	case reflect.Int8, reflect.Int16, reflect.Int32:
		return &Schema{Type: "integer", Format: "int32"}
	case reflect.Int, reflect.Int64:
		return &Schema{Type: "integer", Format: "int64"}
	case reflect.Uint8, reflect.Uint16, reflect.Uint32:
		return &Schema{Type: "integer", Format: "int32", Minimum: &zero}
	case reflect.Uint, reflect.Uint64, reflect.Uintptr:
		return &Schema{Type: "integer", Format: "int64", Minimum: &zero}
	case reflect.Float32:
		return &Schema{Type: "number", Format: "float"}
	case reflect.Float64:
		return &Schema{Type: "number", Format: "double"}
	case reflect.String:
		return &Schema{Type: "string"}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return &Schema{Type: "string", Format: "byte"}
		}
		return &Schema{Type: "array", Items: r.schema(t.Elem())}
	case reflect.Array:
		return &Schema{Type: "array", Items: r.schema(t.Elem())}
	case reflect.Map:
		return &Schema{Type: "object", AdditionalProperties: r.schema(t.Elem())}
	case reflect.Interface:
		return &Schema{}
	case reflect.Struct:
		if t.Name() == "" {
			return r.object(t)
		}
		name, ok := r.names[t]
		if !ok {
			name = r.uniqueName(t)
			r.names[t] = name
			r.schemas[name] = r.object(t)
		}
		return &Schema{Ref: refPrefix + name}
	}
	panic(fmt.Sprintf("minato: unsupported type %s", t))
}

func (r *registry) uniqueName(t reflect.Type) string {
	base := strings.Map(func(c rune) rune {
		if unicode.IsLetter(c) || unicode.IsDigit(c) {
			return c
		}
		return -1
	}, t.Name())
	name := base
	for i := 2; slices.Contains(slices.Collect(maps.Values(r.names)), name); i++ {
		name = base + strconv.Itoa(i)
	}
	return name
}

func (r *registry) object(t reflect.Type) *Schema {
	s := &Schema{Type: "object", Properties: map[string]*Schema{}}
	for _, f := range reflect.VisibleFields(t) {
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if f.Anonymous && name == "" && ft.Kind() == reflect.Struct {
			continue
		}
		if name == "" {
			name = f.Name
		}
		s.Properties[name] = r.fieldSchema(f)
		_, hasDefault := f.Tag.Lookup("default")
		optional := f.Type.Kind() == reflect.Pointer || hasDefault ||
			strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero")
		if req := f.Tag.Get("required"); req != "" {
			optional = req != "true"
		}
		if !optional {
			s.Required = append(s.Required, name)
		}
	}
	return s
}

func (r *registry) fieldSchema(f reflect.StructField) *Schema {
	s := *r.schema(f.Type)
	applyTags(&s, f)
	return &s
}

func applyTags(s *Schema, f reflect.StructField) {
	must := func(err error) {
		if err != nil {
			panic(fmt.Sprintf("minato: field %s: %v", f.Name, err))
		}
	}
	t := f.Type
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	tag := f.Tag
	if v := tag.Get("doc"); v != "" {
		s.Description = v
	}
	if v := tag.Get("format"); v != "" {
		s.Format = v
	}
	if v := tag.Get("pattern"); v != "" {
		re, err := regexp.Compile(v)
		must(err)
		s.Pattern, s.re = v, re
	}
	if v, ok := tag.Lookup("default"); ok {
		x, err := tagValue(t, v)
		must(err)
		s.Default = x
	}
	if v, ok := tag.Lookup("example"); ok {
		x, err := tagValue(t, v)
		must(err)
		s.Examples = []any{x}
	}
	if v := tag.Get("enum"); v != "" {
		target, et := s, t
		if s.Type == "array" {
			items := *s.Items
			target, et, s.Items = &items, t.Elem(), &items
		}
		for _, e := range strings.Split(v, ",") {
			x, err := tagValue(et, e)
			must(err)
			target.Enum = append(target.Enum, x)
		}
	}
	for name, dst := range map[string]**float64{"minimum": &s.Minimum, "maximum": &s.Maximum} {
		if v := tag.Get(name); v != "" {
			x, err := strconv.ParseFloat(v, 64)
			must(err)
			*dst = &x
		}
	}
	for name, dst := range map[string]**int{"minLength": &s.MinLength, "maxLength": &s.MaxLength, "minItems": &s.MinItems, "maxItems": &s.MaxItems} {
		if v := tag.Get(name); v != "" {
			x, err := strconv.Atoi(v)
			must(err)
			*dst = &x
		}
	}
}

func tagValue(t reflect.Type, v string) (any, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Bool:
		return strconv.ParseBool(v)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.ParseInt(v, 10, 64)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.ParseUint(v, 10, 64)
	case reflect.Float32, reflect.Float64:
		return strconv.ParseFloat(v, 64)
	case reflect.String:
		return v, nil
	case reflect.Slice, reflect.Array:
		if !strings.HasPrefix(v, "[") {
			var out []any
			for _, e := range strings.Split(v, ",") {
				x, err := tagValue(t.Elem(), e)
				if err != nil {
					return nil, err
				}
				out = append(out, x)
			}
			return out, nil
		}
	}
	var x any
	return x, json.Unmarshal([]byte(v), &x)
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

var formats = map[string]func(string) bool{
	"email":     func(s string) bool { a, err := mail.ParseAddress(s); return err == nil && a.Address == s },
	"uuid":      uuidRe.MatchString,
	"date-time": func(s string) bool { _, err := time.Parse(time.RFC3339, s); return err == nil },
	"date":      func(s string) bool { _, err := time.Parse(time.DateOnly, s); return err == nil },
	"uri":       func(s string) bool { u, err := url.Parse(s); return err == nil && u.Scheme != "" },
	"ipv4":      func(s string) bool { a, err := netip.ParseAddr(s); return err == nil && a.Is4() },
	"ipv6":      func(s string) bool { a, err := netip.ParseAddr(s); return err == nil && a.Is6() },
	"byte":      func(s string) bool { _, err := base64.StdEncoding.DecodeString(s); return err == nil },
}

func (r *registry) validate(s *Schema, v any, loc string, errs []*ErrorDetail) []*ErrorDetail {
	if s.Ref != "" {
		s = r.schemas[strings.TrimPrefix(s.Ref, refPrefix)]
	}
	add := func(format string, args ...any) {
		errs = append(errs, &ErrorDetail{Location: loc, Message: fmt.Sprintf(format, args...), Value: v})
	}
	switch s.Type {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			add("expected object")
			return errs
		}
		for name, ps := range s.Properties {
			if _, ok := m[name]; !ok && ps.Default != nil {
				m[name] = ps.Default
			}
		}
		for _, name := range s.Required {
			if m[name] == nil {
				errs = append(errs, &ErrorDetail{Location: loc + "." + name, Message: "required"})
			}
		}
		for _, k := range slices.Sorted(maps.Keys(m)) {
			ps := s.Properties[k]
			if ps == nil {
				ps = s.AdditionalProperties
			}
			if ps != nil && m[k] != nil {
				errs = r.validate(ps, m[k], loc+"."+k, errs)
			}
		}
	case "array":
		a, ok := v.([]any)
		if !ok {
			add("expected array")
			return errs
		}
		if s.MinItems != nil && len(a) < *s.MinItems {
			add("expected at least %d items", *s.MinItems)
		}
		if s.MaxItems != nil && len(a) > *s.MaxItems {
			add("expected at most %d items", *s.MaxItems)
		}
		for i, x := range a {
			if x != nil {
				errs = r.validate(s.Items, x, fmt.Sprintf("%s[%d]", loc, i), errs)
			}
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			add("expected string")
			return errs
		}
		n := utf8.RuneCountInString(str)
		if s.MinLength != nil && n < *s.MinLength {
			add("expected length >= %d", *s.MinLength)
		}
		if s.MaxLength != nil && n > *s.MaxLength {
			add("expected length <= %d", *s.MaxLength)
		}
		if s.re != nil && !s.re.MatchString(str) {
			add("expected string matching %s", s.Pattern)
		}
		if check := formats[s.Format]; check != nil && !check(str) {
			add("expected format %s", s.Format)
		}
	case "integer", "number":
		num, ok := v.(json.Number)
		f, err := num.Float64()
		if !ok || err != nil {
			add("expected %s", s.Type)
			return errs
		}
		if s.Type == "integer" && f != math.Trunc(f) {
			add("expected integer")
		}
		if s.Minimum != nil && f < *s.Minimum {
			add("expected number >= %v", *s.Minimum)
		}
		if s.Maximum != nil && f > *s.Maximum {
			add("expected number <= %v", *s.Maximum)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			add("expected boolean")
			return errs
		}
	}
	if len(s.Enum) > 0 && !slices.ContainsFunc(s.Enum, func(e any) bool { return fmt.Sprint(e) == fmt.Sprint(v) }) {
		add("expected one of %v", s.Enum)
	}
	return errs
}
