package plugin

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func decodeSchema(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSanitizeSchema(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "MCP Node SDK output",
			in: `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object",
				"properties":{"n":{"type":"integer","minimum":-9007199254740991,"maximum":9007199254740991},
				"text":{"type":"string","description":"Text to echo"}},
				"required":["text"],"additionalProperties":false}`,
			want: `{"type":"object","properties":{"n":{"type":"integer"},
				"text":{"type":"string","description":"Text to echo"}},"required":["text"]}`,
		},
		{
			name: "int32 bounds kept",
			in:   `{"type":"object","properties":{"page":{"type":"integer","minimum":1,"maximum":100,"default":1}}}`,
			want: `{"type":"object","properties":{"page":{"type":"integer","minimum":1,"maximum":100}}}`,
		},
		{
			name: "nullable union and type array",
			in: `{"type":"object","properties":{
				"a":{"anyOf":[{"type":"string"},{"type":"null"}],"description":"A"},
				"b":{"type":["integer","null"]}}}`,
			want: `{"type":"object","properties":{
				"a":{"type":"string","nullable":true,"description":"A"},
				"b":{"type":"integer","nullable":true}}}`,
		},
		{
			name: "union of string constants becomes an enum",
			in:   `{"type":"object","properties":{"kind":{"anyOf":[{"type":"string","const":"a"},{"type":"string","const":"b"}]}}}`,
			want: `{"type":"object","properties":{"kind":{"type":"string","enum":["a","b"]}}}`,
		},
		{
			name: "formats become description hints",
			in: `{"type":"object","properties":{"d":{"type":"string","format":"date","description":"Paid on"},"t":{"type":"string","format":"date-time"},
				"e":{"type":"string","format":"email"},"u":{"type":"string","format":"uri","pattern":"^h","minLength":1}}}`,
			want: `{"type":"object","properties":{"d":{"type":"string","description":"Paid on (YYYY-MM-DD)"},"t":{"type":"string","description":"(RFC 3339 date-time)"},
				"e":{"type":"string"},"u":{"type":"string"}}}`,
		},
		{
			name: "nested arrays and objects, extras dropped",
			in: `{"type":"object","$defs":{"x":{"type":"string"}},"properties":{
				"items":{"type":"array","items":{"type":"object","properties":{"sku":{"type":"string"},"qty":{"type":"number"}},
					"required":["sku","missing"],"additionalProperties":false},"minItems":1},
				"tags":{"type":"array"}},
				"examples":[{}]}`,
			want: `{"type":"object","properties":{
				"items":{"type":"array","items":{"type":"object","properties":{"sku":{"type":"string"},"qty":{"type":"number"}},"required":["sku"]}},
				"tags":{"type":"array","items":{"type":"string"}}}}`,
		},
		{
			name: "string enum kept, numeric enum becomes a hint",
			in:   `{"type":"object","properties":{"s":{"type":"string","enum":["x","y"]},"n":{"type":"integer","enum":[1,2],"description":"Level"}}}`,
			want: `{"type":"object","properties":{"s":{"type":"string","enum":["x","y"]},"n":{"type":"integer","description":"Level Allowed values: 1, 2."}}}`,
		},
		{
			name: "allOf merged",
			in:   `{"allOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},{"properties":{"b":{"type":"boolean"}}}]}`,
			want: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"boolean"}},"required":["a"]}`,
		},
		{
			name: "non-object top level replaced",
			in:   `{"type":"string"}`,
			want: `{"type":"object","properties":{}}`,
		},
		{
			name: "missing properties added",
			in:   `{"type":"object"}`,
			want: `{"type":"object","properties":{}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, jsonArgs := SanitizeSchema(decodeSchema(t, tt.in))
			if len(jsonArgs) != 0 {
				t.Errorf("jsonArgs = %v, want none", jsonArgs)
			}
			// Compare through JSON so []string and []any match.
			gotJSON, _ := json.Marshal(got)
			var gotNorm, wantNorm any
			_ = json.Unmarshal(gotJSON, &gotNorm)
			_ = json.Unmarshal([]byte(tt.want), &wantNorm)
			if !reflect.DeepEqual(gotNorm, wantNorm) {
				t.Errorf("SanitizeSchema()\n got  %s\n want %s", gotJSON, tt.want)
			}
		})
	}
}

// Gemini rejects nested objects without properties, so free-form values
// below the top level become JSON strings and their paths are reported.
func TestSanitizeSchemaFreeFormObjects(t *testing.T) {
	in := decodeSchema(t, `{"type":"object","$defs":{"x":{"type":"string"}},"properties":{
		"meta":{"type":"object","additionalProperties":{"type":"string"},"description":"Extra fields"},
		"ref":{"$ref":"#/$defs/x"},
		"any":{"description":"Anything"},
		"maybe":{"anyOf":[{"type":"object"},{"type":"null"}]},
		"rows":{"type":"array","items":{"type":"object","properties":{"memo":{"type":"object"}}}},
		"blobs":{"type":"array","items":{"type":"object"}},
		"plain":{"type":"string"}}}`)
	got, jsonArgs := SanitizeSchema(in)
	want := `{"type":"object","properties":{
		"meta":{"type":"string","description":"Extra fields (a JSON object, sent as a string)"},
		"ref":{"type":"string","description":"(any JSON value; send objects and lists as JSON text)"},
		"any":{"type":"string","description":"Anything (any JSON value; send objects and lists as JSON text)"},
		"maybe":{"type":"string","nullable":true,"description":"(a JSON object, sent as a string)"},
		"rows":{"type":"array","items":{"type":"object","properties":{"memo":{"type":"string","description":"(a JSON object, sent as a string)"}}}},
		"blobs":{"type":"array","items":{"type":"string","description":"(a JSON object, sent as a string)"}},
		"plain":{"type":"string"}}}`
	gotJSON, _ := json.Marshal(got)
	var gotNorm, wantNorm any
	_ = json.Unmarshal(gotJSON, &gotNorm)
	_ = json.Unmarshal([]byte(want), &wantNorm)
	if !reflect.DeepEqual(gotNorm, wantNorm) {
		t.Errorf("SanitizeSchema()\n got  %s\n want %s", gotJSON, want)
	}
	paths := map[string]bool{}
	for _, p := range jsonArgs {
		paths[strings.Join(p, ".")] = true
	}
	wantPaths := map[string]bool{"meta": true, "ref": true, "any": true, "maybe": true, "rows.[].memo": true, "blobs.[]": true}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Errorf("jsonArgs = %v, want %v", paths, wantPaths)
	}

	// A free-form object at the top level stays an object: the model can
	// still call a tool without arguments.
	top, topArgs := SanitizeSchema(decodeSchema(t, `{"type":"object","additionalProperties":true}`))
	if top["type"] != "object" || len(topArgs) != 0 {
		t.Errorf("top-level free-form = %v, %v", top, topArgs)
	}
}

func TestDecodeJSONArgs(t *testing.T) {
	args := map[string]any{
		"meta":  `{"a":1}`,
		"list":  `[1,2]`,
		"plain": `not json`,
		"num":   `42`,
		"rows":  []any{map[string]any{"memo": `{"k":"v"}`}, map[string]any{"memo": "x"}, "skip"},
		"blobs": []any{`{"b":true}`, 7},
		"done":  map[string]any{"already": "object"},
	}
	DecodeJSONArgs(args, [][]string{{"meta"}, {"list"}, {"plain"}, {"num"}, {"rows", "[]", "memo"}, {"blobs", "[]"}, {"done"}, {"missing", "x"}})
	want := map[string]any{
		"meta":  map[string]any{"a": float64(1)},
		"list":  []any{float64(1), float64(2)},
		"plain": "not json",
		"num":   "42",
		"rows":  []any{map[string]any{"memo": map[string]any{"k": "v"}}, map[string]any{"memo": "x"}, "skip"},
		"blobs": []any{map[string]any{"b": true}, 7},
		"done":  map[string]any{"already": "object"},
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("DecodeJSONArgs() =\n %#v\nwant\n %#v", args, want)
	}
}

func TestSanitizeSchemaNil(t *testing.T) {
	got, _ := SanitizeSchema(nil)
	if got["type"] != "object" {
		t.Errorf("SanitizeSchema(nil) = %v", got)
	}
}

func TestAccountFieldsFromSchema(t *testing.T) {
	schema := decodeSchema(t, `{"type":"object","properties":{
		"allowKick":{"type":"boolean","title":"기존 접속을 끊어도 됩니다"},
		"password":{"type":"string","title":"비밀번호","format":"password"},
		"username":{"type":"string","title":"아이디","description":"로그인할 때 쓰는 아이디"},
		"otp":{"type":"string","description":"OTP"}},
		"required":["username","password"]}`)
	got := AccountFieldsFromSchema(schema)
	want := []AccountField{
		{Name: "username", Label: "아이디", Hint: "로그인할 때 쓰는 아이디"},
		{Name: "password", Label: "비밀번호", Secret: true},
		{Name: "otp", Label: "OTP", Optional: true},
		{Name: "allowKick", Label: "기존 접속을 끊어도 됩니다", Optional: true, Checkbox: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AccountFieldsFromSchema()\n got  %+v\n want %+v", got, want)
	}
	if AccountFieldsFromSchema(map[string]any{"type": "object"}) != nil {
		t.Error("schema without properties should give no fields")
	}
}
