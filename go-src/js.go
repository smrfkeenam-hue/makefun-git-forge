// JavaScript-semantics helpers: the Node code this Go core ports leans on a few
// JS behaviours (parseInt's lenient prefix parsing, String() coercion, JSON
// escaping) that Go's stdlib does not share. These small functions reproduce
// them exactly where the wire behaviour depends on them.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
)

// jsParseInt parses like JavaScript's parseInt(s, 10): leading whitespace is
// skipped, an optional sign is read, then as many decimal digits as follow.
// It returns ok=false when no digit follows (JS NaN). Only the integer part
// is returned; a fractional or trailing part is ignored, as in JS.
func jsParseInt(s string) (int64, bool) {
	i := 0
	for i < len(s) && isJSWhitespace(s[i]) {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	if i >= len(s) || s[i] < '0' || s[i] > '9' {
		return 0, false
	}
	var n int64
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		n = n*10 + int64(s[i]-'0')
		// JS numbers are doubles; clamp at the safe range rather than overflowing.
		if n > 1<<53 {
			n = 1 << 53
		}
		i++
	}
	if neg {
		n = -n
	}
	return n, true
}

// jsParseFloat parses like JavaScript's parseFloat: the longest valid numeric
// prefix (after leading whitespace and an optional sign). ok=false when the
// string holds no number at all.
func jsParseFloat(s string) (float64, bool) {
	i := 0
	for i < len(s) && isJSWhitespace(s[i]) {
		i++
	}
	start := i
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	// "Infinity" is a valid JS float literal.
	if strings.HasPrefix(s[i:], "Infinity") {
		f := math.Inf(1)
		if start < i && s[start] == '-' {
			f = math.Inf(-1)
		}
		return f, true
	}
	digits := false
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		digits = true
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			digits = true
		}
	}
	if !digits {
		return 0, false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < len(s) && s[j] >= '0' && s[j] <= '9' {
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			i = j
		}
	}
	f, err := strconv.ParseFloat(s[start:i], 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// isJSWhitespace reports whether b is ASCII whitespace in the JS sense (the
// only whitespace the suite's inputs carry).
func isJSWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

// jsString renders a JSON-decoded value the way JavaScript's String() would:
// strings pass through, numbers use JS formatting (integers without a decimal
// point), booleans become "true"/"false", null/undefined become "" here (every
// caller applies the ?? '' fallback first, so nil never reaches String()).
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return jsNumberString(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	default:
		return ""
	}
}

// jsNumberString formats a float64 the way JSON.stringify / String() would for
// the values the suite sends (small integers and short decimals). Integers
// print without a decimal point; other values use the shortest round-trip form.
func jsNumberString(f float64) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e21 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// toInt converts a JSON-decoded or GraphQL-literal number to an int, reporting
// false when the value is not an integer (JS lookups by a fractional number
// never match, so truncation would be wrong).
func toInt(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) || t != math.Trunc(t) {
			return 0, false
		}
		return int64(t), true
	default:
		return 0, false
	}
}

// toIntTrunc converts a number to an int truncating toward zero, the way JS
// ToInteger behaves for slice bounds (Array.slice(0, 2.5) uses 2).
func toIntTrunc(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return 0, false
		}
		return int64(t), true
	default:
		return 0, false
	}
}

// asRecord coerces a decoded JSON value to an object, or an empty one when it
// is not an object (Node's asRecord).
func asRecord(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// asString returns the value when it is a string.
func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// parseJSONBody decodes a request body the way JSON.parse would: an error for
// malformed JSON (the caller answers 400), including trailing garbage after
// the value (JSON.parse rejects it; a bare Decoder.Decode would not).
func parseJSONBody(body []byte) (any, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	// Only whitespace may follow the value.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errTrailingJSON
		}
		return nil, err
	}
	return v, nil
}

var errTrailingJSON = errors.New("unexpected data after JSON value")

// marshalJSON renders a value as JSON the way JSON.stringify would: no HTML
// escaping (Go's default escapes <, >, &; Node's does not), no trailing
// newline (the caller adds one where Node's jsonReply does).
func marshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Encoder.Encode appends a newline; strip it so callers control framing.
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b
}

// jsonStr renders a JSON string literal, fast for plain text: when the string
// holds none of the characters JSON.stringify escapes (", \, control chars
// below 0x20), it is wrapped as-is; otherwise encoding/json does the escaping.
// Output is identical to JSON.stringify for every input.
func jsonStr(s string) string {
	needsEscape := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' || c == '\\' || c < 0x20 {
			needsEscape = true
			break
		}
	}
	if !needsEscape {
		return `"` + s + `"`
	}
	return string(marshalJSON(s))
}
