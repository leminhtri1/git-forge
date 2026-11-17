package main

// JSON in, JSON out. Output uses Obj, an ordered object, so replies keep GitHub's field order (and GraphQL
// keeps the selection order) without encoding/json's map sorting. Input is decoded by encoding/json.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
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

// parseJSON decodes any JSON value: objects become map[string]any, arrays []any, numbers float64,
// as encoding/json decodes into an interface (strings are copied out of data, so data may be reused).
func parseJSON(data []byte) (any, error) {
	d := jsonDecoder{data: data}
	d.skipSpace()
	v, err := d.value(0)
	if err != nil {
		return nil, err
	}
	d.skipSpace()
	if d.at != len(d.data) {
		return nil, d.errorf("invalid character after top-level value")
	}
	return v, nil
}

type jsonDecoder struct {
	data []byte
	at   int
}

func (d *jsonDecoder) errorf(msg string) error {
	return fmt.Errorf("json: %s at offset %d", msg, d.at)
}

func (d *jsonDecoder) skipSpace() {
	for d.at < len(d.data) {
		switch d.data[d.at] {
		case ' ', '\t', '\n', '\r':
			d.at++
		default:
			return
		}
	}
}

func (d *jsonDecoder) value(depth int) (any, error) {
	if depth > 10000 {
		return nil, d.errorf("exceeded max depth")
	}
	if d.at >= len(d.data) {
		return nil, d.errorf("unexpected end of JSON input")
	}
	switch c := d.data[d.at]; {
	case c == '{':
		d.at++
		m := map[string]any{}
		d.skipSpace()
		if d.at < len(d.data) && d.data[d.at] == '}' {
			d.at++
			return m, nil
		}
		for {
			d.skipSpace()
			if d.at >= len(d.data) || d.data[d.at] != '"' {
				return nil, d.errorf("expected object key")
			}
			key, err := d.str()
			if err != nil {
				return nil, err
			}
			d.skipSpace()
			if d.at >= len(d.data) || d.data[d.at] != ':' {
				return nil, d.errorf("expected ':' after object key")
			}
			d.at++
			d.skipSpace()
			v, err := d.value(depth + 1)
			if err != nil {
				return nil, err
			}
			m[key] = v
			d.skipSpace()
			if d.at >= len(d.data) {
				return nil, d.errorf("unexpected end of JSON input")
			}
			if d.data[d.at] == ',' {
				d.at++
				continue
			}
			if d.data[d.at] == '}' {
				d.at++
				return m, nil
			}
			return nil, d.errorf("expected ',' or '}' in object")
		}
	case c == '[':
		d.at++
		list := []any{}
		d.skipSpace()
		if d.at < len(d.data) && d.data[d.at] == ']' {
			d.at++
			return list, nil
		}
		for {
			d.skipSpace()
			v, err := d.value(depth + 1)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
			d.skipSpace()
			if d.at >= len(d.data) {
				return nil, d.errorf("unexpected end of JSON input")
			}
			if d.data[d.at] == ',' {
				d.at++
				continue
			}
			if d.data[d.at] == ']' {
				d.at++
				return list, nil
			}
			return nil, d.errorf("expected ',' or ']' in array")
		}
	case c == '"':
		return d.str()
	case c == 't':
		return d.word("true", true)
	case c == 'f':
		return d.word("false", false)
	case c == 'n':
		return d.word("null", nil)
	case c == '-' || (c >= '0' && c <= '9'):
		return d.number()
	}
	return nil, d.errorf("invalid character looking for beginning of value")
}

func (d *jsonDecoder) word(w string, v any) (any, error) {
	if !bytes.HasPrefix(d.data[d.at:], []byte(w)) {
		return nil, d.errorf("invalid literal")
	}
	d.at += len(w)
	return v, nil
}

func (d *jsonDecoder) number() (any, error) {
	start := d.at
	if d.data[d.at] == '-' {
		d.at++
	}
	digits := func() int {
		n := 0
		for d.at < len(d.data) && d.data[d.at] >= '0' && d.data[d.at] <= '9' {
			d.at++
			n++
		}
		return n
	}
	if d.at < len(d.data) && d.data[d.at] == '0' {
		d.at++
	} else if digits() == 0 {
		return nil, d.errorf("invalid number")
	}
	if d.at < len(d.data) && d.data[d.at] == '.' {
		d.at++
		if digits() == 0 {
			return nil, d.errorf("invalid number")
		}
	}
	if d.at < len(d.data) && (d.data[d.at] == 'e' || d.data[d.at] == 'E') {
		d.at++
		if d.at < len(d.data) && (d.data[d.at] == '+' || d.data[d.at] == '-') {
			d.at++
		}
		if digits() == 0 {
			return nil, d.errorf("invalid number")
		}
	}
	f, err := strconv.ParseFloat(string(d.data[start:d.at]), 64)
	if err != nil {
		return nil, d.errorf("invalid number")
	}
	return f, nil
}

// str reads a string starting at the opening quote.
func (d *jsonDecoder) str() (string, error) {
	d.at++
	start := d.at
	// Fast path: no escapes, valid UTF-8, no control characters.
	for d.at < len(d.data) {
		c := d.data[d.at]
		if c == '"' {
			s := d.data[start:d.at]
			d.at++
			if utf8.Valid(s) {
				return string(s), nil
			}
			return fixUTF8(string(s)), nil
		}
		if c == '\\' || c < 0x20 {
			break
		}
		d.at++
	}
	var b strings.Builder
	b.Write(d.data[start:d.at])
	for d.at < len(d.data) {
		c := d.data[d.at]
		switch {
		case c == '"':
			d.at++
			return fixUTF8(b.String()), nil
		case c < 0x20:
			return "", d.errorf("invalid character in string literal")
		case c == '\\':
			if d.at+1 >= len(d.data) {
				return "", d.errorf("unexpected end of JSON input")
			}
			e := d.data[d.at+1]
			d.at += 2
			switch e {
			case '"', '\\', '/':
				b.WriteByte(e)
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				r, ok := d.hex4()
				if !ok {
					return "", d.errorf("invalid unicode escape")
				}
				if utf16.IsSurrogate(r) {
					if d.at+1 < len(d.data) && d.data[d.at] == '\\' && d.data[d.at+1] == 'u' {
						save := d.at
						d.at += 2
						if r2, ok2 := d.hex4(); ok2 {
							if pair := utf16.DecodeRune(r, r2); pair != utf8.RuneError {
								b.WriteRune(pair)
								continue
							}
						}
						d.at = save
					}
					r = utf8.RuneError
				}
				b.WriteRune(r)
			default:
				return "", d.errorf("invalid escape in string literal")
			}
		default:
			b.WriteByte(c)
			d.at++
		}
	}
	return "", d.errorf("unexpected end of JSON input")
}

// fixUTF8 replaces every invalid byte with U+FFFD (one per byte, as encoding/json does).
func fixUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteRune(utf8.RuneError)
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func (d *jsonDecoder) hex4() (rune, bool) {
	if d.at+4 > len(d.data) {
		return 0, false
	}
	var r rune
	for _, c := range d.data[d.at : d.at+4] {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			r |= rune(c-'A') + 10
		default:
			return 0, false
		}
	}
	d.at += 4
	return r, true
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
