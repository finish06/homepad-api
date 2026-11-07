// Package schemacheck validates JSON documents against the JSON Schema subset
// the backup schema uses, so tests need no third-party schema library.
package schemacheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Validate checks doc against schema and returns every violation
// (nil when doc conforms). It implements the subset of JSON Schema 2020-12
// that docs/schema/homepad-backup-v1.json uses: type, const, enum, required,
// properties, additionalProperties: false, items, pattern, minLength,
// maxLength, minimum, maximum, maxItems and local $ref to #/$defs. Anything
// else in the schema is reported as unsupported, so the checker can't pass a
// document by quietly skipping a rule it doesn't know.
func Validate(schema, doc []byte) []string {
	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil {
		return []string{"schema: " + err.Error()}
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return []string{"document: " + err.Error()}
	}
	c := &schemaChecker{root: root}
	c.check(root, v, "$")
	return c.errs
}

type schemaChecker struct {
	root map[string]any
	errs []string
}

var supportedKeywords = map[string]bool{
	"$schema": true, "$id": true, "title": true, "description": true, "$defs": true,
	"type": true, "const": true, "enum": true, "required": true, "properties": true,
	"additionalProperties": true, "items": true, "pattern": true, "minLength": true,
	"maxLength": true, "minimum": true, "maximum": true, "maxItems": true, "$ref": true,
}

func (c *schemaChecker) fail(path, format string, args ...any) {
	c.errs = append(c.errs, path+": "+fmt.Sprintf(format, args...))
}

func (c *schemaChecker) check(s map[string]any, v any, path string) {
	for k := range s {
		if !supportedKeywords[k] {
			c.fail(path, "schema keyword %q is not supported by this checker", k)
		}
	}
	if ref, ok := s["$ref"].(string); ok {
		name, found := strings.CutPrefix(ref, "#/$defs/")
		defs, _ := c.root["$defs"].(map[string]any)
		target, _ := defs[name].(map[string]any)
		if !found || target == nil {
			c.fail(path, "unresolvable $ref %q", ref)
			return
		}
		c.check(target, v, path)
	}
	if t, ok := s["type"]; ok && !matchesType(t, v) {
		c.fail(path, "type %v wanted, got %s", t, jsonType(v))
		return
	}
	if want, ok := s["const"]; ok && !jsonEqual(want, v) {
		c.fail(path, "const %v wanted, got %v", want, v)
	}
	if enum, ok := s["enum"].([]any); ok {
		hit := false
		for _, e := range enum {
			if jsonEqual(e, v) {
				hit = true
			}
		}
		if !hit {
			c.fail(path, "value %v is not one of %v", v, enum)
		}
	}
	switch x := v.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if _, present := x[r.(string)]; !present {
					c.fail(path, "missing required key %q", r)
				}
			}
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if ps, ok := props[k].(map[string]any); ok {
				c.check(ps, x[k], path+"."+k)
			} else if ap, ok := s["additionalProperties"].(bool); ok && !ap {
				c.fail(path, "key %q is not allowed", k)
			}
		}
	case []any:
		if max, ok := s["maxItems"].(float64); ok && float64(len(x)) > max {
			c.fail(path, "%d items, at most %v allowed", len(x), max)
		}
		if is, ok := s["items"].(map[string]any); ok {
			for i, e := range x {
				c.check(is, e, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	case string:
		if p, ok := s["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(x) {
			c.fail(path, "%q does not match %s", x, p)
		}
		n := float64(utf8.RuneCountInString(x))
		if m, ok := s["minLength"].(float64); ok && n < m {
			c.fail(path, "shorter than %v", m)
		}
		if m, ok := s["maxLength"].(float64); ok && n > m {
			c.fail(path, "longer than %v", m)
		}
	case json.Number:
		f, _ := x.Float64()
		if m, ok := s["minimum"].(float64); ok && f < m {
			c.fail(path, "%v is below %v", f, m)
		}
		if m, ok := s["maximum"].(float64); ok && f > m {
			c.fail(path, "%v is above %v", f, m)
		}
	}
}

func jsonType(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case json.Number:
		if f, err := x.Float64(); err == nil && f == math.Trunc(f) && !strings.ContainsAny(x.String(), ".eE") {
			return "integer"
		}
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

func matchesType(t, v any) bool {
	got := jsonType(v)
	ok := func(name string) bool {
		return name == got || (name == "number" && got == "integer")
	}
	switch tt := t.(type) {
	case string:
		return ok(tt)
	case []any:
		for _, n := range tt {
			if s, _ := n.(string); ok(s) {
				return true
			}
		}
	}
	return false
}

// jsonEqual compares a schema value (decoded as float64) with a document value
// (decoded with UseNumber).
func jsonEqual(schemaVal, docVal any) bool {
	if n, ok := docVal.(json.Number); ok {
		f, err := n.Float64()
		sf, isNum := schemaVal.(float64)
		return err == nil && isNum && f == sf
	}
	return reflect.DeepEqual(schemaVal, docVal)
}
