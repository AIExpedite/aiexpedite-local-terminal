// jsonc.go — JSON with comments (JSONC), the format OpenCode reads its
// configuration in (opencode.jsonc, and opencode.json alike): `//` line and
// `/* */` block comments, and a trailing comma before `}` or `]`.
//
// A comment marker inside a string is text, not a comment — a provider's
// `"baseURL": "https://…"` must keep its `//` — so the scan copies every string
// verbatim, escapes included, and strips only what lies outside one.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
)

// utf8BOM is the byte-order mark an editor on Windows may write first.
var utf8BOM = []byte("\xef\xbb\xbf")

// stripJSONC turns JSONC into JSON: a leading UTF-8 byte-order mark, every
// comment outside a string and every trailing comma are removed. A block
// comment becomes one space, so it never joins the tokens on either side; a
// line comment keeps its line break. Anything else is copied as-is, so input
// that is not JSONC stays malformed and json.Unmarshal still rejects it; an
// unterminated block comment swallows the rest. It never panics.
func stripJSONC(in []byte) []byte {
	in = bytes.TrimPrefix(in, utf8BOM)
	out := make([]byte, 0, len(in))
	inString, escaped := false, false
	for i := 0; i < len(in); i++ {
		c := in[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(in) && in[i+1] == '/':
			// To the end of the line; the line break itself is kept.
			for i+1 < len(in) && in[i+1] != '\n' && in[i+1] != '\r' {
				i++
			}
		case c == '/' && i+1 < len(in) && in[i+1] == '*':
			end := bytes.Index(in[i+2:], []byte("*/"))
			if end < 0 {
				return dropJSONTrailingCommas(out)
			}
			// i lands on the closing '/', which the loop then steps past.
			i += 2 + end + 1
			out = append(out, ' ')
		default:
			out = append(out, c)
		}
	}
	return dropJSONTrailingCommas(out)
}

// dropJSONTrailingCommas removes each comma outside a string whose next
// non-whitespace byte closes an object or array. It runs after the comments are
// gone, so whitespace is all that can sit between the two.
func dropJSONTrailingCommas(in []byte) []byte {
	out := make([]byte, 0, len(in))
	inString, escaped := false, false
	for i := 0; i < len(in); i++ {
		c := in[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case ',':
			j := i + 1
			for j < len(in) && isJSONWhitespace(in[j]) {
				j++
			}
			if j < len(in) && (in[j] == '}' || in[j] == ']') {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

// isJSONWhitespace is JSON's insignificant whitespace (RFC 8259 §2).
func isJSONWhitespace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// readJSONCFileWithin is readJSONFileWithin for a JSONC file: comments and
// trailing commas are stripped before the decode. A missing, empty, oversized
// or malformed file reads as absent. Strict JSON reads exactly as it would
// through readJSONFileWithin, since neither a comment nor a trailing comma can
// occur in it outside a string.
func readJSONCFileWithin(path string, maxBytes int64, into any) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil || len(b) == 0 || int64(len(b)) > maxBytes {
		return false
	}
	return json.Unmarshal(stripJSONC(b), into) == nil
}
