package privileged

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// exactRow checks that a stored action's arguments and frozen facts are exactly what its type's
// code reads. Go's decoder matches field names in any letter case, takes the last of duplicate
// keys and drops unknown ones, so a row that differs from its type's own encoding could show the
// owner one thing (the phone shows the record's keys as stored) and run another. Each part must
// decode strictly into the type's struct (no unknown field, nothing after the value) and encode
// back canonically to the same JSON. The arguments also pass the type's validation again: their
// struct is the one the type's normalize returns.
func exactRow(row dbq.PrivilegedAction) error {
	e, ok := catalog[row.Type]
	if !ok {
		return fmt.Errorf("unknown type %q", row.Type)
	}
	args, err := e.normalize(row.Args)
	if err != nil {
		return fmt.Errorf("the arguments no longer validate: %w", err)
	}
	if err := roundTrips("arguments", row.Args, reflect.TypeOf(args)); err != nil {
		return err
	}
	if e.facts == nil {
		return fmt.Errorf("type %s declares no facts type", row.Type)
	}
	return roundTrips("frozen facts", row.FrozenFacts, reflect.TypeOf(e.facts))
}

func roundTrips(what string, raw []byte, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	v := reflect.New(t).Interface()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber() // numbers inside free-form values keep their text
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("the %s don't decode as their type: %w", what, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("the %s have data after their value", what)
	}
	got, err := canonical(v)
	if err != nil {
		return err
	}
	want, err := canonicalRaw(raw)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("the %s don't round-trip through their type: a key in another letter case, a duplicate key, or a value the type would change", what)
	}
	return nil
}
