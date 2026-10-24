package server

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/iryzhkov/jocasta/internal/store"
)

// OIDCConfig pins the only trusted issuer, audience and key endpoint. The
// canary intentionally has one algorithm and fixed claim names and bounds.
type OIDCConfig struct{ Issuer, Audience, JWKSURL string }
type OIDCVerifier struct {
	cfg                OIDCConfig
	operatorClientID   string
	client             *http.Client
	now                func() time.Time
	mu                 sync.Mutex
	keys               map[string]*rsa.PublicKey
	expires, attempted time.Time
}

const maxTokenLifetime = 13 * time.Minute
const tokenLeeway = 30 * time.Second

func NewOIDC(cfg OIDCConfig) (*OIDCVerifier, error) {
	for _, raw := range []string{cfg.Issuer, cfg.JWKSURL} {
		u, e := url.Parse(raw)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
			return nil, errors.New("OIDC issuer and JWKS URL must be explicit HTTPS URLs without credentials, query or fragment")
		}
	}
	if cfg.Audience == "" || len(cfg.Audience) > 256 {
		return nil, errors.New("OIDC audience is required and bounded")
	}
	return &OIDCVerifier{cfg: cfg, now: time.Now, client: &http.Client{
		Timeout:       3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("JWKS redirects forbidden") },
	}}, nil
}

type tokenClaims struct {
	jwt.RegisteredClaims
	Scope            string          `json:"scope"`
	Projects         []string        `json:"jocasta_projects"`
	Operator         json.RawMessage `json:"jocasta_operator"`
	AZP              string          `json:"azp"`
	projectsPresent  bool
	operatorClientID string
	// Only MCP defers read-scope refusal until after signature validation, so
	// the transport can distinguish insufficient_scope (403) from bad JWTs.
	allowMissingRead bool
}

func (c *tokenClaims) Validate() error {
	if c.Subject == "" || len(c.Subject) > 256 || c.ExpiresAt == nil || c.IssuedAt == nil ||
		!c.ExpiresAt.After(c.IssuedAt.Time) || c.ExpiresAt.Sub(c.IssuedAt.Time) > maxTokenLifetime ||
		(c.NotBefore != nil && !c.NotBefore.Before(c.ExpiresAt.Time)) || len(c.Scope) > 2048 {
		return errors.New("invalid token bounds")
	}
	hasRead, hasWrite := false, false
	for _, scope := range strings.Fields(c.Scope) {
		if scope == "jocasta.write" {
			hasWrite = true
		}
		if scope == "jocasta.read" {
			hasRead = true
		}
	}
	if (!hasRead && !c.allowMissingRead) || (hasWrite && !hasRead) {
		return errors.New("missing read scope")
	}
	if c.operatorClientID != "" {
		if string(c.Operator) != "true" || c.projectsPresent || c.AZP != c.operatorClientID {
			return errors.New("invalid operator grants")
		}
		return nil
	}
	if c.Operator != nil || hasWrite {
		return errors.New("operator grant on limited issuer")
	}
	_, err := store.WithReadAccess(context.Background(), "validate", c.Projects)
	return err
}

type principal struct {
	actor  string
	claims *tokenClaims
}

func (v *OIDCVerifier) verify(ctx context.Context, raw string) (principal, error) {
	return v.verifyToken(ctx, raw, v.cfg.Audience, false)
}

// MCP requires the canonical resource audience; REST retains its configured audience.
func (v *OIDCVerifier) verifyAudience(ctx context.Context, raw, audience string) (principal, error) {
	return v.verifyToken(ctx, raw, audience, true)
}

func (v *OIDCVerifier) verifyToken(ctx context.Context, raw, audience string, allowMissingRead bool) (principal, error) {
	var p principal
	if len(raw) > 8192 {
		return p, errors.New("token exceeds limit")
	}
	c := &tokenClaims{allowMissingRead: allowMissingRead, operatorClientID: v.operatorClientID}
	token, err := jwt.ParseWithClaims(raw, c, func(t *jwt.Token) (any, error) {
		// Refuse unusable claims before network work. The library validates the
		// signed registered claims after verifying RS256.
		if c.Issuer != v.cfg.Issuer || c.Validate() != nil {
			return nil, errors.New("invalid claims")
		}
		if v.operatorClientID != "" && len(c.Audience) != 1 {
			return nil, errors.New("operator requires one resource audience")
		}
		matched := false
		for _, a := range c.Audience {
			if a == audience {
				matched = true
			}
		}
		if !matched {
			return nil, errors.New("invalid audience")
		}
		kid, ok := t.Header["kid"].(string)
		if !ok || kid == "" || len(kid) > 128 {
			return nil, errors.New("invalid key id")
		}
		return v.key(ctx, kid)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(v.cfg.Issuer),
		jwt.WithAudience(audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt(),
		jwt.WithLeeway(tokenLeeway), jwt.WithTimeFunc(v.now), jwt.WithStrictDecoding())
	if err != nil || !token.Valid {
		return p, errors.New("invalid token")
	}
	return principal{actor: "oidc:" + store.Hash(v.cfg.Issuer+"\x00"+c.Subject), claims: c}, nil
}

// key caches at most 16 public keys for one minute. A single fetch in flight
// and five second retry floor bound unknown-kid traffic and failed refreshes.
// Expired keys are never used if refresh fails.
func (v *OIDCVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	if k := v.keys[kid]; k != nil && now.Before(v.expires) {
		return k, nil
	}
	if !v.attempted.IsZero() && now.Sub(v.attempted) < 5*time.Second {
		return nil, errors.New("JWKS refresh throttled")
	}
	v.attempted = now
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.JWKSURL, nil)
	if err != nil {
		return nil, err
	}
	res, err := v.client.Do(req)
	if err != nil {
		return nil, errors.New("JWKS unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, errors.New("JWKS unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		return nil, errors.New("JWKS exceeds limit")
	}
	keys, err := parseJWKS(raw)
	if err != nil {
		return nil, err
	}
	v.keys = keys
	v.expires = v.now().Add(time.Minute)
	k := keys[kid]
	if k == nil {
		return nil, errors.New("unknown key id")
	}
	return k, nil
}

// This decodes only public RSA JWK material. Signature parsing, algorithm
// selection and verification belong to golang-jwt, not this decoder.
func parseJWKS(raw []byte) (map[string]*rsa.PublicKey, error) {
	var set struct {
		Keys []struct {
			Kty string   `json:"kty"`
			Kid string   `json:"kid"`
			Alg string   `json:"alg"`
			Use string   `json:"use"`
			N   string   `json:"n"`
			E   string   `json:"e"`
			D   string   `json:"d"`
			Ops []string `json:"key_ops"`
		} `json:"keys"`
	}
	bad := errors.New("invalid JWKS")
	if json.Unmarshal(raw, &set) != nil || len(set.Keys) == 0 || len(set.Keys) > 16 {
		return nil, bad
	}
	keys := map[string]*rsa.PublicKey{}
	seen := map[string]bool{}
	for _, j := range set.Keys {
		if j.Kid == "" || len(j.Kid) > 128 || seen[j.Kid] || j.D != "" {
			return nil, bad
		}
		seen[j.Kid] = true
		if j.Kty != "RSA" || (j.Alg != "" && j.Alg != "RS256") || (j.Use != "" && j.Use != "sig") {
			continue
		}
		if len(j.Ops) > 0 && (len(j.Ops) != 1 || j.Ops[0] != "verify") {
			continue
		}
		n, e1 := base64.RawURLEncoding.Strict().DecodeString(j.N)
		e, e2 := base64.RawURLEncoding.Strict().DecodeString(j.E)
		exponent := new(big.Int).SetBytes(e)
		modulus := new(big.Int).SetBytes(n)
		if e1 != nil || e2 != nil || modulus.BitLen() < 2048 || modulus.BitLen() > 4096 ||
			!exponent.IsInt64() || exponent.Int64() < 3 || exponent.Int64() > 2147483647 || exponent.Int64()%2 == 0 {
			return nil, bad
		}
		keys[j.Kid] = &rsa.PublicKey{N: modulus, E: int(exponent.Int64())}
	}
	if len(keys) == 0 {
		return nil, bad
	}
	return keys, nil
}

// OperatorOIDCConfig is an explicit second trust pin, usable only at /mcp.
// Audience is supplied by EnableMCP, never by an arbitrary operator setting.
type OperatorOIDCConfig struct{ Issuer, JWKSURL, ClientID string }

func NewOperatorOIDC(cfg OperatorOIDCConfig) (*OIDCVerifier, error) {
	if cfg.ClientID == "" || len(cfg.ClientID) > 256 || strings.TrimSpace(cfg.ClientID) != cfg.ClientID {
		return nil, errors.New("operator client ID required and bounded")
	}
	v, err := NewOIDC(OIDCConfig{Issuer: cfg.Issuer, JWKSURL: cfg.JWKSURL, Audience: "mcp-only"})
	if err != nil {
		return nil, err
	}
	v.operatorClientID = cfg.ClientID
	return v, nil
}

// Claim names are case-sensitive. Reject aliases of recognized claims before
// encoding/json's case-insensitive struct matching can disagree with map
// presence checks. Selection and verified claims use this same bounded grammar.
func tokenClaimFields(raw []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for name := range fields {
		for _, canonical := range []string{"iss", "sub", "aud", "exp", "nbf", "iat", "jti", "scope", "jocasta_projects", "jocasta_operator", "azp"} {
			if name != canonical && strings.EqualFold(name, canonical) {
				return nil, errors.New("noncanonical token claim name")
			}
		}
	}
	return fields, nil
}

// Preserve presence separately: projects:null is forbidden on the operator issuer.
func (c *tokenClaims) UnmarshalJSON(raw []byte) error {
	type plain tokenClaims
	fields, err := tokenClaimFields(raw)
	if err != nil {
		return err
	}
	if c.operatorClientID != "" {
		if scope, present := fields["scope"]; present && (len(scope) == 0 || scope[0] != '"') {
			return errors.New("operator scope must be a string")
		}
	}
	p := plain(*c)
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	*c = tokenClaims(p)
	_, c.projectsPresent = fields["jocasta_projects"]
	return nil
}
func (p principal) accessContext(ctx context.Context) (context.Context, error) {
	if p.claims == nil {
		return nil, errors.New("verified grants required")
	}
	if p.claims.operatorClientID != "" {
		return store.WithOperatorAccess(ctx, p.actor, p.canMutate())
	}
	return store.WithReadAccess(ctx, p.actor, p.claims.Projects)
}
func (p principal) canMutate() bool {
	return p.claims != nil && p.claims.operatorClientID != "" && hasScope(p.claims.Scope, "jocasta.read") && hasScope(p.claims.Scope, "jocasta.write")
}
func hasScope(scope, want string) bool {
	for _, s := range strings.Fields(scope) {
		if s == want {
			return true
		}
	}
	return false
}

// Decode only a bounded issuer to select a configured verifier. No token URL
// can initiate discovery, and selection confers no authority before verification.
func (s *Server) mcpVerifier(raw string) (*OIDCVerifier, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || len(raw) > 8192 {
		return nil, errors.New("invalid JWT")
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	fields, err := tokenClaimFields(payload)
	if err != nil {
		return nil, err
	}
	var issuer string
	if json.Unmarshal(fields["iss"], &issuer) != nil {
		return nil, errors.New("invalid issuer")
	}
	if s.cfg.OIDC != nil && issuer == s.cfg.OIDC.cfg.Issuer {
		return s.cfg.OIDC, nil
	}
	if s.cfg.OperatorOIDC != nil && issuer == s.cfg.OperatorOIDC.cfg.Issuer {
		return s.cfg.OperatorOIDC, nil
	}
	return nil, errors.New("unknown issuer")
}
