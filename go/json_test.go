package main

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"testing"
)

// The hand-written decoder must agree with encoding/json decoding into an interface.
func sameAsStdlib(t *testing.T, input string) {
	t.Helper()
	var want any
	wantErr := json.Unmarshal([]byte(input), &want)
	got, gotErr := parseJSON([]byte(input))
	if (wantErr == nil) != (gotErr == nil) {
		t.Fatalf("%q: stdlib err=%v, ours err=%v", input, wantErr, gotErr)
	}
	if wantErr == nil && !reflect.DeepEqual(want, got) {
		t.Fatalf("%q: stdlib %#v, ours %#v", input, want, got)
	}
}

func TestParseJSONCases(t *testing.T) {
	for _, c := range []string{
		`{}`, `[]`, `null`, `true`, `false`, `0`, `-0`, `1.5e3`, `-12.25E-2`, `"x"`, ` {"a" : [1, 2, {"b": null}] } `,
		`{"title":"t","body":"line\nnext \"q\" \\ \/ \b\f\r\t","labels":["bug"]}`,
		`"é中😀"`, `"\ud83d"`, `"\ud83dx"`, `"\udc00\ud83d"`, "\"caf\xc3\xa9\"", "\"bad\xff\xfeutf\"",
		`{"a":1,"a":2}`, `{"name":`, `{"name":"x",}`, `[1,]`, `01`, `1.`, `.5`, `-`, `+1`, `1e`, `"\x"`, `"a` + "\n" + `b"`,
		`{"a":1} x`, ``, ` `, `nul`, `[1 2]`, `{"a" 1}`, `{1:2}`, `"\u12"`, `1e400`,
	} {
		sameAsStdlib(t, c)
	}
}

func TestParseJSONRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	alphabet := []byte(`{}[]":,0123456789.-+eEtrufalsn \\/bfnrtu` + "\xc3\xa9\xff\n")
	for i := 0; i < 200000; i++ {
		n := rng.Intn(24)
		b := make([]byte, n)
		for j := range b {
			b[j] = alphabet[rng.Intn(len(alphabet))]
		}
		sameAsStdlib(t, string(b))
	}
}
