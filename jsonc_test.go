package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

/* --------------------------------------------------------------------------
   jsonc_test.go — JSONC to JSON: comments and trailing commas go, strings stay
   byte for byte (a URL's `//` included), and malformed input stays malformed.
   ------------------------------------------------------------------------ */

func TestStripJSONC_DecodesToTheSameValueAsTheJSONItMeans(t *testing.T) {
	for _, c := range []struct {
		name, in, want string
	}{
		{"strict JSON", `{"model":"a/b","n":[1,2],"o":{}}`, `{"model":"a/b","n":[1,2],"o":{}}`},
		{"a line comment on its own line", "{\n  // the default\n  \"model\": \"a/b\"\n}", `{"model":"a/b"}`},
		{"a line comment after a value", "{\"model\": \"a/b\" // the default\n}", `{"model":"a/b"}`},
		{"a line comment ending the file", "{\"model\": \"a/b\"}\n// done", `{"model":"a/b"}`},
		{"CRLF line endings", "{\r\n  // the default\r\n  \"model\": \"a/b\"\r\n}\r\n", `{"model":"a/b"}`},
		{"a block comment", `{/* the default */"model": "a/b"}`, `{"model":"a/b"}`},
		{"a block comment across lines", "{\n/* one\n   two // not a line comment\n*/\n\"model\": \"a/b\"}", `{"model":"a/b"}`},
		{"a block comment between tokens", `{"a":1/**/,"b":2}`, `{"a":1,"b":2}`},
		{"an empty block comment", `{"a":/**/1}`, `{"a":1}`},
		{"a URL keeps its //", `{"baseURL": "https://api.example.com/v1"} // trailing`, `{"baseURL":"https://api.example.com/v1"}`},
		{"comment markers inside a string are text", `{"a": "x // y", "b": "/* not a comment */", "c": "*/"}`, `{"a":"x // y","b":"/* not a comment */","c":"*/"}`},
		{"an escaped quote does not end the string", `{"q": "say \"hi\" // still text"} // gone`, `{"q":"say \"hi\" // still text"}`},
		{"an escaped backslash ends before the quote", `{"p": "C:\\dir\\"} // gone`, `{"p":"C:\\dir\\"}`},
		{"a trailing comma in an object", `{"a":1,"b":2,}`, `{"a":1,"b":2}`},
		{"a trailing comma in an array", `{"n":[1,2,]}`, `{"n":[1,2]}`},
		{"nested trailing commas", `{"a":{"b":[1,{"c":2,},],},}`, `{"a":{"b":[1,{"c":2}]}}`},
		{"a comment between the comma and the bracket", "{\"a\": 1, // last\n}", `{"a":1}`},
		{"a block comment between the comma and the bracket", `[1, /* c */ ]`, `[1]`},
		{"a comma before a brace inside a string", `{"s": ",}", "t": ",]"}`, `{"s":",}","t":",]"}`},
		{"a byte-order mark", "\xef\xbb\xbf{\"model\": \"a/b\"}", `{"model":"a/b"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got, want any
			if err := json.Unmarshal(stripJSONC([]byte(c.in)), &got); err != nil {
				t.Fatalf("stripped %q = %q does not decode: %v", c.in, stripJSONC([]byte(c.in)), err)
			}
			if err := json.Unmarshal([]byte(c.want), &want); err != nil {
				t.Fatalf("bad expectation %q: %v", c.want, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("stripped %q decodes to %#v, want %#v", c.in, got, want)
			}
		})
	}
}

// Strict JSON passes through byte for byte, and every string is copied verbatim.
func TestStripJSONC_LeavesStrictJSONAndStringsUntouched(t *testing.T) {
	strict := `{"$schema":"https://opencode.ai/config.json","model":"google/gemini-2.5-pro","provider":{"x":{"options":{"baseURL":"http://localhost:11434/v1"}}},"s":"a,} \"//\" \\"}`
	if got := string(stripJSONC([]byte(strict))); got != strict {
		t.Fatalf("strict JSON changed:\n got %s\nwant %s", got, strict)
	}
	jsonc := "{\n  \"url\": \"https://example.com//double\", // why\n  \"glob\": \"src/**/*.go\",\n}"
	got := string(stripJSONC([]byte(jsonc)))
	for _, s := range []string{`"https://example.com//double"`, `"src/**/*.go"`} {
		if !strings.Contains(got, s) {
			t.Fatalf("stripped output %q lost the string %s", got, s)
		}
	}
	if strings.Contains(got, "why") {
		t.Fatalf("stripped output %q kept the comment", got)
	}
}

// Input that is not JSONC stays malformed rather than being repaired into
// something it did not say, and nothing panics.
func TestStripJSONC_MalformedInputStaysMalformed(t *testing.T) {
	for _, in := range []string{
		`{"model": "a/b" /* never closed`,
		`{"model": "a/b"} /`,
		`{"model": "a/b" / 2}`,
		`{"model": "never closed}`,
		`{"a":1,,}`,
		`/*/ {"a":1}`,
		``,
		`/`,
		`"\`,
	} {
		var v any
		if err := json.Unmarshal(stripJSONC([]byte(in)), &v); err == nil {
			t.Errorf("%q decoded as %#v, want an error", in, v)
		}
	}
}

func TestReadJSONCFileWithin(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	var cfg struct {
		Model string `json:"model"`
	}
	if !readJSONCFileWithin(write("ok.jsonc", "{\n// c\n\"model\": \"a/b\",\n}"), 1<<10, &cfg) || cfg.Model != "a/b" {
		t.Fatalf("model = %q, want the JSONC file read", cfg.Model)
	}
	for name, path := range map[string]string{
		"missing":   filepath.Join(dir, "missing.jsonc"),
		"empty":     write("empty.jsonc", ""),
		"oversized": write("big.jsonc", `{"model":"a/b","pad":"`+strings.Repeat("x", 2048)+`"}`),
		"malformed": write("bad.jsonc", `{"model": "a/b" /* never closed`),
	} {
		if readJSONCFileWithin(path, 1<<10, &cfg) {
			t.Errorf("%s file read as present", name)
		}
	}
}
