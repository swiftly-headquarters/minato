package server

import (
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"reflect"
	"slices"
)

var (
	filePtrType   = reflect.TypeFor[*multipart.FileHeader]()
	fileSliceType = reflect.TypeFor[[]*multipart.FileHeader]()
)

func (r *registry) formParams(in *input, f reflect.StructField) {
	if f.Type.Kind() != reflect.Struct {
		panic(fmt.Sprintf("minato: Form (%s) must be a struct", f.Type))
	}
	in.form = true
	in.formDoc = &Schema{Type: "object", Properties: map[string]*Schema{}}
	for _, ff := range reflect.VisibleFields(f.Type) {
		name := ff.Tag.Get("form")
		if ff.Anonymous || !ff.IsExported() || name == "" {
			continue
		}
		p := param{in: "form", name: name, index: slices.Concat(f.Index, ff.Index), schema: r.fieldSchema(ff)}
		p.required = ff.Tag.Get("required") == "true"
		p.def, p.hasDef = ff.Tag.Lookup("default")
		p.file = ff.Type == filePtrType || ff.Type == fileSliceType
		typ := p.schema.Type
		if typ == "array" && p.schema.Items != nil {
			typ = p.schema.Items.Type
		}
		binary := p.schema.Format == "binary" || (p.schema.Items != nil && p.schema.Items.Format == "binary")
		if !scalarTypes[typ] || (binary && !p.file) {
			panic(fmt.Sprintf("minato: form field %s (%s) must be a scalar, a slice of scalars, *multipart.FileHeader or a slice of those", name, ff.Type))
		}
		in.multipart = in.multipart || p.file
		in.params = append(in.params, p)
		in.formDoc.Properties[name] = p.schema
		if p.required {
			in.formDoc.Required = append(in.formDoc.Required, name)
		}
	}
}

func parseForm(w http.ResponseWriter, r *http.Request, maxBody int64) error {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "application/x-www-form-urlencoded" && ct != "multipart/form-data" {
		if r.ContentLength == 0 {
			return nil
		}
		return NewError(http.StatusUnsupportedMediaType, "expected application/x-www-form-urlencoded or multipart/form-data")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	var err error
	if ct == "multipart/form-data" {
		err = r.ParseMultipartForm(8 << 20)
	} else {
		err = r.ParseForm()
	}
	var tooBig *http.MaxBytesError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &tooBig):
		return NewError(http.StatusRequestEntityTooLarge, "request body too large")
	}
	return NewError(http.StatusBadRequest, "invalid form body: "+err.Error())
}
