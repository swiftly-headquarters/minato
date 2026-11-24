package server

import (
	"context"
	"errors"
	"reflect"
)

type Validator interface {
	Validate(ctx context.Context) []error
}

func FieldError(location, message string) error {
	return &ErrorDetail{Location: location, Message: message}
}

func (e *ErrorDetail) Error() string { return e.Message }

var validatorType = reflect.TypeFor[Validator]()

func implementsValidator(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return reflect.PointerTo(t).Implements(validatorType)
}

func validatorOf(v reflect.Value) Validator {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	val, _ := v.Addr().Interface().(Validator)
	return val
}

func (in *input) runValidators(ctx context.Context, v reflect.Value, errs []*ErrorDetail) []*ErrorDetail {
	var vals []Validator
	if in.hook {
		vals = append(vals, validatorOf(v))
	}
	if in.bodyHook {
		vals = append(vals, validatorOf(v.FieldByIndex(in.body.index)))
	}
	for _, val := range vals {
		if val == nil {
			continue
		}
		for _, err := range val.Validate(ctx) {
			var d *ErrorDetail
			if !errors.As(err, &d) {
				d = &ErrorDetail{Message: err.Error()}
			}
			errs = append(errs, d)
		}
	}
	return errs
}
