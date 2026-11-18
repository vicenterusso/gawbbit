package pyjson

import "testing"

// FuzzParse checks that parsing never panics and that everything accepted
// re-encodes into a document the parser accepts again.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{`{"a":[1,2.5,-0,"xé"]}`, `NaN`, `{"a":1,"a":2}`, `[[[]]]`, `"😀"`, `1e400`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := Parse(data)
		if err != nil {
			return
		}
		out := Encode(v.Dict())
		if _, err := Parse(out); err != nil {
			t.Fatalf("re-encoded %q does not parse: %v", out, err)
		}
	})
}
