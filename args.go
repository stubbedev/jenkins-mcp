package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

type jsonType string

const (
	typeString  jsonType = "string"
	typeInteger jsonType = "integer"
	typeNumber  jsonType = "number"
	typeBoolean jsonType = "boolean"
	typeObject  jsonType = "object"
	typeArray   jsonType = "array"
)

var knownJSONTypes = []jsonType{typeString, typeInteger, typeNumber, typeBoolean, typeObject, typeArray}

type schema struct {
	types                []jsonType
	enum                 []string
	minimum, maximum     *int64
	minLength            *int
	pattern              *regexp.Regexp
	defaultValue         any
	hasDefault           bool
	properties           map[string]*schema
	required             []string
	closed               bool
	additionalProperties *schema
	items                *schema
}

var argAliases = map[string]string{"job": "jobPath", "repoRoot": "cwd"}

func compileSchema(path string, raw any) (*schema, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: schema must be an object", path)
	}
	s := &schema{}
	for _, key := range slices.Sorted(mapKeys(m)) {
		v := m[key]
		var err error
		switch key {
		case "description", "title":
		case "type":
			s.types, err = compileTypes(path, v)
		case "enum":
			s.enum, err = compileStringList(path+".enum", v)
		case "minimum":
			s.minimum, err = compileInt(path+".minimum", v)
		case "maximum":
			s.maximum, err = compileInt(path+".maximum", v)
		case "minLength":
			var n *int64
			if n, err = compileInt(path+".minLength", v); err == nil {
				l := int(*n)
				s.minLength = &l
			}
		case "pattern":
			p, isString := v.(string)
			if !isString {
				return nil, fmt.Errorf("%s.pattern must be a string", path)
			}
			s.pattern, err = regexp.Compile(p)
		case "default":
			s.defaultValue, s.hasDefault = v, true
		case "properties":
			props, isObject := v.(map[string]any)
			if !isObject {
				return nil, fmt.Errorf("%s.properties must be an object", path)
			}
			s.properties = make(map[string]*schema, len(props))
			for _, name := range slices.Sorted(mapKeys(props)) {
				if s.properties[name], err = compileSchema(path+"."+name, props[name]); err != nil {
					return nil, err
				}
			}
		case "required":
			s.required, err = compileStringList(path+".required", v)
		case "additionalProperties":
			switch ap := v.(type) {
			case bool:
				s.closed = !ap
			default:
				s.additionalProperties, err = compileSchema(path+".additionalProperties", ap)
			}
		case "items":
			s.items, err = compileSchema(path+".items", v)
		default:
			return nil, fmt.Errorf("%s: unsupported schema keyword %q", path, key)
		}
		if err != nil {
			return nil, err
		}
	}
	if len(s.types) == 0 {
		return nil, fmt.Errorf("%s: schema has no type", path)
	}
	if slices.Contains(s.types, typeObject) && !s.closed && s.additionalProperties == nil {
		return nil, fmt.Errorf("%s: object schema must set additionalProperties", path)
	}
	if slices.Contains(s.types, typeArray) && s.items == nil {
		return nil, fmt.Errorf("%s: array schema must set items", path)
	}
	if s.hasDefault {
		normalized, err := s.validate(path+".default", s.defaultValue)
		if err != nil {
			return nil, fmt.Errorf("invalid default: %w", err)
		}
		s.defaultValue = normalized
	}
	for _, name := range s.required {
		if _, ok := s.properties[name]; !ok {
			return nil, fmt.Errorf("%s: required property %q is not declared", path, name)
		}
	}
	return s, nil
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func compileTypes(path string, v any) ([]jsonType, error) {
	var names []string
	switch t := v.(type) {
	case string:
		names = []string{t}
	case []any:
		for _, e := range t {
			name, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("%s.type entries must be strings", path)
			}
			names = append(names, name)
		}
	default:
		return nil, fmt.Errorf("%s.type must be a string or an array of strings", path)
	}
	types := make([]jsonType, 0, len(names))
	for _, n := range names {
		if !slices.Contains(knownJSONTypes, jsonType(n)) {
			return nil, fmt.Errorf("%s: unsupported type %q", path, n)
		}
		types = append(types, jsonType(n))
	}
	return types, nil
}

func compileStringList(path string, v any) ([]string, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of strings", path)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("%s must be an array of strings", path)
		}
		out = append(out, s)
	}
	return out, nil
}

func compileInt(path string, v any) (*int64, error) {
	n, ok := v.(json.Number)
	if !ok {
		return nil, fmt.Errorf("%s must be an integer", path)
	}
	i, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s must be an integer: %w", path, err)
	}
	return &i, nil
}

func showJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprintf("%v", v)
	}
	return strings.TrimSpace(buf.String())
}

func (s *schema) typeList() string {
	names := make([]string, len(s.types))
	for i, t := range s.types {
		names[i] = string(t)
	}
	return strings.Join(names, " or ")
}

func (s *schema) validate(path string, v any) (any, error) {
	switch val := v.(type) {
	case string:
		if !slices.Contains(s.types, typeString) {
			break
		}
		if s.minLength != nil && utf8.RuneCountInString(val) < *s.minLength {
			if *s.minLength == 1 {
				return nil, fmt.Errorf("%s must not be empty", path)
			}
			return nil, fmt.Errorf("%s must be at least %d characters, got %s", path, *s.minLength, showJSON(val))
		}
		if len(s.enum) > 0 && !slices.Contains(s.enum, val) {
			return nil, fmt.Errorf("%s must be one of %s, got %s", path, showJSON(s.enum), showJSON(val))
		}
		if s.pattern != nil && !s.pattern.MatchString(val) {
			return nil, fmt.Errorf("%s must match %s, got %s", path, s.pattern, showJSON(val))
		}
		return val, nil
	case bool:
		if slices.Contains(s.types, typeBoolean) {
			return val, nil
		}
	case json.Number:
		if slices.Contains(s.types, typeInteger) {
			if i, err := strconv.ParseInt(val.String(), 10, 64); err == nil {
				if s.minimum != nil && i < *s.minimum {
					return nil, fmt.Errorf("%s must be at least %d, got %d", path, *s.minimum, i)
				}
				if s.maximum != nil && i > *s.maximum {
					return nil, fmt.Errorf("%s must be at most %d, got %d", path, *s.maximum, i)
				}
				return int(i), nil
			}
		}
		if slices.Contains(s.types, typeNumber) {
			if _, err := strconv.ParseFloat(val.String(), 64); err == nil {
				return val, nil
			}
		}
	case map[string]any:
		if slices.Contains(s.types, typeObject) {
			return s.validateObject(path, val)
		}
	case []any:
		if slices.Contains(s.types, typeArray) {
			out := make([]any, len(val))
			for i, e := range val {
				item, err := s.items.validate(fmt.Sprintf("%s[%d]", path, i), e)
				if err != nil {
					return nil, err
				}
				out[i] = item
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("%s must be %s, got %s", path, s.typeList(), showJSON(v))
}

func (s *schema) validateObject(path string, m map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(m))
	for _, key := range slices.Sorted(mapKeys(m)) {
		child := path + "." + key
		if path == "" {
			child = key
		}
		prop, declared := s.properties[key]
		switch {
		case declared:
		case s.additionalProperties != nil:
			prop = s.additionalProperties
		default:
			return nil, fmt.Errorf("unknown %s %q (known: %s)", objectNoun(path), key, strings.Join(slices.Sorted(mapKeys(s.properties)), ", "))
		}
		v, err := prop.validate(child, m[key])
		if err != nil {
			return nil, err
		}
		out[key] = v
	}
	for _, name := range s.required {
		if _, ok := out[name]; !ok {
			if path == "" {
				return nil, fmt.Errorf("%s is required", name)
			}
			return nil, fmt.Errorf("%s.%s is required", path, name)
		}
	}
	return out, nil
}

func objectNoun(path string) string {
	if path == "" {
		return "argument"
	}
	return "field in " + path
}

type toolArgs struct {
	values map[string]any
}

func decodeJSONValue(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("unexpected data after the JSON value")
	}
	return v, nil
}

func parseToolArgs(s *schema, raw json.RawMessage) (toolArgs, error) {
	m := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		v, err := decodeJSONValue(raw)
		if err != nil {
			return toolArgs{}, fmt.Errorf("invalid arguments: %w", err)
		}
		obj, ok := v.(map[string]any)
		if !ok {
			return toolArgs{}, fmt.Errorf("arguments must be a JSON object, got %s", showJSON(v))
		}
		m = obj
	}
	for alias, canonical := range argAliases {
		v, ok := m[alias]
		if !ok {
			continue
		}
		if _, both := m[canonical]; both {
			return toolArgs{}, fmt.Errorf("pass either %s or its alias %s, not both", canonical, alias)
		}
		if _, declared := s.properties[canonical]; !declared {
			continue
		}
		delete(m, alias)
		m[canonical] = v
	}
	values, err := s.validateObject("", m)
	if err != nil {
		return toolArgs{}, err
	}
	for name, prop := range s.properties {
		if _, ok := values[name]; !ok && prop.hasDefault {
			values[name] = prop.defaultValue
		}
	}
	return toolArgs{values: values}, nil
}

func argOf[T any](a toolArgs, key string) T {
	var zero T
	v, ok := a.values[key]
	if !ok {
		return zero
	}
	t, ok := v.(T)
	if !ok {
		panic(fmt.Sprintf("argument %s holds %T, not %T: the tool code disagrees with its schema", key, v, zero))
	}
	return t
}

func (a toolArgs) str(key string) string { return argOf[string](a, key) }

func (a toolArgs) integer(key string) int { return argOf[int](a, key) }

func (a toolArgs) boolean(key string) bool { return argOf[bool](a, key) }

func (a toolArgs) object(key string) map[string]any { return argOf[map[string]any](a, key) }

func (a toolArgs) has(key string) bool {
	_, ok := a.values[key]
	return ok
}

func (a toolArgs) raw(key string) any {
	return a.values[key]
}
