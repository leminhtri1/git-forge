package main

// JSON in, JSON out. Output uses Obj, an ordered object, so replies keep GitHub's field order (and GraphQL
// keeps the selection order) without encoding/json's map sorting. Input is decoded by encoding/json.

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"unicode/utf8"
)

// KV is one member of an ordered JSON object.
type KV struct {
	K string
	V any
}

// Obj is an ordered JSON object.
type Obj []KV

func (o Obj) Get(key string) (any, bool) {
	for _, kv := range o {
		if kv.K == key {
			return kv.V, true
		}
	}
	return nil, false
}

// Set replaces key's value, or appends it.
func (o *Obj) Set(key string, v any) {
	for i := range *o {
		if (*o)[i].K == key {
			(*o)[i].V = v
			return
		}
	}
	*o = append(*o, KV{key, v})
}

// RawJSON is pre-serialised JSON written as-is.
type RawJSON []byte

const hexDigits = "0123456789abcdef"

func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			b.WriteString(s[start:i])
			switch c {
			case '"':
				b.WriteString(`\"`)
			case '\\':
				b.WriteString(`\\`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			case '\b':
				b.WriteString(`\b`)
			case '\f':
				b.WriteString(`\f`)
			default:
				b.WriteString(`\u00`)
				b.WriteByte(hexDigits[c>>4])
				b.WriteByte(hexDigits[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteString(s[start:i])
			b.WriteString(`�`)
			i++
			start = i
			continue
		}
		i += size
	}
	b.WriteString(s[start:])
	b.WriteByte('"')
}

func writeJSON(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writeString(b, x)
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			b.WriteString("null")
		} else if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			b.WriteString(strconv.FormatInt(int64(x), 10))
		} else {
			b.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
		}
	case RawJSON:
		b.Write(x)
	case Obj:
		b.WriteByte('{')
		for i, kv := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, kv.K)
			b.WriteByte(':')
			writeJSON(b, kv.V)
		}
		b.WriteByte('}')
	case *Obj:
		writeJSON(b, *x)
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k)
			b.WriteByte(':')
			writeJSON(b, x[k])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSON(b, e)
		}
		b.WriteByte(']')
	case []Obj:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSON(b, e)
		}
		b.WriteByte(']')
	case []string:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, e)
		}
		b.WriteByte(']')
	default:
		raw, err := json.Marshal(x)
		if err != nil {
			b.WriteString("null")
		} else {
			b.Write(raw)
		}
	}
}

func toJSON(v any) []byte {
	var b bytes.Buffer
	writeJSON(&b, v)
	return b.Bytes()
}

// parseJSON decodes any JSON value (objects become map[string]any, numbers float64).
func parseJSON(data []byte) (any, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// ---- small readers over decoded JSON

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func strOf(m map[string]any, key string) (string, bool) {
	v, ok := m[key].(string)
	return v, ok
}

// intOf reads an integer from a JSON number or a digit string.
func intOf(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			return int(x), true
		}
	case int:
		return x, true
	case string:
		if digitsRE.MatchString(x) {
			n, err := strconv.Atoi(x)
			return n, err == nil
		}
	}
	return 0, false
}
