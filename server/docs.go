package server

import (
	"fmt"
	"html/template"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
)

func (a *API) OfflineDocs() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		if err := docsTemplate.Execute(&b, a.Spec); err != nil {
			slog.Error("minato: rendering docs", "err", err)
			WriteError(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(b.String()))
	})
}

func typeString(s *Schema) string {
	if s == nil {
		return ""
	}
	var t string
	switch {
	case s.Ref != "":
		t = strings.TrimPrefix(s.Ref, refPrefix)
	case s.Type == "array":
		t = "[" + typeString(s.Items) + "]"
	case s.Type == "object" && len(s.Properties) > 0:
		parts := make([]string, 0, len(s.Properties))
		for _, k := range slices.Sorted(maps.Keys(s.Properties)) {
			parts = append(parts, k+": "+typeString(s.Properties[k]))
		}
		t = "{ " + strings.Join(parts, ", ") + " }"
	case s.Type == "object" && s.AdditionalProperties != nil:
		t = "map[string]" + typeString(s.AdditionalProperties)
	case s.Type == "":
		t = "any"
	default:
		t = s.Type
		if s.Format != "" {
			t += " (" + s.Format + ")"
		}
	}
	if s.nullable {
		t += " | null"
	}
	return t
}

func constraints(s *Schema) string {
	var c []string
	num := func(name string, p *float64) {
		if p != nil {
			c = append(c, fmt.Sprintf("%s %v", name, *p))
		}
	}
	count := func(name string, p *int) {
		if p != nil {
			c = append(c, fmt.Sprintf("%s %d", name, *p))
		}
	}
	num("min", s.Minimum)
	num("max", s.Maximum)
	num("above", s.ExclusiveMinimum)
	num("below", s.ExclusiveMaximum)
	num("multiple of", s.MultipleOf)
	count("min length", s.MinLength)
	count("max length", s.MaxLength)
	count("min items", s.MinItems)
	count("max items", s.MaxItems)
	if s.UniqueItems {
		c = append(c, "unique items")
	}
	if s.Pattern != "" {
		c = append(c, "pattern "+s.Pattern)
	}
	if len(s.Enum) > 0 {
		c = append(c, fmt.Sprintf("one of %v", s.Enum))
	}
	if s.Default != nil {
		c = append(c, fmt.Sprintf("default %v", s.Default))
	}
	return strings.Join(c, "; ")
}

func isRequired(s *Schema, name string) bool { return slices.Contains(s.Required, name) }

var docsTemplate = template.Must(template.New("docs").Funcs(template.FuncMap{
	"type":        typeString,
	"constraints": constraints,
	"required":    isRequired,
	"upper":       strings.ToUpper,
}).Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Info.Title}}</title>
<style>
body{font:15px/1.5 system-ui,sans-serif;max-width:60rem;margin:2rem auto;padding:0 1rem;color:#1a1a1a}
code,.m{font-family:ui-monospace,monospace}
section{border-top:1px solid #ddd;padding:.5rem 0 1rem}
table{border-collapse:collapse;width:100%;margin:.5rem 0}
th,td{text-align:left;padding:.25rem .6rem;border-bottom:1px solid #eee;vertical-align:top}
.m{display:inline-block;min-width:4.5rem;padding:0 .4rem;border-radius:.25rem;color:#fff;background:#555;text-align:center}
.get{background:#2b7a3d}.post{background:#1f5fbf}.put{background:#b8730a}.patch{background:#8a4fb8}.delete{background:#c0392b}
</style>
</head>
<body>
<h1>{{.Info.Title}} <small>{{.Info.Version}}</small></h1>
{{with .Info.Description}}<p>{{.}}</p>{{end}}
{{range $path, $ops := .Paths}}{{range $method, $op := $ops}}
<section id="{{$op.OperationID}}">
<h2><span class="m {{$method}}">{{upper $method}}</span> <code>{{$path}}</code></h2>
{{with $op.Summary}}<p><b>{{.}}</b></p>{{end}}
{{with $op.Description}}<p>{{.}}</p>{{end}}
{{if $op.Parameters}}<table>
<tr><th>Parameter</th><th>In</th><th>Type</th><th>Required</th><th>Details</th></tr>
{{range $op.Parameters}}<tr><td><code>{{.Name}}</code></td><td>{{.In}}</td><td>{{type .Schema}}</td><td>{{if .Required}}yes{{end}}</td><td>{{constraints .Schema}} {{.Description}}</td></tr>
{{end}}</table>{{end}}
{{with $op.RequestBody}}<h3>Request body{{if .Required}} (required){{end}}</h3>
{{range $mt, $m := .Content}}<p><code>{{$mt}}</code> {{type $m.Schema}}</p>
{{end}}{{end}}
<h3>Responses</h3>
<table>
{{range $code, $r := $op.Responses}}<tr><td><code>{{$code}}</code></td><td>{{$r.Description}}</td><td>{{range $mt, $m := $r.Content}}<code>{{$mt}}</code> {{type $m.Schema}}<br>{{end}}</td></tr>
{{end}}</table>
</section>
{{end}}{{end}}
{{if .Components.Schemas}}<h2>Schemas</h2>
{{range $name, $s := .Components.Schemas}}<section id="schema-{{$name}}">
<h3>{{$name}}</h3>
<table>
<tr><th>Field</th><th>Type</th><th>Required</th><th>Details</th></tr>
{{range $k, $p := $s.Properties}}<tr><td><code>{{$k}}</code></td><td>{{type $p}}</td><td>{{if required $s $k}}yes{{end}}</td><td>{{constraints $p}} {{$p.Description}}</td></tr>
{{end}}</table>
</section>
{{end}}{{end}}
</body>
</html>
`))
