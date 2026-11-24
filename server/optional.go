package server

import (
	"bytes"
	"encoding/json"
	"reflect"
)

type Optional[T any] struct {
	Value T
	Set   bool
}

func (o *Optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	return json.Unmarshal(b, &o.Value)
}

func (o Optional[T]) MarshalJSON() ([]byte, error) { return json.Marshal(o.Value) }

func (o Optional[T]) IsZero() bool { return !o.Set }

func (Optional[T]) wrapped() (reflect.Type, bool) { return reflect.TypeFor[T](), false }

type Nullable[T any] struct {
	Value T
	Set   bool
	Null  bool
}

func (n *Nullable[T]) UnmarshalJSON(b []byte) error {
	n.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		var zero T
		n.Value, n.Null = zero, true
		return nil
	}
	n.Null = false
	return json.Unmarshal(b, &n.Value)
}

func (n Nullable[T]) MarshalJSON() ([]byte, error) {
	if n.Null {
		return []byte("null"), nil
	}
	return json.Marshal(n.Value)
}

func (n Nullable[T]) IsZero() bool { return !n.Set }

func (Nullable[T]) wrapped() (reflect.Type, bool) { return reflect.TypeFor[T](), true }

type wrapper interface{ wrapped() (reflect.Type, bool) }

var wrapperType = reflect.TypeFor[wrapper]()

func unwrap(t reflect.Type) (elem reflect.Type, nullable, ok bool) {
	if !reflect.PointerTo(t).Implements(wrapperType) {
		return t, false, false
	}
	elem, nullable = reflect.New(t).Interface().(wrapper).wrapped()
	return elem, nullable, true
}
