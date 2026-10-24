package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

func newOperatorFixture(t *testing.T) (*mcpFixture, *oidcFixture) {
	t.Helper()
	f := newMCPFixture(t)
	o := newOIDCFixture(t)
	v, err := NewOperatorOIDC(OperatorOIDCConfig{Issuer: o.verifier.cfg.Issuer, JWKSURL: o.verifier.cfg.JWKSURL, ClientID: "jocasta-laptop-interactive"})
	if err != nil {
		t.Fatal(err)
	}
	v.client.Transport = o.tls.Client().Transport
	v.now = o.verifier.now
	o.verifier = v
	f.server.cfg.OperatorOIDC = v
	// Configure before HTTP use; no request has started.
	if err = f.server.EnableMCP(f.server.mcpResource, true); err != nil {
		t.Fatal(err)
	}
	return f, o
}
func operatorClaims(f *mcpFixture, o *oidcFixture, write bool) jwt.MapClaims {
	c := o.claims()
	delete(c, "jocasta_projects")
	c["aud"] = f.server.mcpResource
	c["azp"] = "jocasta-laptop-interactive"
	c["jocasta_operator"] = true
	c["scope"] = "jocasta.read offline_access"
	if write {
		c["scope"] = "jocasta.read jocasta.write offline_access"
	}
	return c
}
func TestOperatorStrictClaimsIssuerSelectionAndRESTBoundary(t *testing.T) {
	f, o := newOperatorFixture(t)
	good := operatorClaims(f, o, true)
	if _, e := o.verifier.verifyAudience(context.Background(), signed(t, o.key, good), f.server.mcpResource); e != nil {
		t.Fatal(e)
	}
	changes := []struct {
		name   string
		change func(jwt.MapClaims)
	}{
		{"false", func(c jwt.MapClaims) { c["jocasta_operator"] = false }},
		{"null", func(c jwt.MapClaims) { c["jocasta_operator"] = nil }},
		{"string", func(c jwt.MapClaims) { c["jocasta_operator"] = "true" }},
		{"number", func(c jwt.MapClaims) { c["jocasta_operator"] = 1 }},
		{"array", func(c jwt.MapClaims) { c["jocasta_operator"] = []bool{true} }},
		{"missing", func(c jwt.MapClaims) { delete(c, "jocasta_operator") }},
		{"projects-empty", func(c jwt.MapClaims) { c["jocasta_projects"] = []string{} }},
		{"projects-null", func(c jwt.MapClaims) { c["jocasta_projects"] = nil }},
		{"projects-full", func(c jwt.MapClaims) { c["jocasta_projects"] = []string{"A"} }},
		{"azp-missing", func(c jwt.MapClaims) { delete(c, "azp") }},
		{"azp-other", func(c jwt.MapClaims) { c["azp"] = "other" }},
		{"azp-array", func(c jwt.MapClaims) { c["azp"] = []string{"jocasta-laptop-interactive"} }},
		{"scope-array", func(c jwt.MapClaims) { c["scope"] = []string{"jocasta.read", "jocasta.write"} }},
		{"scope-null", func(c jwt.MapClaims) { c["scope"] = nil }},
		{"write-without-read", func(c jwt.MapClaims) { c["scope"] = "jocasta.write" }},
		{"audience", func(c jwt.MapClaims) { c["aud"] = "wrong" }},
		{"combined-audience", func(c jwt.MapClaims) { c["aud"] = []string{f.server.mcpResource, "other"} }},
		{"duplicate-audience", func(c jwt.MapClaims) { c["aud"] = []string{f.server.mcpResource, f.server.mcpResource} }},
		{"issuer", func(c jwt.MapClaims) { c["iss"] = "https://unknown.invalid/" }},
		{"subject", func(c jwt.MapClaims) { delete(c, "sub") }},
		{"missing-exp", func(c jwt.MapClaims) { delete(c, "exp") }},
		{"missing-iat", func(c jwt.MapClaims) { delete(c, "iat") }},
		{"expired", func(c jwt.MapClaims) {
			c["iat"] = o.clock.Load() - 400
			c["nbf"] = c["iat"]
			c["exp"] = o.clock.Load() - 31
		}},
		{"future", func(c jwt.MapClaims) { c["iat"] = o.clock.Load() + 31 }},
		{"future-nbf", func(c jwt.MapClaims) { c["nbf"] = o.clock.Load() + 31 }},
		{"overlong", func(c jwt.MapClaims) { c["exp"] = o.clock.Load() + 781 }},
	}
	for _, tt := range changes {
		t.Run(tt.name, func(t *testing.T) {
			c := operatorClaims(f, o, true)
			tt.change(c)
			if _, e := o.verifier.verifyAudience(context.Background(), signed(t, o.key, c), f.server.mcpResource); e == nil {
				t.Fatal("invalid accepted")
			}
		})
	}
	// An ordinary ID token has neither an access scope nor the authorized party.
	id := operatorClaims(f, o, false)
	delete(id, "scope")
	delete(id, "azp")
	if status, _ := canaryRequest(t, f.server, signed(t, o.key, id), "POST", "/mcp", "{}"); status != 401 {
		t.Fatal("ID token", status)
	}
	noRead := operatorClaims(f, o, false)
	noRead["scope"] = "offline_access"
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+signed(t, o.key, noRead))
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, req)
	if w.Code != 403 || !strings.Contains(w.Header().Get("WWW-Authenticate"), "insufficient_scope") {
		t.Fatal(w.Code, w.Header())
	}
	for _, op := range []any{true, false, nil, "true"} {
		c := f.oidc.claims("A")
		c["aud"] = f.server.mcpResource
		c["jocasta_operator"] = op
		if status, _ := canaryRequest(t, f.server, signed(t, f.oidc.key, c), "POST", "/mcp", "{}"); status != 401 {
			t.Fatal("limited operator claim", status)
		}
	}
	c := f.oidc.claims("A")
	c["aud"] = f.server.mcpResource
	c["scope"] = "jocasta.read jocasta.write"
	if status, _ := canaryRequest(t, f.server, signed(t, f.oidc.key, c), "POST", "/mcp", "{}"); status != 401 {
		t.Fatal("limited write scope", status)
	}
	// Unknown issuers never trigger either pinned key endpoint.
	before := f.oidc.hits.Load() + o.hits.Load()
	unknown := operatorClaims(f, o, true)
	unknown["iss"] = "https://untrusted.invalid/jwks"
	if status, _ := canaryRequest(t, f.server, signed(t, o.key, unknown), "POST", "/mcp", "{}"); status != 401 || before != f.oidc.hits.Load()+o.hits.Load() {
		t.Fatal("unknown issuer network work", status)
	}
	wrongSignature := signed(t, f.oidc.key, operatorClaims(f, o, true))
	if status, _ := canaryRequest(t, f.server, wrongSignature, "POST", "/mcp", "{}"); status != 401 {
		t.Fatal("wrong signature", status)
	}
	for _, path := range []string{"/v1/get?path=A/a", "/v1/list", "/v1/mutate", "/metrics", "/readyz"} {
		for _, method := range []string{"GET", "POST"} {
			if status, _ := canaryRequest(t, f.server, signed(t, o.key, good), method, path, "{}"); status != 401 {
				t.Fatal("operator REST", method, path, status)
			}
		}
	}
	req = httptest.NewRequest("GET", "/.well-known/oauth-protected-resource/mcp", nil)
	w = httptest.NewRecorder()
	f.server.ServeHTTP(w, req)
	var metadata struct {
		Issuers []string `json:"authorization_servers"`
		Scopes  []string `json:"scopes_supported"`
	}
	if json.Unmarshal(w.Body.Bytes(), &metadata) != nil || len(metadata.Issuers) != 2 || metadata.Issuers[0] != o.verifier.cfg.Issuer || strings.Join(metadata.Scopes, ",") != "jocasta.read,jocasta.write" {
		t.Fatal(w.Body.String())
	}
}
func TestOperatorConfigPins(t *testing.T) {
	for _, c := range []OperatorOIDCConfig{
		{}, {Issuer: "https://issuer", ClientID: "client"}, {Issuer: "https://issuer", JWKSURL: "https://issuer/jwks"},
		{Issuer: "http://issuer", JWKSURL: "https://issuer/jwks", ClientID: "client"}, {Issuer: "https://issuer", JWKSURL: "https://issuer/jwks?caller=url", ClientID: "client"},
	} {
		if _, e := NewOperatorOIDC(c); e == nil {
			t.Fatal("partial or insecure config", c)
		}
	}
	f, o := newOperatorFixture(t)
	o.verifier.cfg.Issuer = f.oidc.verifier.cfg.Issuer
	if e := f.server.EnableMCP(f.server.mcpResource, true); e == nil {
		t.Fatal("same issuer accepted")
	}
}
