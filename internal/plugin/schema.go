package plugin

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
)

// SanitizeSchema converts a tool's JSON Schema (draft 2020-12, as MCP servers
// emit it) to the subset Gemini function declarations accept. It keeps type,
// properties, required, items, enum (strings only), description, nullable,
// and minimum/maximum inside the int32 range. Unions with null become
// nullable; other unions keep their first branch, or one string enum when
// every branch is a string constant. A date or date-time format becomes a
// hint in the description. Keys Gemini rejects ($schema, $ref, $defs,
// additionalProperties, format, default, examples and the rest) are dropped.
// The result is always an object schema.
//
// Gemini rejects a nested object without properties, so a free-form object
// below the top level (a map, z.record, a dropped $ref, an untyped value)
// becomes a string the model fills with JSON. SanitizeSchema returns the
// paths of those arguments, as property names with "[]" for array elements;
// the registry decodes them back to objects before calling the plugin.
func SanitizeSchema(schema map[string]any) (map[string]any, [][]string) {
	empty := map[string]any{"type": "object", "properties": map[string]any{}}
	if schema == nil {
		return empty, nil
	}
	s := &sanitizer{}
	out := s.node(schema, nil)
	if out["type"] != "object" {
		return empty, nil
	}
	return out, s.jsonArgs
}

const (
	hintJSONObject = "(a JSON object, sent as a string)"
	hintJSONValue  = "(any JSON value; send objects and lists as JSON text)"
)

type sanitizer struct {
	jsonArgs [][]string
}

func (s *sanitizer) node(in map[string]any, path []string) map[string]any {
	node, nullable := flatten(in)
	typ, _ := node["type"].(string)
	desc, _ := node["description"].(string)
	if typ == "" || typ == "null" {
		switch {
		case typ == "null":
			nullable = true
			typ = "string"
		case node["properties"] != nil:
			typ = "object"
		case node["items"] != nil:
			typ = "array"
		case node["enum"] != nil:
			typ = "string"
		case len(path) > 0:
			return s.jsonString(path, desc, hintJSONValue, nullable)
		default:
			typ = "string"
		}
	}

	out := map[string]any{"type": typ}
	if nullable || node["nullable"] == true {
		out["nullable"] = true
	}

	switch typ {
	case "object":
		props := map[string]any{}
		if p, ok := node["properties"].(map[string]any); ok {
			for name, v := range p {
				if m, ok := v.(map[string]any); ok {
					props[name] = s.node(m, append(slices.Clip(path), name))
				}
			}
		}
		if len(props) == 0 && len(path) > 0 {
			return s.jsonString(path, desc, hintJSONObject, nullable || node["nullable"] == true)
		}
		out["properties"] = props
		if req := requiredNames(node["required"], props); len(req) > 0 {
			out["required"] = req
		}
	case "array":
		items := map[string]any{"type": "string"}
		if m, ok := node["items"].(map[string]any); ok {
			items = s.node(m, append(slices.Clip(path), "[]"))
		}
		out["items"] = items
	case "string":
		if vals, ok := stringEnum(node["enum"]); ok && len(vals) > 0 {
			out["enum"] = vals
		}
		switch f, _ := node["format"].(string); f {
		case "date":
			desc = appendSentence(desc, "(YYYY-MM-DD)")
		case "date-time":
			desc = appendSentence(desc, "(RFC 3339 date-time)")
		}
	case "integer", "number":
		for _, k := range []string{"minimum", "maximum"} {
			if v, ok := toFloat(node[k]); ok && v >= math.MinInt32 && v <= math.MaxInt32 {
				out[k] = v
			}
		}
		// Gemini takes enum on strings only; keep the allowed values as a hint.
		if vals, ok := node["enum"].([]any); ok && len(vals) > 0 {
			desc = appendSentence(desc, "Allowed values: "+joinValues(vals)+".")
		}
	}
	if desc != "" {
		out["description"] = desc
	}
	return out
}

// jsonString declares an argument the model sends as JSON text and records its path.
func (s *sanitizer) jsonString(path []string, desc, hint string, nullable bool) map[string]any {
	s.jsonArgs = append(s.jsonArgs, slices.Clone(path))
	out := map[string]any{"type": "string", "description": appendSentence(desc, hint)}
	if nullable {
		out["nullable"] = true
	}
	return out
}

// DecodeJSONArgs replaces, in place, each argument at paths that holds JSON
// text for an object or a list with the decoded value. Anything else is left
// as it is, so the plugin's own validation reports a bad value.
func DecodeJSONArgs(args map[string]any, paths [][]string) {
	for _, p := range paths {
		decodeAt(args, p)
	}
}

func decodeAt(v any, path []string) any {
	if len(path) == 0 {
		text, ok := v.(string)
		if !ok {
			return v
		}
		var decoded any
		if err := json.Unmarshal([]byte(text), &decoded); err == nil {
			switch decoded.(type) {
			case map[string]any, []any:
				return decoded
			}
		}
		return v
	}
	switch x := v.(type) {
	case map[string]any:
		if child, ok := x[path[0]]; ok && path[0] != "[]" {
			x[path[0]] = decodeAt(child, path[1:])
		}
	case []any:
		if path[0] == "[]" {
			for i := range x {
				x[i] = decodeAt(x[i], path[1:])
			}
		}
	}
	return v
}

// flatten resolves the constructs Gemini lacks (type arrays, anyOf, oneOf,
// allOf, const) into one plain node, reporting whether null was allowed.
func flatten(in map[string]any) (map[string]any, bool) {
	node := in
	nullable := false

	if arr, ok := node["type"].([]any); ok {
		node = clone(node)
		delete(node, "type")
		for _, t := range arr {
			s, _ := t.(string)
			switch {
			case s == "null":
				nullable = true
			case s != "" && node["type"] == nil:
				node["type"] = s
			}
		}
	}

	for _, key := range []string{"anyOf", "oneOf"} {
		branches, ok := node[key].([]any)
		if !ok {
			continue
		}
		var rest []map[string]any
		for _, b := range branches {
			m, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if m["type"] == "null" {
				nullable = true
				continue
			}
			rest = append(rest, m)
		}
		merged := overlay(mergeBranches(rest), node, key)
		var n bool
		merged, n = flatten(merged)
		return merged, nullable || n
	}

	if branches, ok := node["allOf"].([]any); ok {
		merged := map[string]any{}
		for _, b := range branches {
			if m, ok := b.(map[string]any); ok {
				m, _ = flatten(m)
				merged = mergeObjects(merged, m)
			}
		}
		merged = overlay(merged, node, "allOf")
		var n bool
		merged, n = flatten(merged)
		return merged, nullable || n
	}

	if c, ok := node["const"]; ok {
		node = clone(node)
		delete(node, "const")
		if s, ok := c.(string); ok {
			node["type"] = "string"
			node["enum"] = []any{s}
		}
	}
	return node, nullable
}

// mergeBranches picks what a union of non-null branches becomes.
func mergeBranches(branches []map[string]any) map[string]any {
	switch len(branches) {
	case 0:
		return map[string]any{"type": "string"}
	case 1:
		return branches[0]
	}
	var values []any
	for _, b := range branches {
		b, _ = flatten(b)
		vals, ok := stringEnum(b["enum"])
		if !ok || b["type"] != "string" {
			return branches[0]
		}
		for _, v := range vals {
			values = append(values, v)
		}
	}
	return map[string]any{"type": "string", "enum": values}
}

// overlay copies the keys of outer (except skip) onto base. The outer
// description wins; other keys fill only what base lacks.
func overlay(base, outer map[string]any, skip string) map[string]any {
	out := clone(base)
	for k, v := range outer {
		if k == skip {
			continue
		}
		if _, exists := out[k]; !exists || k == "description" {
			out[k] = v
		}
	}
	return out
}

func mergeObjects(a, b map[string]any) map[string]any {
	out := clone(a)
	for k, v := range b {
		switch k {
		case "properties":
			props, _ := out["properties"].(map[string]any)
			props = clone(props)
			if bp, ok := v.(map[string]any); ok {
				for name, s := range bp {
					props[name] = s
				}
			}
			out["properties"] = props
		case "required":
			req, _ := out["required"].([]any)
			if br, ok := v.([]any); ok {
				req = append(slices.Clone(req), br...)
			}
			out["required"] = req
		default:
			if _, exists := out[k]; !exists {
				out[k] = v
			}
		}
	}
	return out
}

func clone(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// requiredNames keeps the required names that are real properties, in order.
func requiredNames(v any, props map[string]any) []string {
	var names []string
	add := func(s string) {
		if _, ok := props[s]; ok && !slices.Contains(names, s) {
			names = append(names, s)
		}
	}
	switch req := v.(type) {
	case []any:
		for _, r := range req {
			if s, ok := r.(string); ok {
				add(s)
			}
		}
	case []string:
		for _, s := range req {
			add(s)
		}
	}
	return names
}

func stringEnum(v any) ([]any, bool) {
	var vals []any
	switch e := v.(type) {
	case []any:
		for _, x := range e {
			s, ok := x.(string)
			if !ok {
				return nil, false
			}
			vals = append(vals, s)
		}
	case []string:
		for _, s := range e {
			vals = append(vals, s)
		}
	default:
		return nil, false
	}
	return vals, true
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func joinValues(vals []any) string {
	parts := make([]string, 0, len(vals))
	for _, v := range vals {
		parts = append(parts, fmt.Sprint(v))
	}
	return strings.Join(parts, ", ")
}

func appendSentence(desc, s string) string {
	if desc == "" {
		return s
	}
	return strings.TrimRight(desc, " ") + " " + s
}

// AccountFieldsFromSchema reads a connect tool's input schema as form inputs:
// property title → label, description → hint, format "password" → secret,
// boolean → checkbox. JSON objects carry no key order once decoded, so
// required fields come first in their listed order, then optional text
// fields, then checkboxes, each group sorted by name.
func AccountFieldsFromSchema(schema map[string]any) []AccountField {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	required := requiredNames(schema["required"], props)

	var fields []AccountField
	for name, v := range props {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		node, _ := flatten(m)
		title, _ := node["title"].(string)
		desc, _ := node["description"].(string)
		format, _ := node["format"].(string)
		f := AccountField{
			Name:     name,
			Label:    title,
			Hint:     desc,
			Secret:   format == "password" || node["writeOnly"] == true,
			Checkbox: node["type"] == "boolean",
			Optional: !slices.Contains(required, name),
		}
		if f.Checkbox {
			f.Optional = true
		}
		if f.Label == "" {
			f.Label, f.Hint = desc, ""
		}
		if f.Label == "" {
			f.Label = name
		}
		fields = append(fields, f)
	}

	rank := func(f AccountField) int {
		switch {
		case f.Checkbox:
			return len(required) + 1
		case !f.Optional:
			return slices.Index(required, f.Name)
		default:
			return len(required)
		}
	}
	sort.Slice(fields, func(i, j int) bool {
		ri, rj := rank(fields[i]), rank(fields[j])
		if ri != rj {
			return ri < rj
		}
		return fields[i].Name < fields[j].Name
	})
	return fields
}
