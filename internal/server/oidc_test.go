package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/iryzhkov/jocasta/internal/cli"
	"github.com/iryzhkov/jocasta/internal/client"
	"github.com/iryzhkov/jocasta/internal/store"
	"github.com/iryzhkov/jocasta/pkg/api"
)

type oidcFixture struct {
	key      *rsa.PrivateKey
	tls      *httptest.Server
	verifier *OIDCVerifier
	clock    atomic.Int64
	hits     atomic.Int64
	mu       sync.Mutex
	body     string
	status   int
}

func jwks(t *testing.T, key *rsa.PrivateKey, kid string) string {
	t.Helper()
	b, e := json.Marshal(map[string]any{"keys": []any{map[string]any{
		"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}})
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func newOIDCFixture(t *testing.T) *oidcFixture {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	f := &oidcFixture{key: key, status: 200, body: jwks(t, key, "one")}
	f.clock.Store(time.Now().Unix())
	f.tls = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		w.WriteHeader(f.status)
		io.WriteString(w, f.body)
	}))
	t.Cleanup(f.tls.Close)
	f.verifier, e = NewOIDC(OIDCConfig{Issuer: f.tls.URL + "/issuer/", Audience: "jocasta-canary", JWKSURL: f.tls.URL + "/jwks"})
	if e != nil {
		t.Fatal(e)
	}
	// Trust only this ephemeral fixture CA. Production has no TLS bypass.
	f.verifier.client.Transport = f.tls.Client().Transport
	f.verifier.now = func() time.Time { return time.Unix(f.clock.Load(), 0) }
	return f
}
func (f *oidcFixture) claims(projects ...string) jwt.MapClaims {
	now := f.clock.Load()
	return jwt.MapClaims{"iss": f.verifier.cfg.Issuer, "aud": "jocasta-canary", "sub": "same-machine",
		"iat": now, "nbf": now, "exp": now + 300, "scope": "openid jocasta.read", "jocasta_projects": projects}
}
func signed(t *testing.T, key *rsa.PrivateKey, c jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	token.Header["kid"] = "one"
	token.Header["jku"] = "https://untrusted.invalid/never-used"
	b, e := token.SignedString(key)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestOIDCClaimsAndSignatures(t *testing.T) {
	f := newOIDCFixture(t)
	if _, e := f.verifier.verify(context.Background(), signed(t, f.key, f.claims("A"))); e != nil {
		t.Fatal(e)
	}
	tests := []struct {
		name   string
		change func(jwt.MapClaims)
	}{
		{"audience", func(c jwt.MapClaims) { c["aud"] = "other" }},
		{"issuer", func(c jwt.MapClaims) { c["iss"] = f.verifier.cfg.Issuer + "different" }},
		{"missing-aud", func(c jwt.MapClaims) { delete(c, "aud") }},
		{"missing-iss", func(c jwt.MapClaims) { delete(c, "iss") }},
		{"missing-sub", func(c jwt.MapClaims) { delete(c, "sub") }},
		{"missing-exp", func(c jwt.MapClaims) { delete(c, "exp") }},
		{"missing-iat", func(c jwt.MapClaims) { delete(c, "iat") }},
		{"expired", func(c jwt.MapClaims) {
			c["iat"] = f.clock.Load() - 600
			c["nbf"] = f.clock.Load() - 600
			c["exp"] = f.clock.Load() - 31
		}},
		{"overlong", func(c jwt.MapClaims) { c["exp"] = f.clock.Load() + 781 }},
		{"future-iat", func(c jwt.MapClaims) { c["iat"] = f.clock.Load() + 31 }},
		{"future-nbf", func(c jwt.MapClaims) { c["nbf"] = f.clock.Load() + 31 }},
		{"nbf-after-exp", func(c jwt.MapClaims) { c["nbf"] = f.clock.Load() + 301 }},
		{"exp-before-iat", func(c jwt.MapClaims) { c["exp"] = f.clock.Load() - 1 }},
		{"missing-scope", func(c jwt.MapClaims) { delete(c, "scope") }},
		{"scope-substring", func(c jwt.MapClaims) { c["scope"] = "jocasta.readwrite" }},
		{"scope-array", func(c jwt.MapClaims) { c["scope"] = []string{"jocasta.read"} }},
		{"missing-projects", func(c jwt.MapClaims) { delete(c, "jocasta_projects") }},
		{"null-projects", func(c jwt.MapClaims) { c["jocasta_projects"] = nil }},
		{"empty-projects", func(c jwt.MapClaims) { c["jocasta_projects"] = []string{} }},
		{"string-projects", func(c jwt.MapClaims) { c["jocasta_projects"] = "A" }},
		{"mixed-projects", func(c jwt.MapClaims) { c["jocasta_projects"] = []any{"A", 7} }},
		{"traversal-project", func(c jwt.MapClaims) { c["jocasta_projects"] = []string{"A/../B"} }},
		{"wildcard-project", func(c jwt.MapClaims) { c["jocasta_projects"] = []string{"*"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := f.claims("A")
			tt.change(c)
			if _, e := f.verifier.verify(context.Background(), signed(t, f.key, c)); e == nil {
				t.Fatal("accepted invalid claims")
			}
		})
	}
	other, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.verifier.verify(context.Background(), signed(t, other, f.claims("A"))); e == nil {
		t.Fatal("wrong signature accepted")
	}
	hs := jwt.NewWithClaims(jwt.SigningMethodHS256, f.claims("A"))
	hs.Header["kid"] = "one"
	raw, e := hs.SignedString(f.key.N.Bytes())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.verifier.verify(context.Background(), raw); e == nil {
		t.Fatal("algorithm confusion")
	}
	good := signed(t, f.key, f.claims("A"))
	parts := strings.Split(good, ".")
	expanded := f.claims("A", "B")
	b, _ := json.Marshal(expanded)
	parts[1] = base64.RawURLEncoding.EncodeToString(b)
	if _, e = f.verifier.verify(context.Background(), strings.Join(parts, ".")); e == nil {
		t.Fatal("unsigned grant expansion accepted")
	}
	c := f.claims("A")
	c["exp"] = f.clock.Load() + 780
	if _, e = f.verifier.verify(context.Background(), signed(t, f.key, c)); e != nil {
		t.Fatal("13 minute bound", e)
	}
	c = f.claims("A")
	c["iat"] = f.clock.Load() + 30
	c["nbf"] = f.clock.Load() + 30
	if _, e = f.verifier.verify(context.Background(), signed(t, f.key, c)); e != nil {
		t.Fatal("clock leeway", e)
	}
}
func TestOIDCJWKSRotationAndFetchBounds(t *testing.T) {
	f := newOIDCFixture(t)
	ctx := context.Background()
	if _, e := f.verifier.key(ctx, "one"); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 20; i++ {
		if _, e := f.verifier.key(ctx, fmt.Sprint("unknown", i)); e == nil {
			t.Fatal("unknown kid")
		}
	}
	if f.hits.Load() != 1 {
		t.Fatal("unbounded unknown-key fetches", f.hits.Load())
	}
	other, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	f.body = jwks(t, other, "two")
	f.mu.Unlock()
	f.clock.Add(5)
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, f.claims("A"))
	token.Header["kid"] = "two"
	raw, e := token.SignedString(other)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.verifier.verify(ctx, raw); e != nil {
		t.Fatal("rotation", e)
	}
	if f.hits.Load() != 2 {
		t.Fatal(f.hits.Load())
	}
	f.mu.Lock()
	f.status = 503
	f.mu.Unlock()
	f.clock.Add(61)
	if _, e = f.verifier.key(ctx, "two"); e == nil {
		t.Fatal("stale key used after failed refresh")
	}
	for i := 0; i < 20; i++ {
		f.verifier.key(ctx, "two")
	}
	if f.hits.Load() != 3 {
		t.Fatal("unbounded failed refreshes", f.hits.Load())
	}
}
func TestOIDCTransportAndConfiguration(t *testing.T) {
	for _, cfg := range []OIDCConfig{
		{Issuer: "http://issuer", Audience: "a", JWKSURL: "https://issuer/keys"},
		{Issuer: "https://issuer", Audience: "a", JWKSURL: "http://issuer/keys"},
		{Issuer: "https://issuer", Audience: "", JWKSURL: "https://issuer/keys"},
		{Issuer: "https://user:password@issuer", Audience: "a", JWKSURL: "https://issuer/keys"},
		{Issuer: "https://issuer", Audience: "a", JWKSURL: "https://issuer/keys?url=other"},
	} {
		if _, e := NewOIDC(cfg); e == nil {
			t.Fatal("insecure/partial config accepted")
		}
	}
	f := newOIDCFixture(t)
	untrusted, e := NewOIDC(f.verifier.cfg)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = untrusted.key(context.Background(), "one"); e == nil {
		t.Fatal("untrusted TLS accepted")
	}
	for _, body := range []string{"{}", strings.Repeat("x", (64<<10)+1), `{"keys":[{"kty":"RSA","kid":"one","n":"AQ","e":"AQAB"}]}`} {
		f.mu.Lock()
		f.body = body
		f.mu.Unlock()
		f.clock.Add(61)
		if _, e = f.verifier.key(context.Background(), "one"); e == nil {
			t.Fatal("invalid or oversized JWKS")
		}
	}
	redirects := atomic.Int64{}
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirects.Add(1) }))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer redirect.Close()
	v, e := NewOIDC(OIDCConfig{Issuer: redirect.URL, Audience: "a", JWKSURL: redirect.URL + "/keys"})
	if e != nil {
		t.Fatal(e)
	}
	v.client.Transport = redirect.Client().Transport
	if _, e = v.key(context.Background(), "one"); e == nil || redirects.Load() != 0 {
		t.Fatal("followed JWKS redirect")
	}
}
func canaryRequest(t *testing.T, s http.Handler, token, method, path, body string) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Idempotency-Key", "canary")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}
func TestOIDCReadOnlyREST(t *testing.T) {
	f := newOIDCFixture(t)
	s, e := store.Open(store.Config{Path: filepath.Join(t.TempDir(), "db"), MinFreeBytes: 1})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	seq := 0
	mutate := func(m api.Mutation) api.Document {
		t.Helper()
		seq++
		r, e := s.Mutate(context.Background(), "operator", fmt.Sprint(seq), m)
		if e != nil {
			t.Fatal(e)
		}
		return r.Document
	}
	create := func(path, content string) api.Document {
		return mutate(api.Mutation{Operation: "create", Path: path, Content: &content})
	}
	a := create("A/public", "exact\r\nπ\nbodyneedle")
	create("A/second", "second")
	b := create("B/secret", "DENIED-CONTENT secretneedle")
	// Cross-project aliases in both directions, and a hidden historical snapshot
	// whose title/content have been replaced before returning to project A.
	moved := create("A/old", "old")
	mutate(api.Mutation{Operation: "move", ID: moved.ID, IfRevision: moved.Revision, Destination: "B/current"})
	history := create("B/hidden-alias", "HIDDEN-HISTORY")
	history = mutate(api.Mutation{Operation: "move", ID: history.ID, IfRevision: history.Revision, Destination: "A/history"})
	clean := "safe body"
	title := "safe title"
	history = mutate(api.Mutation{Operation: "put", ID: history.ID, IfRevision: history.Revision, Content: &clean, Title: &title})
	archived := create("A/archived", "archivedbody")
	mutate(api.Mutation{Operation: "archive", ID: archived.ID, IfRevision: archived.Revision})
	deniedArchive := create("B/archived-secret", "DENIED-ARCHIVED")
	mutate(api.Mutation{Operation: "archive", ID: deniedArchive.ID, IfRevision: deniedArchive.Revision})
	handler := New(s, Config{OIDC: f.verifier})
	ta := signed(t, f.key, f.claims("A"))
	tb := signed(t, f.key, f.claims("B"))
	status, raw := canaryRequest(t, handler, ta, "GET", "/v1/get?path=A/public", "")
	var receipt api.Receipt
	if json.Unmarshal(raw, &receipt) != nil || status != 200 || receipt.Document.Content != "exact\r\nπ\nbodyneedle" || receipt.Document.ContentHash != store.Hash(receipt.Document.Content) {
		t.Fatalf("%d %s", status, raw)
	}
	status, raw = canaryRequest(t, handler, ta, "GET", "/v1/get?path=A/public&lines=1:2", "")
	if json.Unmarshal(raw, &receipt) != nil || status != 200 || !receipt.Partial || receipt.Document.Content != "exact\r\nπ\n" || receipt.FullDocumentHash != store.Hash("exact\r\nπ\nbodyneedle") {
		t.Fatalf("range bytes %d %s", status, raw)
	}
	for _, path := range []string{
		"/v1/get?path=B/secret", "/v1/get?id=" + b.ID, "/v1/get?path=A/old", "/v1/get?path=B/archived-secret",
		"/v1/get?path=B/hidden-alias", "/v1/get?id=" + history.ID + "&revision=1",
		"/v1/get?id=" + moved.ID + "&revision=1", "/v1/get?path=B/absent",
		"/v1/get?path=B/secret&lines=1:2", "/v1/history?path=B/hidden-alias",
		"/v1/history?id=" + b.ID,
	} {
		status, raw = canaryRequest(t, handler, ta, "GET", path, "")
		if status != 404 || string(raw) != `{"version":"jocasta/v1","error":{"code":"not_found","message":"document or revision does not exist"}}`+"\n" {
			t.Fatalf("denial %s: %d %s", path, status, raw)
		}
	}
	if status, _ = canaryRequest(t, handler, tb, "GET", "/v1/get?path=B/secret", ""); status != 200 {
		t.Fatal("same-sub B grants", status)
	}
	if status, _ = canaryRequest(t, handler, tb, "GET", "/v1/get?path=A/public", ""); status != 404 {
		t.Fatal("sub cached A grants")
	}
	for _, path := range []string{"/v1/list", "/v1/list?include_archived=true", "/v1/search?q=secretneedle", "/v1/search?q=hidden-alias", "/v1/search?q=bodyneedle", "/v1/search?q=secretneedle&mode=literal", "/v1/list?project=B", "/v1/list?prefix=B/", "/v1/search?q=DENIED&include_archived=true"} {
		status, raw = canaryRequest(t, handler, ta, "GET", path, "")
		if status != 200 || strings.Contains(string(raw), "B/") || strings.Contains(string(raw), "DENIED") || strings.Contains(string(raw), "HIDDEN") {
			t.Fatalf("page leak %s: %d %s", path, status, raw)
		}
		var results api.Page
		if e := json.Unmarshal(raw, &results); e != nil {
			t.Fatal(e)
		}
		if (strings.Contains(path, "secretneedle") || strings.Contains(path, "hidden-alias") || strings.Contains(path, "q=DENIED")) && len(results.Documents) != 0 {
			t.Fatalf("denied match influenced search %s: %s", path, raw)
		}
		if strings.Contains(path, "bodyneedle") && !strings.Contains(string(raw), "A/public") {
			t.Fatal("scoped content search lost", string(raw))
		}
	}
	if status, raw = canaryRequest(t, handler, ta, "GET", "/v1/search?q=", ""); status != 400 {
		t.Fatalf("empty search %d %s", status, raw)
	}
	status, raw = canaryRequest(t, handler, ta, "GET", "/v1/history?id="+history.ID+"&limit=1", "")
	var page api.Page
	if json.Unmarshal(raw, &page) != nil || status != 200 || len(page.Documents) != 1 || page.Documents[0].Revision != 2 || !page.HasMore {
		t.Fatalf("history filtering %d %s", status, raw)
	}
	if status, raw = canaryRequest(t, handler, signed(t, f.key, f.claims("A", "B")), "GET", "/v1/history?id="+history.ID+"&limit=1&cursor="+page.NextCursor, ""); status != 400 {
		t.Fatalf("history cursor accepted changed grants %d %s", status, raw)
	}
	if status, raw = canaryRequest(t, handler, tb, "GET", "/v1/history?id="+history.ID+"&limit=1&cursor="+page.NextCursor, ""); status != 404 {
		t.Fatalf("history target grant %d %s", status, raw)
	}
	status, raw = canaryRequest(t, handler, ta, "GET", "/v1/list?limit=1", "")
	if json.Unmarshal(raw, &page) != nil || status != 200 || !page.HasMore {
		t.Fatal(string(raw))
	}
	cursor := page.NextCursor
	for _, token := range []string{tb, signed(t, f.key, f.claims("A", "B"))} {
		status, raw = canaryRequest(t, handler, token, "GET", "/v1/list?limit=1&cursor="+cursor, "")
		if status != 400 {
			t.Fatalf("cursor changed grants %d %s", status, raw)
		}
	}
	create("B/new-hidden", "never-observed")
	if status, raw = canaryRequest(t, handler, ta, "GET", "/v1/list?limit=1&cursor="+cursor, ""); status != 200 {
		t.Fatalf("hidden change expired cursor %d %s", status, raw)
	}
	create("A/new-visible", "visible")
	if status, _ = canaryRequest(t, handler, ta, "GET", "/v1/list?limit=1&cursor="+cursor, ""); status != 409 {
		t.Fatal("visible change failed to expire cursor")
	}
	for _, op := range []string{"create", "put", "move", "archive", "unarchive", "restore", "unknown"} {
		body := fmt.Sprintf(`{"operation":%q,"path":"A/public","if_revision":1,"destination":"B/changed","content":"bad","source_revision":1}`, op)
		if status, raw = canaryRequest(t, handler, ta, "POST", "/v1/mutate", body); status != 403 {
			t.Fatalf("write %s %d %s", op, status, raw)
		}
	}
	for _, path := range []string{"/metrics", "/readyz", "/unknown", "/v1/get", "/v1/search", "/v1/history"} {
		if status, _ = canaryRequest(t, handler, ta, "POST", path, "invalid body"); status != 403 {
			t.Fatal("unexpected method admitted", path, status)
		}
	}
	for _, path := range []string{"/metrics", "/readyz", "/unknown"} {
		if status, _ = canaryRequest(t, handler, ta, "GET", path, ""); status != 403 {
			t.Fatal("non-read route", path, status)
		}
	}
	if status, raw = canaryRequest(t, handler, ta, "GET", "/v1/get?path=A/archived", ""); status != 200 {
		t.Fatalf("authorized archive %d %s", status, raw)
	}
	// The unchanged CLI transports exists and diff through protected get.
	local := httptest.NewServer(handler)
	defer local.Close()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	cfgFile := filepath.Join(dir, "config.json")
	if e := os.WriteFile(tokenFile, []byte(ta), 0600); e != nil {
		t.Fatal(e)
	}
	config, _ := json.Marshal(client.Config{URL: local.URL, TokenFile: tokenFile, AllowLoopbackHTTP: true})
	if e := os.WriteFile(cfgFile, config, 0600); e != nil {
		t.Fatal(e)
	}
	for _, tt := range []struct {
		args []string
		code int
	}{
		{[]string{"exists", "B/secret", "--json"}, cli.ExitNotFound},
		{[]string{"exists", "A/public", "--json"}, cli.ExitOK},
		{[]string{"diff", "--id", history.ID, "--from", "1", "--to", "3", "--json"}, cli.ExitNotFound},
		{[]string{"diff", "A/public", "--from", "1", "--to", "1", "--json"}, cli.ExitOK},
	} {
		var out, diagnostics bytes.Buffer
		code := cli.Run(context.Background(), append([]string{"--config", cfgFile}, tt.args...), strings.NewReader(""), &out, &diagnostics, "test")
		if code != tt.code || strings.Contains(out.String()+diagnostics.String(), "HIDDEN") || strings.Contains(out.String()+diagnostics.String(), "B/secret") {
			t.Fatalf("CLI %v: %d %s %s", tt.args, code, out.String(), diagnostics.String())
		}
	}
	got, e := s.Get(context.Background(), "", a.ID, 0)
	if e != nil || got.Document.Revision != 1 || got.Document.Content != "exact\r\nπ\nbodyneedle" {
		t.Fatal("JWT wrote document")
	}
}
func TestOIDCNeverFallsBackToOperator(t *testing.T) {
	f := newOIDCFixture(t)
	// This value fits the legacy length rules, so an auth fallback really
	// would confer operator authority if the JWT branch were bypassed.
	invalid := "eyJhbGciOiJSUzI1NiJ9.e30." + strings.Repeat("a", 40)
	file := filepath.Join(t.TempDir(), "tokens")
	legacy := strings.Repeat("s", 40)
	data, _ := json.Marshal(map[string]string{"operator": invalid, "legacy": legacy})
	if e := os.WriteFile(file, data, 0600); e != nil {
		t.Fatal(e)
	}
	if e := ValidateTokens(file); e != nil {
		t.Fatal("test must use a valid legacy token file", e)
	}
	for _, v := range []*OIDCVerifier{f.verifier} {
		handler := New(nil, Config{TokensFile: file, OIDC: v})
		if status, _ := canaryRequest(t, handler, invalid, "GET", "/v1/list", ""); status != 401 {
			t.Fatal("JWT became operator", status)
		}
		r := httptest.NewRequest("POST", "/v1/mutate", nil)
		r.Header.Set("Authorization", "Bearer "+legacy)
		p, e := handler.authenticate(r)
		if e != nil || p.actor != "legacy" || p.claims != nil {
			t.Fatal("legacy authority changed", e)
		}
	}
	// Without OIDC, configured opaque operator credentials keep their exact
	// previous meaning, including punctuation. No JWT parsing is attempted.
	missing := New(nil, Config{TokensFile: file})
	r := httptest.NewRequest("POST", "/v1/mutate", nil)
	r.Header.Set("Authorization", "Bearer "+invalid)
	p, e := missing.authenticate(r)
	if e != nil || p.actor != "operator" || p.claims != nil {
		t.Fatal("legacy-only opaque authority changed", e)
	}
	if status, _ := canaryRequest(t, missing, signed(t, f.key, f.claims("A")), "GET", "/v1/list", ""); status != 401 {
		t.Fatal("JWT accepted without OIDC configuration")
	}
}
