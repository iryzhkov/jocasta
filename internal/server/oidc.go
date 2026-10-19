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
	Scope    string   `json:"scope"`
	Projects []string `json:"jocasta_projects"`
}

func (c *tokenClaims) Validate() error {
	if c.Subject == "" || len(c.Subject) > 256 || c.ExpiresAt == nil || c.IssuedAt == nil ||
		!c.ExpiresAt.After(c.IssuedAt.Time) || c.ExpiresAt.Sub(c.IssuedAt.Time) > maxTokenLifetime ||
		(c.NotBefore != nil && !c.NotBefore.Before(c.ExpiresAt.Time)) || len(c.Scope) > 2048 {
		return errors.New("invalid token bounds")
	}
	hasRead := false
	for _, scope := range strings.Fields(c.Scope) {
		if scope == "jocasta.read" {
			hasRead = true
		}
	}
	if !hasRead {
		return errors.New("missing read scope")
	}
	_, err := store.WithReadAccess(context.Background(), "validate", c.Projects)
	return err
}

type principal struct {
	actor  string
	claims *tokenClaims
}

func (v *OIDCVerifier) verify(ctx context.Context, raw string) (principal, error) {
	var p principal
	if len(raw) > 8192 {
		return p, errors.New("token exceeds limit")
	}
	c := new(tokenClaims)
	token, err := jwt.ParseWithClaims(raw, c, func(t *jwt.Token) (any, error) {
		// Refuse unusable claims before network work. The library validates the
		// signed registered claims after verifying RS256.
		if c.Issuer != v.cfg.Issuer || c.Validate() != nil {
			return nil, errors.New("invalid claims")
		}
		matched := false
		for _, a := range c.Audience {
			if a == v.cfg.Audience {
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
		jwt.WithAudience(v.cfg.Audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt(),
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
