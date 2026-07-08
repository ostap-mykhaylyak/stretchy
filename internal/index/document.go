package index

import (
	"encoding/json"
	"strconv"
	"time"
)

// dateFormats covers what WordPress/WooCommerce and ElasticPress emit.
var dateFormats = []string{
	"2006-01-02 15:04:05",
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02",
	"15:04:05",
}

// ParseDate parses a date string into epoch milliseconds.
func ParseDate(s string) (int64, bool) {
	if len(s) < 8 || len(s) > 35 {
		return 0, false
	}
	if s[0] < '0' || s[0] > '9' {
		return 0, false
	}
	for _, f := range dateFormats {
		if t, err := time.Parse(f, s); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

// Flatten walks a decoded JSON object and emits every leaf value under
// its dot-separated path. Arrays contribute multiple values for the
// same path; nested objects inside arrays are flattened the same way
// (nested-type correlation is intentionally not preserved).
func Flatten(source map[string]interface{}) map[string][]interface{} {
	out := map[string][]interface{}{}
	var walk func(prefix string, v interface{})
	walk = func(prefix string, v interface{}) {
		switch t := v.(type) {
		case map[string]interface{}:
			for k, sub := range t {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(p, sub)
			}
		case []interface{}:
			for _, sub := range t {
				walk(prefix, sub)
			}
		case nil:
			// null values are not indexed
		default:
			out[prefix] = append(out[prefix], t)
		}
	}
	walk("", source)
	return out
}

// ToFloat coerces an indexed or query value to float64 for numeric and
// date comparisons.
func ToFloat(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		if ms, ok := ParseDate(t); ok {
			return float64(ms), true
		}
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	}
	return 0, false
}

// ToString renders an indexed value the way it participates in keyword
// term matching.
func ToString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}
