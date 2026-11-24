package server

import (
	"cmp"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

type OpenAPI struct {
	OpenAPI    string                              `json:"openapi"`
	Info       Info                                `json:"info"`
	Servers    []Server                            `json:"servers,omitempty"`
	Security   []map[string][]string               `json:"security,omitempty"`
	Paths      map[string]map[string]*OperationDoc `json:"paths"`
	Components Components                          `json:"components"`
}

type Info struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

type Server struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

type Components struct {
	Schemas         map[string]*Schema `json:"schemas,omitempty"`
	SecuritySchemes map[string]any     `json:"securitySchemes,omitempty"`
}

type OperationDoc struct {
	OperationID string                  `json:"operationId,omitempty"`
	Summary     string                  `json:"summary,omitempty"`
	Description string                  `json:"description,omitempty"`
	Tags        []string                `json:"tags,omitempty"`
	Deprecated  bool                    `json:"deprecated,omitempty"`
	Security    []map[string][]string   `json:"security,omitempty"`
	Parameters  []*ParamDoc             `json:"parameters,omitempty"`
	RequestBody *RequestBodyDoc         `json:"requestBody,omitempty"`
	Responses   map[string]*ResponseDoc `json:"responses"`
}

type ParamDoc struct {
	Name        string  `json:"name"`
	In          string  `json:"in"`
	Required    bool    `json:"required,omitempty"`
	Description string  `json:"description,omitempty"`
	Schema      *Schema `json:"schema"`
}

type RequestBodyDoc struct {
	Required bool                  `json:"required,omitempty"`
	Content  map[string]*MediaType `json:"content"`
}

type ResponseDoc struct {
	Description string                `json:"description"`
	Headers     map[string]*HeaderDoc `json:"headers,omitempty"`
	Content     map[string]*MediaType `json:"content,omitempty"`
}

type HeaderDoc struct {
	Schema *Schema `json:"schema"`
}

type MediaType struct {
	Schema *Schema `json:"schema"`
}

var errorModelType = reflect.TypeFor[ErrorModel]()

func (a *API) document(op Operation, in *input, out *output) {
	d := &OperationDoc{
		OperationID: op.OperationID,
		Summary:     op.Summary,
		Description: op.Description,
		Tags:        op.Tags,
		Deprecated:  op.Deprecated,
		Security:    op.Security,
		Responses:   map[string]*ResponseDoc{},
	}
	for _, p := range in.params {
		if p.in != "form" {
			d.Parameters = append(d.Parameters, &ParamDoc{Name: p.name, In: p.in, Required: p.required, Description: p.schema.Description, Schema: p.schema})
		}
	}
	if in.body != nil {
		d.RequestBody = &RequestBodyDoc{Required: in.body.required, Content: content(in.body, slices.Sorted(maps.Keys(a.Decoders)))}
	}
	if in.form {
		form := &MediaType{Schema: in.formDoc}
		d.RequestBody = &RequestBodyDoc{Required: len(in.formDoc.Required) > 0, Content: map[string]*MediaType{"multipart/form-data": form}}
		if !in.multipart {
			d.RequestBody.Content["application/x-www-form-urlencoded"] = form
		}
	}
	status := out.defaultStatus(op.DefaultStatus)
	resp := &ResponseDoc{Description: http.StatusText(status)}
	for _, h := range out.headers {
		if resp.Headers == nil {
			resp.Headers = map[string]*HeaderDoc{}
		}
		resp.Headers[h.name] = &HeaderDoc{Schema: h.schema}
	}
	if out.body != nil {
		resp.Content = content(out.body, slices.Sorted(maps.Keys(a.Formats)))
	}
	d.Responses[strconv.Itoa(status)] = resp
	problem := map[string]*MediaType{"application/problem+json": {Schema: a.reg.schema(errorModelType)}}
	d.Responses["default"] = &ResponseDoc{Description: "Error", Content: problem}
	if len(in.params) > 0 || in.body != nil || in.hook || in.bodyHook {
		if _, taken := d.Responses["422"]; !taken {
			d.Responses["422"] = &ResponseDoc{Description: http.StatusText(http.StatusUnprocessableEntity), Content: problem}
		}
	}
	if len(op.Security) > 0 {
		if _, taken := d.Responses["401"]; !taken {
			d.Responses["401"] = &ResponseDoc{Description: http.StatusText(http.StatusUnauthorized), Content: problem}
		}
	}
	for _, code := range op.Errors {
		d.Responses[strconv.Itoa(code)] = &ResponseDoc{Description: http.StatusText(code), Content: problem}
	}
	for _, rs := range op.Responses {
		key := strconv.Itoa(rs.Status)
		doc := &ResponseDoc{Description: cmp.Or(rs.Description, http.StatusText(rs.Status))}
		if old := d.Responses[key]; old != nil {
			doc.Headers = old.Headers
		}
		if rs.Body != nil {
			doc.Content = content(a.reg.typeField(reflect.TypeOf(rs.Body)), slices.Sorted(maps.Keys(a.Formats)))
		}
		d.Responses[key] = doc
	}

	path := strings.ReplaceAll(op.Path, "...}", "}")
	if a.Spec.Paths[path] == nil {
		a.Spec.Paths[path] = map[string]*OperationDoc{}
	}
	a.Spec.Paths[path][strings.ToLower(op.Method)] = d
}

func content(f *field, formats []string) map[string]*MediaType {
	switch {
	case f.raw || f.stream:
		return map[string]*MediaType{"application/octet-stream": {Schema: &Schema{Type: "string", Format: "binary"}}}
	case f.sse:
		return map[string]*MediaType{"text/event-stream": {Schema: &Schema{Type: "string"}}}
	}
	m := map[string]*MediaType{}
	for _, mt := range formats {
		m[mt] = &MediaType{Schema: f.schema}
	}
	return m
}

const docsHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>API Reference</title>
<script src="https://unpkg.com/@stoplight/elements/web-components.min.js"></script>
<link rel="stylesheet" href="https://unpkg.com/@stoplight/elements/styles.min.css">
</head>
<body style="height:100vh;margin:0">
<elements-api apiDescriptionUrl="openapi.json" router="hash" layout="sidebar"></elements-api>
</body>
</html>
`
