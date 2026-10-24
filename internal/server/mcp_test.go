package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iryzhkov/jocasta/internal/store"
	"github.com/iryzhkov/jocasta/pkg/api"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpBearerTransport struct {
	mu        sync.Mutex
	token     string
	sessionID string
}

func (b *mcpBearerTransport) set(token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.token = token
}
func (b *mcpBearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	b.mu.Lock()
	token := b.token
	b.mu.Unlock()
	r.Header.Set("Authorization", "Bearer "+token)
	// A supplied session identifier must never retain a previous grant.
	r.Header.Set("Mcp-Session-Id", "synthetic-old-session")
	res, err := http.DefaultTransport.RoundTrip(r)
	if res != nil {
		b.mu.Lock()
		b.sessionID = res.Header.Get("Mcp-Session-Id")
		b.mu.Unlock()
	}
	return res, err
}

type mcpFixture struct {
	oidc   *oidcFixture
	store  *store.Store
	server *Server
	http   *httptest.Server
}

func newMCPFixture(t *testing.T) *mcpFixture {
	t.Helper()
	// Give this fixture its own HOME, then assert it before any credential or
	// database writes. Chaldea also confines the enclosing test process.
	home := filepath.Join(t.TempDir(), "home")
	tmp := filepath.Join(home, "tmp")
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", tmp)
	if os.Getenv("HOME") != home || os.Getenv("TMPDIR") != tmp {
		t.Fatal("fixture scratch environment mismatch")
	}
	if err := os.MkdirAll(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	f := &mcpFixture{oidc: newOIDCFixture(t)}
	var err error
	f.store, err = store.Open(store.Config{Path: filepath.Join(home, "db"), MinFreeBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	f.server = New(f.store, Config{OIDC: f.oidc.verifier})
	f.http = httptest.NewUnstartedServer(f.server)
	resource := "http://" + f.http.Listener.Addr().String() + "/mcp"
	if err = f.server.EnableMCP(resource, true); err != nil {
		t.Fatal(err)
	}
	f.http.Start()
	t.Cleanup(f.http.Close)
	return f
}
func (f *mcpFixture) token(t *testing.T, projects ...string) string {
	t.Helper()
	c := f.oidc.claims(projects...)
	c["aud"] = f.server.mcpResource
	return signed(t, f.oidc.key, c)
}
func (f *mcpFixture) client(t *testing.T, bearer *mcpBearerTransport, timeouts ...time.Duration) *mcp.ClientSession {
	t.Helper()
	timeout := 10 * time.Second
	if len(timeouts) > 0 {
		timeout = timeouts[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+5*time.Second)
	t.Cleanup(cancel)
	client := mcp.NewClient(&mcp.Implementation{Name: "jocasta-fixture", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: f.http.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer, Timeout: timeout},
		DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}
func mcpCall(t *testing.T, session *mcp.ClientSession, tool string, args any, out any, timeouts ...time.Duration) *mcp.CallToolResult {
	t.Helper()
	timeout := 10 * time.Second
	if len(timeouts) > 0 {
		timeout = timeouts[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	r, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatal(tool, err)
	}
	if !r.IsError && out != nil {
		b, err := json.Marshal(r.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(b, out); err != nil {
			t.Fatalf("decode %s: %s: %v", tool, b, err)
		}
	}
	return r
}
func mcpError(t *testing.T, session *mcp.ClientSession, tool string, args any, code string, timeouts ...time.Duration) {
	t.Helper()
	r := mcpCall(t, session, tool, args, nil, timeouts...)
	raw, _ := json.Marshal(r)
	if !r.IsError || !strings.Contains(string(raw), code) || strings.Contains(string(raw), "DENIED") {
		t.Fatalf("expected %s error: %s", code, raw)
	}
}

func TestMCPReadOnlySDKHTTP(t *testing.T) {
	f := newMCPFixture(t)
	seq := 0
	mutate := func(m api.Mutation) api.Document {
		t.Helper()
		seq++
		r, err := f.store.Mutate(context.Background(), "operator", fmt.Sprint(seq), m)
		if err != nil {
			t.Fatal(err)
		}
		return r.Document
	}
	create := func(path, body string) api.Document {
		return mutate(api.Mutation{Operation: "create", Path: path, Content: &body})
	}
	a := create("A/public", "exact\r\nπ\nbodyneedle")
	create("A/second", "second")
	b := create("B/secret", "DENIED secretneedle")
	moved := create("A/old", "old")
	moved = mutate(api.Mutation{Operation: "move", ID: moved.ID, IfRevision: moved.Revision, Destination: "B/current"})
	history := create("B/hidden-alias", "DENIED-HISTORY")
	history = mutate(api.Mutation{Operation: "move", ID: history.ID, IfRevision: history.Revision, Destination: "A/history"})
	body, title := "safe body", "safe title"
	history = mutate(api.Mutation{Operation: "put", ID: history.ID, IfRevision: history.Revision, Content: &body, Title: &title})
	alias := create("A/old-name", "alias")
	alias = mutate(api.Mutation{Operation: "move", ID: alias.ID, IfRevision: alias.Revision, Destination: "A/new-name"})
	archive := create("A/archived", "archive")
	mutate(api.Mutation{Operation: "archive", ID: archive.ID, IfRevision: archive.Revision})

	bearer := &mcpBearerTransport{token: f.token(t, "A", "B")}
	session := f.client(t, bearer)
	var receipt api.Receipt
	if mcpCall(t, session, "get", map[string]any{"id": b.ID}, &receipt).IsError || receipt.Document.ID != b.ID {
		t.Fatal("broad grant not exercised")
	}
	bearer.set(f.token(t, "A")) // Same subject and same SDK client, narrower rights.
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatal("missing read hint")
		}
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "get,history,list,search" {
		t.Fatal(names)
	}
	for _, args := range []map[string]any{{"path": "A/public", "extra": true}, {"path": 7}} {
		r, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "get", Arguments: args})
		if err == nil && (r == nil || !r.IsError) {
			t.Fatalf("invalid schema accepted: %#v", args)
		}
	}
	if mcpCall(t, session, "get", map[string]any{"id": a.ID}, &receipt).IsError ||
		receipt.Document.Content != "exact\r\nπ\nbodyneedle" ||
		receipt.Document.ContentHash != store.Hash(receipt.Document.Content) {
		t.Fatalf("receipt %#v", receipt)
	}
	if mcpCall(t, session, "get", map[string]any{"path": "A/public", "lines": "1:2"}, &receipt).IsError ||
		!receipt.Partial || receipt.Document.Content != "exact\r\nπ\n" ||
		receipt.FullDocumentHash != store.Hash("exact\r\nπ\nbodyneedle") {
		t.Fatalf("range %#v", receipt)
	}
	if mcpCall(t, session, "get", map[string]any{"path": "A/old-name"}, &receipt).IsError || !receipt.Alias || receipt.Document.ID != alias.ID {
		t.Fatalf("allowed alias %#v", receipt)
	}
	for _, args := range []map[string]any{
		{"path": "B/secret"}, {"id": b.ID}, {"path": "A/old"}, {"path": "B/hidden-alias"},
		{"id": moved.ID, "revision": 1}, {"id": history.ID, "revision": 1}, {"path": "B/missing"},
	} {
		mcpError(t, session, "get", args, "not_found")
	}
	for _, args := range []map[string]any{{"id": b.ID}, {"path": "B/hidden-alias"}} {
		mcpError(t, session, "history", args, "not_found")
	}
	for _, tt := range []struct {
		tool  string
		args  map[string]any
		empty bool
	}{
		{"list", map[string]any{}, false},
		{"list", map[string]any{"include_archived": true}, false},
		{"list", map[string]any{"project": "B"}, true},
		{"list", map[string]any{"prefix": "B/"}, true},
		{"search", map[string]any{"q": "secretneedle"}, true},
		{"search", map[string]any{"q": "DENIED", "mode": "literal", "include_archived": true}, true},
		{"search", map[string]any{"q": "hidden-alias"}, true},
		{"search", map[string]any{"q": "bodyneedle"}, false},
	} {
		var page api.Page
		r := mcpCall(t, session, tt.tool, tt.args, &page)
		raw, _ := json.Marshal(page)
		if r.IsError || strings.Contains(string(raw), "B/") || strings.Contains(string(raw), "DENIED") ||
			(tt.empty && len(page.Documents) != 0) || (!tt.empty && len(page.Documents) == 0) {
			t.Fatalf("page %s %v: %s %#v", tt.tool, tt.args, raw, r)
		}
	}
	var page api.Page
	if mcpCall(t, session, "history", map[string]any{"id": history.ID, "limit": 1}, &page).IsError ||
		len(page.Documents) != 1 || page.Documents[0].Revision != 2 || !page.HasMore {
		t.Fatalf("history %#v", page)
	}
	historyCursor := page.NextCursor
	bearer.set(f.token(t, "A", "B"))
	mcpError(t, session, "history", map[string]any{"id": history.ID, "limit": 1, "cursor": historyCursor}, "invalid")
	bearer.set(f.token(t, "A"))
	if mcpCall(t, session, "list", map[string]any{"limit": 1}, &page).IsError || !page.HasMore {
		t.Fatalf("cursor %#v", page)
	}
	cursor := page.NextCursor
	if mcpCall(t, session, "list", map[string]any{"limit": 1, "cursor": cursor}, &page).IsError {
		t.Fatal("valid cursor")
	}
	bearer.set(f.token(t, "A", "B"))
	mcpError(t, session, "list", map[string]any{"limit": 1, "cursor": cursor}, "invalid")
	bearer.set(f.token(t, "A"))
	create("B/new-hidden", "DENIED")
	if mcpCall(t, session, "list", map[string]any{"limit": 1, "cursor": cursor}, &page).IsError {
		t.Fatal("hidden change affected cursor")
	}
	create("A/new-visible", "visible")
	mcpError(t, session, "list", map[string]any{"limit": 1, "cursor": cursor}, "cursor_expired")
	mcpError(t, session, "search", map[string]any{}, "invalid")
	for _, revision := range []int{-1, 0} {
		mcpError(t, session, "get", map[string]any{"path": "A/public", "revision": revision}, "invalid")
	}
	mcpError(t, session, "list", map[string]any{"limit": 0}, "invalid")

	// No registered write tool, including for a broad JWT.
	bearer.set(f.token(t, "A", "B"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "mutate", Arguments: map[string]any{
		"operation": "put", "id": a.ID, "if_revision": 1, "content": "bad"}})
	if err == nil && (r == nil || !r.IsError) {
		t.Fatalf("mutation admitted %#v", r)
	}
	got, err := f.store.Get(context.Background(), "", a.ID, 0)
	if err != nil || got.Document.Revision != 1 || got.Document.Content != "exact\r\nπ\nbodyneedle" {
		t.Fatal("MCP wrote store", err)
	}
	bearer.mu.Lock()
	sid := bearer.sessionID
	bearer.mu.Unlock()
	if sid != "" {
		t.Fatal("stateless server issued session", sid)
	}
}

func TestMCPHTTPAuthenticationAndBounds(t *testing.T) {
	f := newMCPFixture(t)
	good := f.token(t, "A")
	post := func(token, body string, headers map[string]string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, f.http.URL+"/mcp", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for k, v := range headers {
			if k == "Transfer-Encoding" && v == "chunked" {
				req.ContentLength = -1
				req.TransferEncoding = []string{"chunked"}
				continue
			}
			req.Header.Set(k, v)
		}
		res, err := f.http.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, res.Header, string(raw)
	}
	message := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	for _, tt := range []struct {
		name  string
		token string
	}{
		{"missing", ""}, {"invalid", "bad.jwt.token"},
		{"REST-client-only", signed(t, f.oidc.key, f.oidc.claims("A"))},
		{"wrong-audience", func() string { c := f.oidc.claims("A"); c["aud"] = "other-resource"; return signed(t, f.oidc.key, c) }()},
		{"expired", func() string {
			c := f.oidc.claims("A")
			c["aud"] = f.server.mcpResource
			c["iat"] = f.oidc.clock.Load() - 400
			c["nbf"] = c["iat"]
			c["exp"] = f.oidc.clock.Load() - 60
			return signed(t, f.oidc.key, c)
		}()},
	} {
		status, h, raw := post(tt.token, message, nil)
		if status != 401 || !strings.Contains(h.Get("WWW-Authenticate"), `resource_metadata="`+f.server.mcpOrigin+`/.well-known/oauth-protected-resource/mcp"`) ||
			!strings.Contains(h.Get("WWW-Authenticate"), `scope="jocasta.read"`) {
			t.Fatalf("%s %d %v %s", tt.name, status, h, raw)
		}
	}
	c := f.oidc.claims("A")
	c["aud"], c["scope"] = f.server.mcpResource, "openid"
	noRead := signed(t, f.oidc.key, c)
	status, h, raw := post(noRead, message, nil)
	if status != 403 || !strings.Contains(h.Get("WWW-Authenticate"), `error="insufficient_scope"`) || !strings.Contains(h.Get("WWW-Authenticate"), `scope="jocasta.read"`) {
		t.Fatalf("scope refusal %d %v %s", status, h, raw)
	}
	if status, _, raw := post(noRead+"x", message, nil); status != 401 {
		t.Fatalf("unsigned insufficient scope %d %s", status, raw)
	}
	// A configured opaque operator credential cannot widen MCP rights.
	operator := strings.Repeat("o", 40)
	dotted := strings.Repeat("bad.", 10)
	path := filepath.Join(os.Getenv("HOME"), "tokens")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"operator":%q,"dotted":%q}`, operator, dotted)), 0600); err != nil {
		t.Fatal(err)
	}
	f.server.cfg.TokensFile = path
	for _, token := range []string{operator, dotted} {
		status, _, raw := post(token, message, nil)
		if status != 401 {
			t.Fatalf("fallback %d %s", status, raw)
		}
	}
	// Existing REST operator behavior and configured REST audience remain valid.
	if status, _ := canaryRequest(t, f.server, operator, "GET", "/readyz", ""); status != 200 {
		t.Fatal("operator REST changed", status)
	}
	if status, _ := canaryRequest(t, f.server, signed(t, f.oidc.key, f.oidc.claims("A")), "GET", "/v1/list", ""); status != 200 {
		t.Fatal("REST audience changed", status)
	}

	res, err := f.http.Client().Get(f.http.URL + "/.well-known/oauth-protected-resource/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var metadata struct {
		Resource string
		Issuers  []string `json:"authorization_servers"`
		Scopes   []string `json:"scopes_supported"`
		Methods  []string `json:"bearer_methods_supported"`
	}
	if err = json.NewDecoder(res.Body).Decode(&metadata); err != nil || res.StatusCode != 200 ||
		metadata.Resource != f.server.mcpResource || len(metadata.Issuers) != 1 || metadata.Issuers[0] != f.oidc.verifier.cfg.Issuer ||
		strings.Join(metadata.Scopes, ",") != "jocasta.read" || strings.Join(metadata.Methods, ",") != "header" {
		t.Fatalf("metadata %#v %v", metadata, err)
	}
	for _, origin := range []string{"https://evil.invalid", "null", f.server.mcpOrigin + "/", ""} {
		headers := map[string]string{"Origin": origin, "X-Forwarded-Host": "evil.invalid", "X-Forwarded-Proto": "https"}
		status, _, raw := post(good, message, headers)
		if status != 403 {
			t.Fatalf("origin %q %d %s", origin, status, raw)
		}
	}
	for _, headers := range []map[string]string{nil, {"Origin": f.server.mcpOrigin}} {
		status, _, raw := post(good, message, headers)
		if status != 200 {
			t.Fatalf("valid origin %d %s", status, raw)
		}
	}
	for _, body := range []string{"{", message + message, "[]", `{"jsonrpc":"2.0","method":"tools/list","params":"\ud800"}`} {
		status, _, raw := post(good, body, nil)
		if status != 400 {
			t.Fatalf("malformed %d %s", status, raw)
		}
	}
	oversized := `{"padding":"` + strings.Repeat("x", mcpBodyLimit) + `"}`
	for _, headers := range []map[string]string{nil, {"Transfer-Encoding": "chunked"}} {
		if status, _, raw := post(good, oversized, headers); status != 413 {
			t.Fatalf("oversize %v %d %s", headers, status, raw)
		}
	}
	for _, method := range []string{"GET", "DELETE", "PUT"} {
		req, _ := http.NewRequest(method, f.http.URL+"/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+good)
		res, err := f.http.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 405 {
			t.Fatal("method", method, res.StatusCode)
		}
	}
	// Preserve shared admission limits.
	f.server.mu.Lock()
	f.server.rates = map[string]rate{}
	f.server.mu.Unlock()
	for i := 0; i < cap(f.server.slots); i++ {
		f.server.slots <- struct{}{}
	}
	status, _, _ = post(good, message, nil)
	for i := 0; i < cap(f.server.slots); i++ {
		<-f.server.slots
	}
	if status != 503 {
		t.Fatal("capacity", status)
	}
	p, err := f.oidc.verifier.verifyAudience(context.Background(), good, f.server.mcpResource)
	if err != nil {
		t.Fatal(err)
	}
	f.server.mu.Lock()
	f.server.rates[p.actor] = rate{since: time.Now(), n: 600}
	f.server.mu.Unlock()
	if status, _, _ := post(good, message, nil); status != 429 {
		t.Fatal("rate", status)
	}
}

func TestMCPResourceConfiguration(t *testing.T) {
	f := newOIDCFixture(t)
	for _, raw := range []string{"", "http://example.com/mcp", "https://EXAMPLE.com/mcp", "https://example.com/mcp/",
		"https://example.com/other", "https://user@example.com/mcp", "https://example.com/mcp?q=x",
		"https://example.com/mcp#f", "https://example.com:443/mcp", "https://example.com./mcp",
		"https://example.com/%6dcp", "https://example.com:/mcp", "https://example.com:0/mcp"} {
		if err := New(nil, Config{OIDC: f.verifier}).EnableMCP(raw, false); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	for _, raw := range []string{"https://example.com/mcp", "https://example.com:8443/mcp", "http://127.0.0.1:1234/mcp", "http://[::1]:1234/mcp"} {
		if err := New(nil, Config{OIDC: f.verifier}).EnableMCP(raw, true); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	if err := New(nil, Config{OIDC: f.verifier}).EnableMCP("http://127.0.0.1:1234/mcp", false); err == nil {
		t.Fatal("implicit loopback")
	}
	if err := New(nil, Config{}).EnableMCP("https://example.com/mcp", false); err == nil {
		t.Fatal("missing issuer")
	}
	disabled := New(nil, Config{})
	if status, _ := canaryRequest(t, disabled, "", "POST", "/mcp", "{}"); status != 404 {
		t.Fatal("MCP enabled by default", status)
	}
}
