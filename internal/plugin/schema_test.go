package plugin

import (
	"encoding/json"
	"reflect"
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
			name: "formats other than date and date-time dropped",
			in: `{"type":"object","properties":{"d":{"type":"string","format":"date"},"t":{"type":"string","format":"date-time"},
				"e":{"type":"string","format":"email"},"u":{"type":"string","format":"uri","pattern":"^h","minLength":1}}}`,
			want: `{"type":"object","properties":{"d":{"type":"string","format":"date"},"t":{"type":"string","format":"date-time"},
				"e":{"type":"string"},"u":{"type":"string"}}}`,
		},
		{
			name: "nested arrays and objects, refs and extras dropped",
			in: `{"type":"object","$defs":{"x":{"type":"string"}},"properties":{
				"items":{"type":"array","items":{"type":"object","properties":{"sku":{"type":"string"},"qty":{"type":"number"}},
					"required":["sku","missing"],"additionalProperties":false},"minItems":1},
				"tags":{"type":"array"},
				"ref":{"$ref":"#/$defs/x"},
				"meta":{"type":"object","additionalProperties":{"type":"string"}}},
				"examples":[{}]}`,
			want: `{"type":"object","properties":{
				"items":{"type":"array","items":{"type":"object","properties":{"sku":{"type":"string"},"qty":{"type":"number"}},"required":["sku"]}},
				"tags":{"type":"array","items":{"type":"string"}},
				"ref":{"type":"string"},
				"meta":{"type":"object","properties":{}}}}`,
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
			got := SanitizeSchema(decodeSchema(t, tt.in))
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

func TestSanitizeSchemaNil(t *testing.T) {
	got := SanitizeSchema(nil)
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
