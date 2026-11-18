package config

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"reflect"
	"testing"
)

// TestSettingsMatchPydantic replays environments evaluated by the original
// pydantic-settings model.
func TestSettingsMatchPydantic(t *testing.T) {
	f, err := os.Open("testdata/settings_golden.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	n := 0
	for sc.Scan() {
		n++
		var row struct {
			Env map[string]string
			Out *struct {
				Port            json.Number       `json:"port"`
				MaxRequestBytes json.Number       `json:"max_request_bytes"`
				StoreRawIP      bool              `json:"store_raw_ip"`
				Inflight        int64             `json:"inflight"`
				APIKeyMap       map[string]string `json:"api_key_map"`
				CORSOrigins     []string          `json:"cors_origins"`
				CORSMethods     []string          `json:"cors_methods"`
				CORSHeaders     []string          `json:"cors_headers"`
			}
			Errors [][2]string
		}
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.UseNumber()
		if err := dec.Decode(&row); err != nil {
			t.Fatal(err)
		}
		var environ []string
		for k, v := range row.Env {
			environ = append(environ, k+"="+v)
		}
		s, err := Load(environ, "")
		if row.Out == nil {
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Errorf("%v: expected errors %v, got %v", row.Env, row.Errors, err)
				continue
			}
			var got [][2]string
			for _, fe := range verr.errors {
				got = append(got, [2]string{fe.alias, fe.errType})
			}
			if !reflect.DeepEqual(got, row.Errors) {
				t.Errorf("%v: errors %v, want %v", row.Env, got, row.Errors)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v: unexpected error %v", row.Env, err)
			continue
		}
		want := row.Out
		if num(want.Port) != s.Port || num(want.MaxRequestBytes) != s.MaxRequestBytes ||
			want.StoreRawIP != s.StoreRawIP || want.Inflight != s.NATSMaxInflightPublishes ||
			!reflect.DeepEqual(want.APIKeyMap, s.APIKeyMap) ||
			!reflect.DeepEqual(want.CORSOrigins, s.CORSOriginsList()) ||
			!reflect.DeepEqual(want.CORSMethods, s.CORSMethodsList()) ||
			!reflect.DeepEqual(want.CORSHeaders, s.CORSHeadersList()) {
			t.Errorf("%v: got %+v, want %+v", row.Env, s, want)
		}
	}
	if n == 0 {
		t.Fatal("no rows")
	}
}

func num(n json.Number) int64 {
	i, err := n.Int64()
	if err != nil {
		return math.MaxInt64
	}
	return i
}
