package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/iryzhkov/jocasta/internal/store"
	"github.com/iryzhkov/jocasta/pkg/api"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

type Config struct {
	TokensFile       string
	OIDC             *OIDCVerifier
	MaxDocumentBytes int64
}
type Server struct {
	s                  *store.Store
	cfg                Config
	slots              chan struct{}
	writes             chan struct{}
	requests, failures atomic.Uint64
	mu                 sync.Mutex
	rates              map[string]rate
}
type rate struct {
	since time.Time
	n     int
}

func New(s *store.Store, cfg Config) *Server {
	if cfg.MaxDocumentBytes == 0 {
		cfg.MaxDocumentBytes = 16 << 20
	}
	return &Server{s: s, cfg: cfg, slots: make(chan struct{}, 8), writes: make(chan struct{}, 2), rates: map[string]rate{}}
}
func readTokens(path string) (map[string]string, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, errors.New("credentials unavailable")
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if e != nil || len(raw) > 1<<20 {
		return nil, errors.New("credentials file exceeds limit")
	}
	var tokens map[string]string
	if json.Unmarshal(raw, &tokens) != nil || len(tokens) > 256 {
		return nil, errors.New("invalid credentials configuration")
	}
	seen := map[string]bool{}
	for actor, token := range tokens {
		if actor == "" || len(actor) > 128 || len(token) < 32 || len(token) > 512 || seen[token] {
			return nil, errors.New("credentials need distinct tokens of32..512bytes and actor names1..128bytes")
		}
		seen[token] = true
	}
	return tokens, nil
}
func ValidateTokens(path string) error { _, err := readTokens(path); return err }
func (s *Server) authenticate(r *http.Request) (principal, error) {
	var p principal
	header := r.Header.Get("Authorization")
	if len(r.Header.Values("Authorization")) != 1 || !strings.HasPrefix(header, "Bearer ") {
		return p, errors.New("missing token")
	}
	token := strings.TrimPrefix(header, "Bearer ")
	if token == "" || len(token) > 8192 {
		return p, errors.New("invalid token")
	}
	// Enabling OIDC reserves dotted bearers for JWT validation; a failed
	// verification never falls through to operator-token comparison.
	// Legacy-only mode retains the existing opaque-token contract.
	if s.cfg.OIDC != nil && strings.Contains(token, ".") {
		return s.cfg.OIDC.verify(r.Context(), token)
	}
	if s.cfg.TokensFile == "" {
		return p, errors.New("operator credentials disabled")
	}
	tokens, e := readTokens(s.cfg.TokensFile)
	if e != nil {
		return p, e
	}
	for actor, want := range tokens {
		if actor != "" && len(want) >= 32 && subtle.ConstantTimeCompare([]byte(want), []byte(token)) == 1 {
			return principal{actor: actor}, nil
		}
	}
	return p, errors.New("invalid token")
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/healthz" && r.Method == "GET" {
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		return
	}
	p, e := s.authenticate(r)
	if e != nil {
		s.fail(w, &api.Error{Code: "auth", Message: "authentication failed"})
		return
	}
	actor := p.actor
	if p.claims != nil {
		allowed := r.Method == "GET"
		switch r.URL.Path {
		case "/v1/get", "/v1/list", "/v1/search", "/v1/history":
		default:
			allowed = false
		}
		if !allowed {
			s.fail(w, &api.Error{Code: "forbidden", Message: "read-only credential"})
			return
		}
		ctx, err := store.WithReadAccess(r.Context(), actor, p.claims.Projects)
		if err != nil {
			s.fail(w, &api.Error{Code: "auth", Message: "authentication failed"})
			return
		}
		r = r.WithContext(ctx)
	}
	s.mu.Lock()
	// Bound newly introduced OIDC subjects as well as existing static actors.
	if len(s.rates) >= 4096 {
		for key, rate := range s.rates {
			if time.Since(rate.since) > time.Minute {
				delete(s.rates, key)
			}
		}
		if _, ok := s.rates[actor]; !ok && len(s.rates) >= 4096 {
			s.mu.Unlock()
			s.fail(w, &api.Error{Code: "rate_limited", Message: "request rate exceeded; retry later"})
			return
		}
	}
	rate := s.rates[actor]
	if time.Since(rate.since) > time.Minute {
		rate.since = time.Now()
		rate.n = 0
	}
	rate.n++
	s.rates[actor] = rate
	s.mu.Unlock()
	if rate.n > 600 {
		s.fail(w, &api.Error{Code: "rate_limited", Message: "request rate exceeded; retry later"})
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		s.fail(w, &api.Error{Code: "unavailable", Message: "server busy"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	if len(r.URL.RawQuery) > 8192 {
		s.fail(w, store.Invalid("query exceeds 8192 bytes"))
		return
	}
	q := r.URL.Query()
	var result any
	var err error
	switch r.URL.Path {
	case "/readyz":
		if r.Method != "GET" {
			err = store.Invalid("GET required")
			break
		}
		err = s.s.Health(ctx)
		result = map[string]string{"status": "ready"}
	case "/metrics":
		if r.Method != "GET" {
			err = store.Invalid("GET required")
			break
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "jocasta_requests_total %d\njocasta_errors_total %d\n", s.requests.Load(), s.failures.Load())
		if usage, e := s.s.DiskUsage(); e == nil {
			fmt.Fprintf(w, "jocasta_database_bytes %d\njocasta_wal_bytes %d\njocasta_disk_free_bytes %d\n", usage.DatabaseBytes, usage.WALBytes, usage.FreeBytes)
		}
		return
	case "/v1/mutate":
		if r.Method != "POST" {
			err = store.Invalid("POST required")
			break
		}
		select {
		case s.writes <- struct{}{}:
			defer func() { <-s.writes }()
		default:
			err = &api.Error{Code: "unavailable", Message: "write capacity busy"}
			break
		}
		if err != nil {
			break
		}
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxDocumentBytes*6+65536)
		raw, readErr := io.ReadAll(r.Body)
		if readErr != nil || !validJSONUnicode(raw) {
			err = store.Invalid("body must contain valid UTF-8 and paired Unicode escapes within request size limit")
			break
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		var m api.Mutation
		if e = decoder.Decode(&m); e != nil {
			err = store.Invalid("invalid or oversized mutation JSON")
			break
		}
		var extra any
		if e = decoder.Decode(&extra); e != io.EOF {
			err = store.Invalid("body must contain one JSON object")
			break
		}
		result, err = s.s.Mutate(ctx, actor, r.Header.Get("Idempotency-Key"), m)
	case "/v1/get":
		if r.Method != "GET" {
			err = store.Invalid("GET required")
			break
		}
		revision := int64(0)
		if q.Has("revision") {
			revision, e = strconv.ParseInt(q.Get("revision"), 10, 64)
			if e != nil || revision < 1 {
				err = store.Invalid("revision must be positive")
				break
			}
		}
		var receipt api.Receipt
		receipt, err = s.s.Get(ctx, q.Get("path"), q.Get("id"), revision)
		if err == nil && q.Has("lines") {
			receipt, err = store.Range(receipt, q.Get("lines"))
		}
		result = receipt
	case "/v1/list", "/v1/search":
		if r.Method != "GET" {
			err = store.Invalid("GET required")
			break
		}
		limit := 0
		if q.Has("limit") {
			limit, e = strconv.Atoi(q.Get("limit"))
			if e != nil || limit < 1 {
				err = store.Invalid("invalid limit")
				break
			}
		}
		archived := false
		if q.Has("include_archived") {
			archived, e = strconv.ParseBool(q.Get("include_archived"))
			if e != nil {
				err = store.Invalid("invalid include_archived")
				break
			}
		}
		result, err = s.s.Page(ctx, api.Filter{Prefix: q.Get("prefix"), Project: q.Get("project"), Kind: q.Get("kind"), Query: q.Get("q"), Mode: q.Get("mode"), Cursor: q.Get("cursor"), Limit: limit, IncludeArchived: archived}, r.URL.Path == "/v1/search")
	case "/v1/history":
		if r.Method != "GET" {
			err = store.Invalid("GET required")
			break
		}
		limit := 0
		if q.Has("limit") {
			limit, e = strconv.Atoi(q.Get("limit"))
			if e != nil || limit < 1 {
				err = store.Invalid("invalid limit")
				break
			}
		}
		result, err = s.s.History(ctx, q.Get("path"), q.Get("id"), limit, q.Get("cursor"))
	default:
		err = &api.Error{Code: "not_found", Message: "endpoint does not exist"}
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	json.NewEncoder(w).Encode(result)
}

// encoding/json replaces malformed Unicode with U+FFFD; reject it before decode
// so document bytes are never silently repaired.
func validJSONUnicode(raw []byte) bool {
	if !utf8.Valid(raw) {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		n, e := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if e != nil {
			return false
		}
		i += 4
		if n >= 0xDC00 && n <= 0xDFFF {
			return false
		}
		if n >= 0xD800 && n <= 0xDBFF {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			low, e := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if e != nil || low < 0xDC00 || low > 0xDFFF {
				return false
			}
			i += 6
		}
	}
	return true
}
func (s *Server) fail(w http.ResponseWriter, err error) {
	s.failures.Add(1)
	var ae *api.Error
	if !errors.As(err, &ae) {
		ae = &api.Error{Code: "unavailable", Message: "store operation failed"}
		if strings.Contains(strings.ToLower(err.Error()), "full") {
			ae.Code = "storage_full"
			ae.Message = "storage capacity exhausted"
		}
	}
	status := http.StatusBadRequest
	switch ae.Code {
	case "auth":
		status = 401
	case "forbidden":
		status = 403
	case "not_found":
		status = 404
	case "conflict", "cursor_expired", "idempotency_conflict":
		status = 409
	case "storage_full":
		status = 507
	case "unavailable":
		status = 503
	case "rate_limited":
		status = 429
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(api.ErrorEnvelope{Version: api.Version, Error: ae})
}
