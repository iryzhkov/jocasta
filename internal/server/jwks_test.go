package server

import (
	"encoding/json"
	"testing"
)

func TestJWKSPublicKeyConstraints(t *testing.T) {
	f := newOIDCFixture(t)
	var source map[string][]map[string]any
	if e := json.Unmarshal([]byte(jwks(t, f.key, "one")), &source); e != nil {
		t.Fatal(e)
	}
	key := source["keys"][0]
	for _, tt := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"no-kid", func(k map[string]any) { delete(k, "kid") }},
		{"private", func(k map[string]any) { k["d"] = "AQ" }},
		{"wrong-alg", func(k map[string]any) { k["alg"] = "HS256" }},
		{"encryption-key", func(k map[string]any) { k["use"] = "enc" }},
		{"sign-operation", func(k map[string]any) { k["key_ops"] = []string{"sign"} }},
		{"small-modulus", func(k map[string]any) { k["n"] = "AQ" }},
		{"invalid-base64", func(k map[string]any) { k["n"] = "not+base64" }},
		{"even-exponent", func(k map[string]any) { k["e"] = "BA" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			copy := map[string]any{}
			for name, value := range key {
				copy[name] = value
			}
			tt.change(copy)
			raw, _ := json.Marshal(map[string]any{"keys": []any{copy}})
			if _, e := parseJWKS(raw); e == nil {
				t.Fatal("accepted unusable key")
			}
		})
	}
	for _, keys := range [][]map[string]any{
		{key, key},
		make([]map[string]any, 17),
	} {
		raw, _ := json.Marshal(map[string]any{"keys": keys})
		if _, e := parseJWKS(raw); e == nil {
			t.Fatal("duplicate/unbounded keys")
		}
	}
}
