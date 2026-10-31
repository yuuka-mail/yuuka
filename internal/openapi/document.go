package openapi

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/yuuka-mail/yuuka/internal/auth"
)

// Access is who may call an operation.
type Access int

const (
	// Authenticated: a bearer token or session cookie holding Op.Scopes.
	Authenticated Access = iota
	// Optional: anonymous, or a credential the handler looks at.
	Optional
	// Public: no credential at all.
	Public
)

// Op describes one operation. Route tables build it next to the handler
// it describes, so the description cannot name a path nobody serves.
type Op struct {
	Method, Path, Summary string
	Access                Access
	Scopes                []auth.Scope
	// Params is a struct whose path:"name" and query:"name" fields are
	// the parameters, or nil. Its field tagged body:"json", if any, is the
	// request body.
	Params reflect.Type
	// Body is the JSON request body for an Op without Params, or nil.
	Body reflect.Type
	// Response is the JSON body of the success response; nil with Media
	// empty means none.
	Response reflect.Type
	// Media is the media type of an HTTP-native success response.
	Media string
	// Status is the success status; 0 means 200, or 204 without a body.
	Status int
}

// BodyField is the field of a Params struct that holds the JSON body.
func BodyField(params reflect.Type) (reflect.StructField, bool) {
	if params == nil {
		return reflect.StructField{}, false
	}
	for _, f := range reflect.VisibleFields(params) {
		if f.Tag.Get("body") == "json" {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

// Document returns the OpenAPI 3.1 document of ops as indented JSON.
// Every named struct or Enum type reachable from a body or response
// becomes a component named after the Go type; two types with one name
// are an error. Responses never carry null (arrays are always arrays,
// absent values are omitted), so null is removed from every schema.
func Document(title, version string, ops []Op) ([]byte, error) {
	problem := reflect.TypeFor[Problem]()
	shapes := []reflect.Type{problem} // bodies and responses: their named types are components
	all := []reflect.Type{problem}    // and parameters: every enum in them is listed
	for _, op := range ops {
		body := op.Body
		if f, ok := BodyField(op.Params); ok {
			body = f.Type
		}
		for _, t := range []reflect.Type{body, op.Response} {
			if t != nil {
				shapes = append(shapes, t)
			}
		}
		if op.Params != nil {
			all = append(all, op.Params)
		}
	}
	all = append(all, shapes...)
	components := map[reflect.Type]string{}
	byName := map[string]reflect.Type{}
	var dup error
	for _, root := range shapes {
		walk(root, func(t reflect.Type) {
			if isText(t) || t.Kind() != reflect.Struct && !t.Implements(enumType) {
				return
			}
			if other, ok := byName[t.Name()]; ok && other != t {
				dup = fmt.Errorf("openapi: %s and %s are both named %s", other, t, t.Name())
			}
			byName[t.Name()], components[t] = t, t.Name()
		})
	}
	if dup != nil {
		return nil, dup
	}

	schemas := map[string]*jsonschema.Schema{}
	for t, name := range components {
		refs := map[reflect.Type]string{}
		for u, n := range components {
			if u != t && !embeds(t, u) {
				refs[u] = n
			}
		}
		s, err := jsonschema.ForType(t, Options(refs, all...))
		if err != nil {
			return nil, err
		}
		if t.Kind() == reflect.Struct {
			for _, f := range reflect.VisibleFields(t) {
				if p := s.Properties[jsonName(f)]; p != nil {
					if err := Constrain(p, f.Tag); err != nil {
						return nil, fmt.Errorf("openapi: %s.%s: %w", t, f.Name, err)
					}
				}
			}
		}
		schemas[name] = noNull(s)
	}
	refOpts := Options(components, all...)
	schemaOf := func(t reflect.Type) (*jsonschema.Schema, error) {
		if isText(t) {
			return &jsonschema.Schema{Type: "string"}, nil
		}
		s, err := jsonschema.ForType(t, refOpts)
		return noNull(s), err
	}

	paths := map[string]map[string]*operation{}
	for _, op := range ops {
		o := &operation{Summary: op.Summary, Security: security(op), Responses: map[string]*response{
			"default": {Description: "A problem", Content: content("application/problem+json", &jsonschema.Schema{Ref: "#/components/schemas/Problem"})},
		}}
		body := op.Body
		if op.Params != nil {
			for _, f := range reflect.VisibleFields(op.Params) {
				if f.Tag.Get("body") == "json" {
					body = f.Type
					continue
				}
				p := &parameter{In: "path", Name: f.Tag.Get("path"), Required: true, Description: f.Tag.Get("jsonschema")}
				if p.Name == "" {
					p.In, p.Name, p.Required = "query", f.Tag.Get("query"), false
				}
				if p.Name == "" {
					continue
				}
				var err error
				if p.Schema, err = schemaOf(f.Type); err == nil {
					err = Constrain(p.Schema, f.Tag)
				}
				if err != nil {
					return nil, fmt.Errorf("openapi: %s %s: %s: %w", op.Method, op.Path, p.Name, err)
				}
				if p.Schema.Type == "array" {
					// Several values are comma separated: class=human,unknown.
					p.Style, p.Explode = "form", new(false)
				}
				o.Parameters = append(o.Parameters, p)
			}
		}
		if body != nil {
			s, err := schemaOf(body)
			if err != nil {
				return nil, err
			}
			o.RequestBody = &requestBody{Required: true, Content: content("application/json", s)}
		}
		status := StatusOf(op)
		ok := &response{Description: http.StatusText(status)}
		if op.Response != nil {
			s, err := schemaOf(op.Response)
			if err != nil {
				return nil, err
			}
			ok.Content = content("application/json", s)
		} else if op.Media != "" {
			ok.Content = map[string]map[string]any{op.Media: {}} // any bytes
		}
		o.Responses[strconv.Itoa(status)] = ok
		if paths[op.Path] == nil {
			paths[op.Path] = map[string]*operation{}
		}
		paths[op.Path][strings.ToLower(op.Method)] = o
	}

	doc := map[string]any{
		"openapi": "3.1.0",
		"info":    map[string]string{"title": title, "version": version},
		"paths":   paths,
		"components": map[string]any{
			"schemas": schemas,
			"securitySchemes": map[string]any{
				"bearer":  map[string]string{"type": "http", "scheme": "bearer", "description": "A yuuka token (ykt_…)"},
				"session": map[string]string{"type": "apiKey", "in": "cookie", "name": "__Host-yuuka-session", "description": "The WebUI's passkey session"},
			},
		},
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	return append(out, '\n'), err
}

// StatusOf is the success status of op.
func StatusOf(op Op) int {
	if op.Status != 0 {
		return op.Status
	}
	if op.Response == nil && op.Media == "" {
		return http.StatusNoContent
	}
	return http.StatusOK
}

type operation struct {
	Summary     string                `json:"summary"`
	Security    []map[string][]string `json:"security"`
	Parameters  []*parameter          `json:"parameters,omitempty"`
	RequestBody *requestBody          `json:"requestBody,omitempty"`
	Responses   map[string]*response  `json:"responses"`
}

type parameter struct {
	Name        string             `json:"name"`
	In          string             `json:"in"`
	Description string             `json:"description,omitempty"`
	Required    bool               `json:"required,omitempty"`
	Style       string             `json:"style,omitempty"`
	Explode     *bool              `json:"explode,omitempty"`
	Schema      *jsonschema.Schema `json:"schema"`
}

type requestBody struct {
	Required bool                      `json:"required"`
	Content  map[string]map[string]any `json:"content"`
}

type response struct {
	Description string                    `json:"description"`
	Content     map[string]map[string]any `json:"content,omitempty"`
}

func content(media string, s *jsonschema.Schema) map[string]map[string]any {
	return map[string]map[string]any{media: {"schema": s}}
}

// security lists the alternatives that satisfy op; OpenAPI 3.1 allows
// scopes ("roles") for every scheme type.
func security(op Op) []map[string][]string {
	scopes := auth.Strings(op.Scopes)
	if scopes == nil {
		scopes = []string{}
	}
	switch op.Access {
	case Public:
		return []map[string][]string{}
	case Optional:
		return []map[string][]string{{}, {"bearer": scopes}, {"session": scopes}}
	}
	return []map[string][]string{{"bearer": scopes}, {"session": scopes}}
}

// embeds reports whether struct t embeds u: jsonschema-go flattens an
// embedded struct's fields and cannot refer to it.
func embeds(t, u reflect.Type) bool {
	if t.Kind() != reflect.Struct {
		return false
	}
	for _, f := range reflect.VisibleFields(t) {
		if f.Anonymous && f.Type == u {
			return true
		}
	}
	return false
}

func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return cmp.Or(name, f.Name)
}

// noNull removes null from every type in s: the API never sends it.
func noNull(s *jsonschema.Schema) *jsonschema.Schema {
	if s == nil {
		return nil
	}
	if s.Types != nil {
		var ts []string
		for _, t := range s.Types {
			if t != "null" {
				ts = append(ts, t)
			}
		}
		s.Types = nil
		if len(ts) == 1 && s.Ref == "" {
			s.Type = ts[0]
		} else if len(ts) > 1 {
			s.Types = ts
		}
	}
	noNull(s.Items)
	noNull(s.AdditionalProperties)
	for _, p := range s.Properties {
		noNull(p)
	}
	return s
}
