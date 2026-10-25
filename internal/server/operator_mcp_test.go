package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iryzhkov/jocasta/internal/store"
	"github.com/iryzhkov/jocasta/pkg/api"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
)

func TestOperatorMCPMutationsEditsAndRequestCatalog(t *testing.T) {
	f, o := newOperatorFixture(t)
	token := signed(t, o.key, operatorClaims(f, o, true))
	bearer := &mcpBearerTransport{token: token}
	session := f.client(t, bearer)
	tools, e := session.ListTools(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	if len(names) != 6 || !names["mutate"] || !names["edit"] {
		t.Fatal(names)
	}
	call := func(args map[string]any) api.Receipt {
		t.Helper()
		var r api.Receipt
		if x := mcpCall(t, session, "mutate", args, &r); x.IsError {
			t.Fatalf("%+v", x)
		}
		return r
	}
	r := call(map[string]any{"operation": "create", "path": "newproject/exact", "content": "# One\r\nπ old\r\n# Two\nleave", "idempotency_key": "create"})
	actor := "oidc:" + store.Hash(o.verifier.cfg.Issuer+"\x00same-machine")
	if r.Document.Actor != actor || r.Document.Revision != 1 || r.Document.Content != "" {
		t.Fatal(r)
	}
	r = call(map[string]any{"operation": "put", "id": r.Document.ID, "if_revision": 1, "content": "# One\r\nπ old\r\n# Two\nleave", "title": "Title", "idempotency_key": "put"})
	// Changing the title appends a revision even when content is unchanged.
	if r.Document.Revision != 2 {
		t.Fatal(r)
	}
	edit := map[string]any{"id": r.Document.ID, "if_revision": 2, "old": "old", "new": "NEW", "section": map[string]any{"heading": "One"}, "message": "exact", "idempotency_key": "edit"}
	var edited api.Receipt
	if x := mcpCall(t, session, "edit", edit, &edited); x.IsError {
		t.Fatal(x)
	}
	if edited.Document.Revision != 3 {
		t.Fatal(edited)
	}
	var got api.Receipt
	mcpCall(t, session, "get", map[string]any{"id": r.Document.ID}, &got)
	if got.Document.Content != "# One\r\nπ NEW\r\n# Two\nleave" {
		t.Fatal(got)
	}
	moved := call(map[string]any{"operation": "move", "id": r.Document.ID, "if_revision": 3, "destination": "future/moved", "idempotency_key": "move"})
	archived := call(map[string]any{"operation": "archive", "id": r.Document.ID, "if_revision": 4, "idempotency_key": "archive"})
	unarchived := call(map[string]any{"operation": "unarchive", "id": r.Document.ID, "if_revision": 5, "idempotency_key": "unarchive"})
	restored := call(map[string]any{"operation": "restore", "id": r.Document.ID, "if_revision": 6, "source_revision": 1, "idempotency_key": "restore"})
	if moved.Document.Revision != 4 || !archived.Document.Archived || unarchived.Document.Archived || restored.Document.Revision != 7 {
		t.Fatal(moved, archived, unarchived, restored)
	}
	createdReplay := call(map[string]any{"operation": "create", "path": "newproject/exact", "content": "# One\r\nπ old\r\n# Two\nleave", "idempotency_key": "create"})
	if createdReplay.Document.ID != r.Document.ID || createdReplay.Document.Revision != 1 {
		t.Fatal("mutation replay after later writes", createdReplay)
	}
	var replay api.Receipt
	mcpCall(t, session, "edit", edit, &replay)
	if replay.Document.Revision != 3 || replay.Document.UpdatedAt != edited.Document.UpdatedAt {
		t.Fatal("replay after moves and writes", replay)
	}
	edit["new"] = "changed"
	mcpError(t, session, "edit", edit, "idempotency_conflict")
	mcpError(t, session, "mutate", map[string]any{"operation": "put", "id": r.Document.ID, "if_revision": 1, "content": "bad", "idempotency_key": "conflict"}, "conflict")
	// Unknown actor/fields and absent-vs-empty content retain strict decoding.
	mcpError(t, session, "mutate", map[string]any{"operation": "create", "path": "A/spoof", "content": "bad", "actor": "spoof", "idempotency_key": "spoof"}, "actor")
	mcpError(t, session, "mutate", map[string]any{"operation": "create", "path": "A/absent", "idempotency_key": "absent"}, "invalid")
	empty := call(map[string]any{"operation": "create", "path": "A/empty", "content": "", "idempotency_key": "empty"})
	if empty.Document.Revision != 1 {
		t.Fatal(empty)
	}
	// A single native session switches catalogs and grants on every request.
	bearer.set(signed(t, o.key, operatorClaims(f, o, false)))
	tools, e = session.ListTools(context.Background(), nil)
	if e != nil || len(tools.Tools) != 4 {
		t.Fatal(tools, e)
	}
	result, e := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "mutate", Arguments: map[string]any{"operation": "archive", "id": r.Document.ID, "if_revision": 7, "idempotency_key": "denied"}})
	raw, _ := json.Marshal(result)
	if e != nil || result == nil || !result.IsError || !strings.Contains(string(raw), "forbidden") {
		t.Fatal("expected structured read-only operator refusal", result, e)
	}
	bearer.set(f.token(t, "A"))
	tools, e = session.ListTools(context.Background(), nil)
	if e != nil || len(tools.Tools) != 4 {
		t.Fatal(tools, e)
	}
	result, e = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "edit", Arguments: edit})
	raw, _ = json.Marshal(result)
	if e != nil || result == nil || !result.IsError || !strings.Contains(string(raw), "forbidden") {
		t.Fatal("expected structured limited refusal", result, e)
	}
	mcpError(t, session, "get", map[string]any{"id": r.Document.ID, "view": "outline"}, "not_found")
	bearer.set(token)
	mcpCall(t, session, "get", map[string]any{"path": "newproject/exact"}, &got)
	if !got.Alias || got.Document.Revision != 7 {
		t.Fatal("current alias", got)
	}
	mcpCall(t, session, "get", map[string]any{"path": "newproject/exact", "revision": 1}, &got)
	if got.Document.ID != r.Document.ID || got.Document.Revision != 1 {
		t.Fatal("pinned history", got)
	}
	// Operator JWT is never a REST write credential, including MCP audience.
	if status, _ := canaryRequest(t, f.server, token, "POST", "/v1/mutate", `{"operation":"archive"}`); status != 401 {
		t.Fatal(status)
	}
}

// Exercise the write handler independently of catalog selection: this isolated
// server deliberately advertises the tool to a limited TokenInfo.
func TestMCPWriteHandlerGuardIndependentOfCatalog(t *testing.T) {
	f := newMCPFixture(t)
	raw := f.token(t, "A")
	p, err := f.oidc.verifier.verifyAudience(context.Background(), raw, f.server.mcpResource)
	if err != nil {
		t.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "guard-fixture", Version: "1"}, nil)
	called := false
	addJocastaWriteTool(srv, f.server.acquireWrite, "mutate", "guard fixture", func(ctx context.Context, actor string, args mcpMutationArgs) (any, error) {
		called = true
		return nil, nil
	})
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	guarded := auth.RequireBearerToken(func(context.Context, string, *http.Request) (*auth.TokenInfo, error) {
		return &auth.TokenInfo{UserID: p.actor, Expiration: p.claims.ExpiresAt.Time, Scopes: []string{"jocasta.read"}, Extra: map[string]any{"principal": p}}, nil
	}, nil)(transport)
	server := httptest.NewServer(guarded)
	defer server.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "guard-client", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: &http.Client{Transport: &mcpBearerTransport{token: raw}, Timeout: 10 * time.Second}, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 1 {
		t.Fatal(tools, err)
	}
	mcpError(t, session, "mutate", map[string]any{"operation": "archive", "idempotency_key": "guard"}, "forbidden")
	if called {
		t.Fatal("write callback ran without current operator permission")
	}
}

type observedBody struct {
	read  bool
	bytes int
	body  io.Reader
}

func (b *observedBody) Read(p []byte) (int, error) {
	b.read = true
	n, err := b.body.Read(p)
	b.bytes += n
	return n, err
}
func (b *observedBody) Close() error { return nil }

type cancelBody struct {
	ctx     context.Context
	started chan struct{}
}

func (b *cancelBody) Read(p []byte) (int, error) {
	close(b.started)
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (b *cancelBody) Close() error { return nil }
func TestMCPWriteBodyAdmissionCancellationAndBounds(t *testing.T) {
	f, o := newOperatorFixture(t)
	token := signed(t, o.key, operatorClaims(f, o, true))
	message := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	post := func(body string, chunked bool) int {
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if chunked {
			req.ContentLength = -1
			req.TransferEncoding = []string{"chunked"}
		}
		w := httptest.NewRecorder()
		f.server.ServeHTTP(w, req)
		return w.Code
	}
	// Two large envelopes may remain live in the SDK. A rejected third
	// reads only the bounded prefix, before any large allocation.
	for i := 0; i < cap(f.server.writeBodies); i++ {
		f.server.writeBodies <- struct{}{}
	}
	body := &observedBody{body: strings.NewReader(strings.Repeat(" ", mcpBodyLimit+1) + message)}
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Body = body
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, req)
	if w.Code != 503 || body.bytes != mcpBodyLimit+1 {
		t.Fatal("allocated beyond prefix before admission", w.Code, body.bytes)
	}
	// Small protocol messages still work when large-body capacity is full.
	if status := post(message, false); status != 200 {
		t.Fatal("small message used large-body capacity", status)
	}
	for i := 0; i < cap(f.server.writeBodies); i++ {
		<-f.server.writeBodies
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if release, e := f.server.acquireWrite(canceled); e == nil {
		release()
		t.Fatal("canceled slot acquired")
	}
	live, cancelRead := context.WithCancel(context.Background())
	blocked := &cancelBody{ctx: live, started: make(chan struct{})}
	req = httptest.NewRequest("POST", "/mcp", nil).WithContext(live)
	req.Body = io.NopCloser(io.MultiReader(strings.NewReader(strings.Repeat(" ", mcpBodyLimit+1)), blocked))
	req.Header.Set("Authorization", "Bearer "+token)
	done := make(chan struct{})
	go func() { defer close(done); f.server.ServeHTTP(httptest.NewRecorder(), req) }()
	select {
	case <-blocked.started:
	case <-time.After(time.Second):
		t.Fatal("body read did not start")
	}
	if len(f.server.writeBodies) != 1 || len(f.server.writes) != 0 {
		t.Fatal("large body must reserve body capacity only")
	}
	cancelRead()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled pre-read did not release")
	}
	if len(f.server.writeBodies) != 0 || len(f.server.writes) != 0 {
		t.Fatal("cancelled body leaked capacity")
	}
	for _, invalid := range []string{"[]", "{}{}", `{"x":"\ud800"}`, string([]byte{'{', '"', 255, '"', '}'})} {
		if status := post(invalid, false); status != 400 || len(f.server.writes) != 0 {
			t.Fatal("invalid body leaked slot", status, len(f.server.writes))
		}
	}
	f.server.cfg.MaxDocumentBytes = 64 // HTTP pre-read limit has the same bound as REST.
	for _, chunked := range []bool{false, true} {
		if status := post(strings.Repeat("x", int(f.server.mcpWriteBodyLimit())+1), chunked); status != 413 || len(f.server.writes) != 0 {
			t.Fatal("oversized body", status)
		}
	}
	if status := post(message, false); status != 200 || len(f.server.writes) != 0 {
		t.Fatal("slot not released", status)
	}
	// A large valid document was impossible under the old 64-KiB envelope.
	f.server.cfg.MaxDocumentBytes = 16 << 20
	bearer := &mcpBearerTransport{token: token}
	// Race instrumentation needs a longer fixture deadline for the maximum body.
	session := f.client(t, bearer, 35*time.Second)
	var r api.Receipt
	content := strings.Repeat("π\r\n", 20000)
	if x := mcpCall(t, session, "mutate", map[string]any{"operation": "create", "path": "large/doc", "content": content, "idempotency_key": "large"}, &r); x.IsError {
		t.Fatal(x)
	}
	if r.FullDocumentHash != store.Hash(content) {
		t.Fatal(r)
	}
	// Exercise the actual default maximum, rather than only a scaled fixture.
	content = strings.Repeat("x", 16<<20)
	if x := mcpCall(t, session, "mutate", map[string]any{"operation": "create", "path": "large/maximum", "content": content, "idempotency_key": "maximum"}, &r, 35*time.Second); x.IsError {
		t.Fatal(x)
	}
	if r.FullDocumentHash != store.Hash(content) {
		t.Fatal("maximum receipt hash")
	}
	mcpError(t, session, "mutate", map[string]any{"operation": "create", "path": "large/too-large", "content": content + "x", "idempotency_key": "too-large"}, "invalid", 35*time.Second)
}
func TestOperatorMCPConcurrentCAS(t *testing.T) {
	f, o := newOperatorFixture(t)
	bearer := &mcpBearerTransport{token: signed(t, o.key, operatorClaims(f, o, true))}
	session := f.client(t, bearer)
	var r api.Receipt
	mcpCall(t, session, "mutate", map[string]any{"operation": "create", "path": "A/race", "content": "old", "idempotency_key": "create"}, &r)
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, e := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "edit", Arguments: map[string]any{"id": r.Document.ID, "if_revision": 1, "old": "old", "new": fmt.Sprint(i), "idempotency_key": fmt.Sprint(i)}})
			results <- e == nil && !res.IsError
		}(i)
	}
	wg.Wait()
	close(results)
	success := 0
	for ok := range results {
		if ok {
			success++
		}
	}
	if success != 1 {
		t.Fatal(success)
	}
	var got api.Receipt
	mcpCall(t, session, "get", map[string]any{"id": r.Document.ID}, &got)
	if got.Document.Revision != 2 {
		t.Fatal(got)
	}
}
