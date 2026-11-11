package journal

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// jsonSchema interprets the JSON Schema keywords the v1 schema uses: $ref,
// type, const, enum, pattern, minLength, minimum, maximum, format, required,
// properties, additionalProperties, anyOf, allOf, if/then/else.
type jsonSchema struct {
	root map[string]any
}

func loadSchema(t testing.TB) *jsonSchema {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(contractDir, "request-journal-v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	return &jsonSchema{root: root}
}

func (s *jsonSchema) validateRecord(rec map[string]any) error {
	return s.validate(s.root, rec, "$")
}

func (s *jsonSchema) validate(schema map[string]any, v any, at string) error {
	if ref, ok := schema["$ref"].(string); ok {
		def, _ := s.root["$defs"].(map[string]any)[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
		if def == nil {
			return fmt.Errorf("%s: unresolved %s", at, ref)
		}
		if err := s.validate(def, v, at); err != nil {
			return err
		}
	}
	if want, ok := schema["type"]; ok && !matchesType(want, v) {
		return fmt.Errorf("%s: %v is not %v", at, v, want)
	}
	if c, ok := schema["const"]; ok && !sameValue(c, v) {
		return fmt.Errorf("%s: %v is not %v", at, v, c)
	}
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			found = found || sameValue(e, v)
		}
		if !found {
			return fmt.Errorf("%s: %v not in %v", at, v, enum)
		}
	}
	if str, ok := v.(string); ok {
		if p, ok := schema["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(str) {
			return fmt.Errorf("%s: %q does not match %s", at, str, p)
		}
		if n, ok := schema["minLength"].(float64); ok && float64(len([]rune(str))) < n {
			return fmt.Errorf("%s: %q is too short", at, str)
		}
		if schema["format"] == "date-time" {
			if _, err := time.Parse(time.RFC3339Nano, str); err != nil {
				return fmt.Errorf("%s: %v", at, err)
			}
		}
	}
	if n, ok := number(v); ok {
		if min, ok := schema["minimum"].(float64); ok && n.Cmp(big.NewFloat(min)) < 0 {
			return fmt.Errorf("%s: %v below %v", at, v, min)
		}
		if max, ok := schema["maximum"].(float64); ok && n.Cmp(big.NewFloat(max)) > 0 {
			return fmt.Errorf("%s: %v above %v", at, v, max)
		}
	}
	if obj, ok := v.(map[string]any); ok {
		props, _ := schema["properties"].(map[string]any)
		for _, name := range asStrings(schema["required"]) {
			if _, ok := obj[name]; !ok {
				return fmt.Errorf("%s: missing %s", at, name)
			}
		}
		for name, value := range obj {
			sub, ok := props[name].(map[string]any)
			if !ok {
				if schema["additionalProperties"] == false {
					return fmt.Errorf("%s: unexpected %s", at, name)
				}
				continue
			}
			if err := s.validate(sub, value, at+"."+name); err != nil {
				return err
			}
		}
	}
	if anyOf, ok := schema["anyOf"].([]any); ok {
		var errs []string
		for _, sub := range anyOf {
			err := s.validate(sub.(map[string]any), v, at)
			if err == nil {
				errs = nil
				break
			}
			errs = append(errs, err.Error())
		}
		if errs != nil {
			return fmt.Errorf("%s: no anyOf branch matched: %s", at, strings.Join(errs, "; "))
		}
	}
	if allOf, ok := schema["allOf"].([]any); ok {
		for _, sub := range allOf {
			if err := s.validate(sub.(map[string]any), v, at); err != nil {
				return err
			}
		}
	}
	if cond, ok := schema["if"].(map[string]any); ok {
		branch := "else"
		if s.validate(cond, v, at) == nil {
			branch = "then"
		}
		if sub, ok := schema[branch].(map[string]any); ok {
			if err := s.validate(sub, v, at); err != nil {
				return fmt.Errorf("%s (%s): %w", at, branch, err)
			}
		}
	}
	return nil
}

func matchesType(want, v any) bool {
	if list, ok := want.([]any); ok {
		for _, w := range list {
			if matchesType(w, v) {
				return true
			}
		}
		return false
	}
	switch want {
	case "null":
		return v == nil
	case "string":
		_, ok := v.(string)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "integer":
		n, ok := number(v)
		return ok && n.IsInt()
	case "number":
		_, ok := number(v)
		return ok
	}
	return false
}

func number(v any) (*big.Float, bool) {
	var s string
	switch n := v.(type) {
	case json.Number:
		s = n.String()
	case float64:
		return big.NewFloat(n), true
	default:
		return nil, false
	}
	f, _, err := big.ParseFloat(s, 10, 256, big.ToNearestEven)
	return f, err == nil
}

func sameValue(a, b any) bool {
	na, okA := number(a)
	nb, okB := number(b)
	if okA || okB {
		return okA && okB && na.Cmp(nb) == 0
	}
	return reflect.DeepEqual(a, b)
}

func asStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, item.(string))
	}
	return out
}
