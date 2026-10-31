package api

import (
	"context"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/openapi"
)

// route is one operation: its description, which openapi.json and the
// ACL test read, and the handler that serves it.
type route struct {
	openapi.Op
	// serve runs after authentication. A JSON route returns the body to
	// send with Op.Status; an HTTP-native one writes its response itself
	// and returns nil.
	serve func(a *API, w http.ResponseWriter, r *http.Request, v *auth.Viewer) (any, error)
	// needsSubmitter: served only by an API with a submission core.
	needsSubmitter bool
}

// jsonRoute is an operation answered with JSON: P is bound from the
// request (bind), R is the response body, struct{} for none.
func jsonRoute[P, R any](method, path, summary string, scopes []auth.Scope,
	h func(*API, context.Context, *auth.Viewer, P) (R, error),
) route {
	op := describe[P](method, path, summary, scopes)
	if op.Response = reflect.TypeFor[R](); op.Response == reflect.TypeFor[struct{}]() {
		op.Response = nil // 204 No Content
	}
	return route{Op: op, serve: func(a *API, _ http.ResponseWriter, r *http.Request, v *auth.Viewer) (any, error) {
		var p P
		if err := bind(r, &p); err != nil {
			return nil, err
		}
		return h(a, r.Context(), v, p)
	}}
}

// rawRoute is an HTTP-native operation (a download, a stream): P is bound
// as for jsonRoute, and h writes a success response of media type media.
func rawRoute[P any](method, path, summary, media string, scopes []auth.Scope,
	h func(*API, http.ResponseWriter, *http.Request, *auth.Viewer, P) error,
) route {
	op := describe[P](method, path, summary, scopes)
	op.Media = media
	return route{Op: op, serve: func(a *API, w http.ResponseWriter, r *http.Request, v *auth.Viewer) (any, error) {
		var p P
		if err := bind(r, &p); err != nil {
			return nil, err
		}
		return nil, h(a, w, r, v, p)
	}}
}

func describe[P any](method, path, summary string, scopes []auth.Scope) openapi.Op {
	return openapi.Op{Method: method, Path: path, Summary: summary, Scopes: scopes, Params: reflect.TypeFor[P]()}
}

// withStatus sets the success status, e.g. 201 for a creation.
func (rt route) withStatus(code int) route {
	rt.Status = code
	return rt
}

// authority marks a route that reads or changes who may do what
// (§13.10): on top of its scopes it is served only to a person's
// session, checked before the handler runs. A token holding every scope
// is refused, so an agent cannot use one to change its own authority.
func (rt route) authority() route {
	rt.Access = openapi.HumanSession
	return rt
}

// submitting marks a route that hands mail to the submission core.
func (rt route) submitting() route {
	rt.needsSubmitter = true
	return rt
}

// bind fills *p from r: path:"name" and query:"name" fields from the URL,
// the field tagged body:"json" from the JSON body. An absent or empty
// parameter takes its default tag, if any. Several values of a slice
// parameter are comma separated. Every value is checked against its
// type's Enum and its field's constraint tags (openapi.Check). A query
// parameter the operation does not have is refused: ignoring it would
// let a caller believe it filtered by it, as by search's label= before
// §16 moved it into q.
func bind(r *http.Request, p any) error {
	v := reflect.ValueOf(p).Elem()
	query := r.URL.Query()
	for name := range query {
		if !slices.ContainsFunc(reflect.VisibleFields(v.Type()), func(f reflect.StructField) bool { return f.Tag.Get("query") == name }) {
			return openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidParam, "%s: no such parameter", name)
		}
	}
	for _, f := range reflect.VisibleFields(v.Type()) {
		var name, s string
		switch {
		case f.Tag.Get("body") == "json":
			if err := readBody(r, v.FieldByIndex(f.Index).Addr().Interface()); err != nil {
				return err
			}
			continue
		case f.Tag.Get("path") != "":
			name = f.Tag.Get("path")
			s = r.PathValue(name)
		case f.Tag.Get("query") != "":
			name = f.Tag.Get("query")
			s = query.Get(name)
		default:
			continue
		}
		if s == "" {
			if s = f.Tag.Get("default"); s == "" {
				continue
			}
		}
		if err := setParam(v.FieldByIndex(f.Index), s, f.Tag); err != nil {
			return openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidParam, "%s: %v", name, err)
		}
	}
	return nil
}

func setParam(v reflect.Value, s string, tag reflect.StructTag) error {
	if v.Kind() == reflect.Pointer {
		v.Set(reflect.New(v.Type().Elem()))
		v = v.Elem()
	}
	if u, ok := v.Addr().Interface().(encoding.TextUnmarshaler); ok {
		return u.UnmarshalText([]byte(s))
	}
	switch v.Kind() {
	case reflect.Slice:
		parts := strings.Split(s, ",")
		v.Set(reflect.MakeSlice(v.Type(), len(parts), len(parts)))
		for i, part := range parts {
			if err := setParam(v.Index(i), part, tag); err != nil {
				return err
			}
		}
		return nil
	case reflect.String:
		v.SetString(s)
	case reflect.Int:
		n, err := strconv.Atoi(s)
		if err != nil {
			return errors.New("want an integer")
		}
		v.SetInt(int64(n))
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return errors.New("want true or false")
		}
		v.SetBool(b)
	default:
		return fmt.Errorf("cannot bind a %s", v.Type())
	}
	return openapi.Check(v, tag)
}

// maxBody bounds a JSON request body.
const maxBody = 1 << 20

// readBody decodes an application/json body into dst, refusing unknown
// fields, and checks every value in it as bind checks parameters. An
// omitempty field left at its zero value is absent and not checked.
func readBody(r *http.Request, dst any) error {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		return openapi.Errorf(http.StatusUnsupportedMediaType, openapi.CodeUnsupportedType, "want application/json")
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return openapi.Errorf(http.StatusRequestEntityTooLarge, openapi.CodeBodyTooLarge, "at most %d bytes", maxBody)
		}
		return openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidBody, "%v", err)
	}
	if err := checkBody(reflect.ValueOf(dst).Elem(), "", "body"); err != nil {
		return openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidBody, "%v", err)
	}
	return nil
}

func checkBody(v reflect.Value, tag reflect.StructTag, path string) error {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return checkBody(v.Elem(), tag, path)
	case reflect.Slice:
		for i := range v.Len() {
			if err := checkBody(v.Index(i), tag, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	case reflect.Struct:
		if _, ok := v.Addr().Interface().(encoding.TextUnmarshaler); ok {
			return nil // time.Time and the like: decoding checked them
		}
		for _, f := range reflect.VisibleFields(v.Type()) {
			name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
			fv := v.FieldByIndex(f.Index)
			if !f.IsExported() || f.Anonymous || name == "-" || fv.IsZero() && strings.Contains(opts, "omit") {
				continue
			}
			if err := checkBody(fv, f.Tag, path+"."+name); err != nil {
				return err
			}
		}
		return nil
	}
	if err := openapi.Check(v, tag); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
