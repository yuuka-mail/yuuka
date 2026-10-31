package openapi

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// Enum is implemented by string types with a closed set of values. The
// values are declared once, on the type: the schema lists them and
// binding refuses anything else.
type Enum interface{ Enum() []string }

var enumType = reflect.TypeFor[Enum]()

// Schemer is a type that says its own schema, one reflection cannot
// derive: a value that takes more than one JSON shape.
type Schemer interface{ JSONSchema() *jsonschema.Schema }

var schemerType = reflect.TypeFor[Schemer]()

// Options are the ForOptions every schema of yuuka's contracts is derived
// with, REST and MCP alike: each Enum type reachable from roots becomes a
// string schema listing its values, each Schemer its own schema,
// time.Time a date-time string, and each type in refs (named by its key)
// a $ref instead.
func Options(refs map[reflect.Type]string, roots ...reflect.Type) *jsonschema.ForOptions {
	m := map[reflect.Type]*jsonschema.Schema{
		reflect.TypeFor[time.Time](): {Type: "string", Format: "date-time"},
	}
	for _, root := range roots {
		walk(root, func(t reflect.Type) {
			if t.Implements(enumType) && t.Kind() == reflect.String {
				vals := reflect.Zero(t).Interface().(Enum).Enum()
				m[t] = &jsonschema.Schema{Type: "string", Enum: anys(vals)}
			}
			if t.Implements(schemerType) {
				m[t] = reflect.Zero(t).Interface().(Schemer).JSONSchema()
			}
		})
	}
	for t, name := range refs {
		m[t] = &jsonschema.Schema{Ref: "#/components/schemas/" + name}
	}
	return &jsonschema.ForOptions{TypeSchemas: m}
}

// Schema is the schema of T as the MCP tools declare their inputs.
func Schema[T any]() (*jsonschema.Schema, error) {
	t := reflect.TypeFor[T]()
	return jsonschema.ForType(t, Options(nil, t))
}

// walk calls fn for every named type reachable from t, each once.
func walk(t reflect.Type, fn func(reflect.Type)) {
	seen := map[reflect.Type]bool{}
	var visit func(reflect.Type)
	visit = func(t reflect.Type) {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
			t = t.Elem()
		}
		if seen[t] {
			return
		}
		seen[t] = true
		if t.Name() != "" {
			fn(t)
		}
		if t.Kind() == reflect.Struct && !isText(t) {
			for _, f := range reflect.VisibleFields(t) {
				if f.IsExported() {
					visit(f.Type)
				}
			}
		}
	}
	visit(t)
}

var textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()

// isText reports whether t is parsed from a string by UnmarshalText; its
// schema is a plain string whatever its Go shape.
func isText(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return reflect.PointerTo(t).Implements(textUnmarshaler)
}

func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// The constraint tags. A field's tags are read in two places only: here,
// into its schema (Constrain), and at runtime by Check, so the document
// and the server cannot drift apart.
//
//	minimum:"1" maximum:"500"   integers
//	pattern:"^[a-z]+$"          strings, an RE2 regexp that is also valid ECMA-262
//	default:"100"               applied when the parameter is absent
func Constrain(s *jsonschema.Schema, tag reflect.StructTag) error {
	for k, dst := range map[string]**float64{"minimum": &s.Minimum, "maximum": &s.Maximum} {
		if v, ok := tag.Lookup(k); ok {
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("%s:%q: %w", k, v, err)
			}
			*dst = &n
		}
	}
	if p, ok := tag.Lookup("pattern"); ok {
		if _, err := compile(p); err != nil {
			return err
		}
		s.Pattern = p
	}
	if d, ok := tag.Lookup("default"); ok {
		raw := json.RawMessage(d)
		if s.Type == "string" || s.Ref != "" {
			raw, _ = json.Marshal(d)
		}
		s.Default = raw
	}
	return nil
}

// Check validates one bound scalar value against its type's Enum and the
// field's constraint tags. The error says what is wanted.
func Check(v reflect.Value, tag reflect.StructTag) error {
	if e, ok := v.Interface().(Enum); ok && v.Kind() == reflect.String && !slices.Contains(e.Enum(), v.String()) {
		return fmt.Errorf("want one of %s", strings.Join(e.Enum(), ", "))
	}
	if v.CanInt() {
		if m, ok := tag.Lookup("minimum"); ok && float64(v.Int()) < atof(m) {
			return fmt.Errorf("want at least %s", m)
		}
		if m, ok := tag.Lookup("maximum"); ok && float64(v.Int()) > atof(m) {
			return fmt.Errorf("want at most %s", m)
		}
	}
	if p, ok := tag.Lookup("pattern"); ok && v.Kind() == reflect.String {
		re, err := compile(p)
		if err != nil {
			return err
		}
		if !re.MatchString(v.String()) {
			return fmt.Errorf("want a match of %s", p)
		}
	}
	return nil
}

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

var patterns sync.Map // string → *regexp.Regexp

func compile(p string) (*regexp.Regexp, error) {
	if re, ok := patterns.Load(p); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, fmt.Errorf("pattern %q: %w", p, err)
	}
	patterns.Store(p, re)
	return re, nil
}
