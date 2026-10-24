// Package runtime is the small parameter-styling and JSON-merge support the
// generated client needs. It replaces github.com/oapi-codegen/runtime for this
// client so the public client adds no third-party module to the repository's
// dependency set; the generated code reaches it through the import alias
// `runtime` set in templates/imports.tmpl. It implements only what the core
// subset's operations use: simple-style path and header parameters, form-style
// query parameters (exploded or not) of scalars, enums and slices of them,
// and RFC 7386 merging of union members.
package runtime

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// ParamLocation says where a styled parameter will be placed.
type ParamLocation int

// Parameter locations.
const (
	ParamLocationUndefined ParamLocation = iota
	ParamLocationQuery
	ParamLocationPath
	ParamLocationHeader
	ParamLocationCookie
)

// StyleParamWithLocation serializes value for the named parameter. Supported
// styles are "simple" (path, header) and "form" (query, cookie).
func StyleParamWithLocation(style string, explode bool, paramName string, location ParamLocation, value any) (string, error) {
	rv := reflect.ValueOf(value)
	for rv.IsValid() && (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface) {
		if rv.IsNil() {
			return "", errors.New("runtime: a nil parameter value")
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return "", errors.New("runtime: an invalid parameter value")
	}
	var parts []string
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return "", errors.New("runtime: byte slices are not styled parameters")
		}
		for i := 0; i < rv.Len(); i++ {
			s, err := scalar(rv.Index(i))
			if err != nil {
				return "", err
			}
			parts = append(parts, escape(s, location))
		}
	case reflect.Map, reflect.Struct:
		if _, ok := rv.Interface().(time.Time); !ok {
			return "", fmt.Errorf("runtime: object parameter %q is not supported by this client", paramName)
		}
		fallthrough
	default:
		s, err := scalar(rv)
		if err != nil {
			return "", err
		}
		parts = []string{escape(s, location)}
	}
	switch style {
	case "simple":
		return strings.Join(parts, ","), nil
	case "form":
		if explode {
			for i, p := range parts {
				parts[i] = url.QueryEscape(paramName) + "=" + p
			}
			return strings.Join(parts, "&"), nil
		}
		return url.QueryEscape(paramName) + "=" + strings.Join(parts, ","), nil
	}
	return "", fmt.Errorf("runtime: parameter style %q is not supported", style)
}

func scalar(v reflect.Value) (string, error) {
	if v.CanInterface() {
		switch t := v.Interface().(type) {
		case time.Time:
			return t.UTC().Format(time.RFC3339Nano), nil
		case encoding.TextMarshaler:
			b, err := t.MarshalText()
			return string(b), err
		}
	}
	switch v.Kind() {
	case reflect.String:
		return v.String(), nil
	case reflect.Bool:
		return strconv.FormatBool(v.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10), nil
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'f', -1, 64), nil
	}
	return "", fmt.Errorf("runtime: unsupported parameter type %s", v.Type())
}

func escape(s string, location ParamLocation) string {
	switch location {
	case ParamLocationQuery:
		return url.QueryEscape(s)
	case ParamLocationPath:
		return url.PathEscape(s)
	}
	return s
}

// JSONMerge applies patch to data as a JSON merge patch (RFC 7386).
func JSONMerge(data, patch []byte) ([]byte, error) {
	var base, p any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &base); err != nil {
			return nil, err
		}
	}
	if err := json.Unmarshal(patch, &p); err != nil {
		return nil, err
	}
	return json.Marshal(merge(base, p))
}

func merge(base, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	bm, ok := base.(map[string]any)
	if !ok {
		bm = map[string]any{}
	}
	for k, v := range pm {
		if v == nil {
			delete(bm, k)
			continue
		}
		bm[k] = merge(bm[k], v)
	}
	return bm
}
