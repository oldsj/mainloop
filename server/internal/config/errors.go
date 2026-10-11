package config

import (
	"errors"
	"reflect"
	"strings"

	"github.com/caarlos0/env/v11"
)

// nameParseErrors rewrites env's parse errors, which name Go fields, so they name the
// environment variable instead. Messages from the custom types never quote the value.
func nameParseErrors(err error, vars map[string]string) error {
	var agg env.AggregateError
	if !errors.As(err, &agg) {
		return err
	}
	fields := envFields(reflect.TypeOf(Config{}), nil)
	out := make([]error, 0, len(agg.Errors))
	for _, e := range agg.Errors {
		var pe env.ParseError
		if !errors.As(e, &pe) {
			out = append(out, e)
			continue
		}
		out = append(out, namedError{vars: candidates(fields, pe, vars), cause: pe.Err})
	}
	return errors.Join(out...)
}

type namedError struct {
	vars  []string
	cause error
}

func (e namedError) Error() string {
	return strings.Join(e.vars, "/") + ": " + e.cause.Error()
}

func (e namedError) Unwrap() error { return e.cause }

type envField struct{ field, typ, name string }

func envFields(t reflect.Type, out []envField) []envField {
	for i := range t.NumField() {
		sf := t.Field(i)
		if sf.Type.Kind() == reflect.Struct && sf.Tag.Get("env") == "" {
			out = envFields(sf.Type, out)
			continue
		}
		if tag := sf.Tag.Get("env"); tag != "" {
			name, _, _ := strings.Cut(tag, ",")
			out = append(out, envField{sf.Name, sf.Type.String(), name})
		}
	}
	return out
}

// candidates finds the variables behind a failed field. Field names are not unique (Port), so
// prefer the variables that were actually set.
func candidates(fields []envField, pe env.ParseError, vars map[string]string) []string {
	var all, set []string
	for _, f := range fields {
		if f.field != pe.Name || f.typ != pe.Type.String() {
			continue
		}
		all = append(all, f.name)
		if _, ok := vars[f.name]; ok {
			set = append(set, f.name)
		}
	}
	if len(set) > 0 {
		return set
	}
	if len(all) > 0 {
		return all
	}
	return []string{pe.Name}
}
