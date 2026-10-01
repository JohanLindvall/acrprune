package rules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/JohanLindvall/crprune/internal/jsonpos"
)

// jsonSpace is the whitespace JSON allows between tokens.
const jsonSpace = " \t\r\n"

var unmarshalerType = reflect.TypeFor[json.Unmarshaler]()

// document is a syntactically valid rule file together with what each of its
// values is called, so that errors can point at the value at fault and name
// it.
type document struct {
	data []byte
	// names is keyed by the offset each value starts at.
	names map[int64]string
}

// scanDocument checks the rule file's syntax, then walks it against the shape
// of []*RepoRuleSpec to report, with their position, the mistakes
// encoding/json would accept silently or report without one: duplicate keys
// (the last would win, so `"keep": true, …, "keep": false` would silently
// become a delete rule), unknown fields and invalid durations. Type mismatches
// are left to the decoder, whose errors document.decodeError then places.
func scanDocument(data []byte) (*document, error) {
	doc := &document{data: data, names: map[int64]string{}}
	var raw json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		if offset, ok := jsonpos.Offset(data, 0, err); ok {
			return nil, jsonpos.At(data, offset, err)
		}
		return nil, err
	}
	w := &walker{doc: doc, dec: json.NewDecoder(bytes.NewReader(data))}
	w.dec.UseNumber()
	if err := w.value(reflect.TypeFor[[]*RepoRuleSpec](), "the rule file", ""); err != nil {
		return nil, err
	}
	return doc, nil
}

// decodeError places an error from decoding the document's value, which
// starts at base, and names the value at fault.
func (d *document) decodeError(err error, base int64) error {
	typeErr, ok := errors.AsType[*json.UnmarshalTypeError](err)
	if !ok {
		return err
	}
	start, _ := jsonpos.Offset(d.data, base, err)
	name, ok := d.names[start]
	if !ok {
		name = "value"
	}
	return d.errorAt(start, fmt.Errorf("%s must be %s, not %s", name, describeType(typeErr.Type), describeValue(typeErr.Value)))
}

// errorAt prefixes err with the line and column of offset.
func (d *document) errorAt(offset int64, err error) error {
	return jsonpos.At(d.data, offset, err)
}

// walker visits the tokens of a syntactically valid document.
type walker struct {
	doc *document
	dec *json.Decoder
}

// next reads the next token and the offset it starts at.
func (w *walker) next() (json.Token, int64, error) {
	start := jsonpos.TokenStart(w.doc.data, w.dec.InputOffset())
	tok, err := w.dec.Token()
	if err != nil {
		return nil, start, w.doc.errorAt(start, err)
	}
	return tok, start, nil
}

// value walks one value that decodes into t, called name in errors. key is the
// object key holding the value, if any. t is nil for values that have no place
// in the rule format; the decoder reports those.
func (w *walker) value(t reflect.Type, name, key string) error {
	tok, start, err := w.next()
	if err != nil {
		return err
	}
	w.doc.names[start] = name
	if tok == nil {
		return nil // null leaves the field unset
	}
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t != nil && reflect.PointerTo(t).Implements(unmarshalerType) {
		// Decode now, while the value's position is known.
		if err := w.skip(tok); err != nil {
			return err
		}
		raw := w.doc.data[start:w.dec.InputOffset()]
		if err := reflect.New(t).Interface().(json.Unmarshaler).UnmarshalJSON(raw); err != nil {
			return w.doc.errorAt(start, fmt.Errorf("%s: %w", name, err))
		}
		return nil
	}
	switch tok {
	case json.Delim('{'):
		return w.object(t)
	case json.Delim('['):
		return w.array(t, key)
	}
	return nil
}

// object walks the members of an object that decodes into t.
func (w *walker) object(t reflect.Type) error {
	type member struct {
		key   string
		start int64
	}
	var seen []member
	for w.dec.More() {
		tok, start, err := w.next()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		// encoding/json matches field names case-insensitively, so "Keep"
		// overrides "keep" as much as a second "keep" does.
		for _, m := range seen {
			if !strings.EqualFold(m.key, key) {
				continue
			}
			line, column := jsonpos.Position(w.doc.data, m.start)
			if m.key == key {
				return w.doc.errorAt(start, fmt.Errorf("duplicate key %q (first set at line %d, column %d)", key, line, column))
			}
			return w.doc.errorAt(start, fmt.Errorf("duplicate key %q (keys ignore case, and %q is set at line %d, column %d)", key, m.key, line, column))
		}
		seen = append(seen, member{key, start})

		var field reflect.Type
		if t != nil && t.Kind() == reflect.Struct {
			var ok bool
			if field, ok = fieldType(t, key); !ok {
				return w.doc.errorAt(start, fmt.Errorf("unknown field %q", key))
			}
		}
		if err := w.value(field, strconv.Quote(key), key); err != nil {
			return err
		}
	}
	_, _, err := w.next() // }
	return err
}

// array walks the elements of an array that decodes into t, naming them
// after the key holding the array: "rule 1" at the top level, "tagged rule 1"
// in a "tagged" list.
func (w *walker) array(t reflect.Type, key string) error {
	var elem reflect.Type
	if t != nil && t.Kind() == reflect.Slice {
		elem = t.Elem()
	}
	prefix := "rule"
	if key != "" {
		prefix = key + " rule"
	}
	for i := 0; w.dec.More(); i++ {
		if err := w.value(elem, fmt.Sprintf("%s %d", prefix, i+1), ""); err != nil {
			return err
		}
	}
	_, _, err := w.next() // ]
	return err
}

// skip reads past the rest of the value that tok starts.
func (w *walker) skip(tok json.Token) error {
	for depth := 0; ; {
		switch tok {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
		if depth == 0 {
			return nil
		}
		var err error
		if tok, _, err = w.next(); err != nil {
			return err
		}
	}
}

// fieldType returns the type of the struct field that encoding/json decodes
// key into: fields are named by their json tag, embedded structs are
// flattened, and names match case-insensitively.
func fieldType(t reflect.Type, key string) (reflect.Type, bool) {
	for _, f := range reflect.VisibleFields(t) {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if !f.IsExported() || name == "-" || f.Anonymous && name == "" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		if strings.EqualFold(name, key) {
			return f.Type, true
		}
	}
	return nil, false
}

// describeType says in rule file terms what a value decoding into t must be.
func describeType(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Bool:
		return "true or false"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "an integer"
	case reflect.String:
		return "a string"
	case reflect.Slice, reflect.Array:
		return "an array"
	case reflect.Struct:
		return "an object"
	default:
		return t.Kind().String()
	}
}

// describeValue rephrases the kind of value encoding/json's
// UnmarshalTypeError reports ("string", "number 1.5", …).
func describeValue(value string) string {
	switch value {
	case "string":
		return "a string"
	case "number":
		return "a number"
	case "bool":
		return "a boolean"
	case "array":
		return "an array"
	case "object":
		return "an object"
	}
	if number, ok := strings.CutPrefix(value, "number "); ok {
		return number
	}
	return value
}
