package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iryzhkov/jocasta/pkg/api"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Hold delivery after the real SDK has produced a response. This models a
// client or ingress that is slow to consume it, without delaying the store.
type heldMCPResponse struct {
	http.ResponseWriter
	entered chan<- struct{}
	release <-chan struct{}
	once    sync.Once
}

func (w *heldMCPResponse) Write(p []byte) (int, error) {
	w.once.Do(func() {
		w.entered <- struct{}{}
		<-w.release
	})
	return w.ResponseWriter.Write(p)
}

func TestOperatorMCPReadResponsesDoNotReserveMutationCapacity(t *testing.T) {
	testOperatorMCPResponseCapacity(t, "list")
}

func TestOperatorMCPMutationResponsesDoNotReserveMutationCapacity(t *testing.T) {
	testOperatorMCPResponseCapacity(t, "mutate")
}

func TestOperatorMCPMutationCapacitySharedAndBounded(t *testing.T) {
	f, o := newOperatorFixture(t)
	session := f.client(t, &mcpBearerTransport{token: signed(t, o.key, operatorClaims(f, o, true))})
	var created api.Receipt
	mcpCall(t, session, "mutate", map[string]any{
		"operation": "create", "path": "capacity/seed", "content": "old", "idempotency_key": "seed",
	}, &created)
	if cap(f.server.writes) != 2 {
		t.Fatal("mutation capacity changed", cap(f.server.writes))
	}
	// Reserve the same slots REST uses, then enter via the actual SDK path.
	for i := 0; i < 2; i++ {
		f.server.writes <- struct{}{}
	}
	defer func() {
		for len(f.server.writes) > 0 {
			<-f.server.writes
		}
	}()
	var page api.Page
	if result := mcpCall(t, session, "list", map[string]any{"limit": 1}, &page); result.IsError {
		t.Fatal("reads blocked by mutations", result)
	}
	mutation := map[string]any{"operation": "create", "path": "capacity/next", "content": "new", "idempotency_key": "next"}
	edit := map[string]any{"id": created.Document.ID, "if_revision": 1, "old": "old", "new": "edited", "idempotency_key": "edit"}
	mcpError(t, session, "mutate", mutation, "write capacity busy")
	mcpError(t, session, "edit", edit, "write capacity busy")
	var got api.Receipt
	mcpCall(t, session, "get", map[string]any{"id": created.Document.ID}, &got)
	if got.Document.Revision != 1 || got.Document.Content != "old" {
		t.Fatal("refused mutation changed document", got)
	}
	<-f.server.writes
	if result := mcpCall(t, session, "mutate", mutation, &got); result.IsError || got.Document.Revision != 1 {
		t.Fatal("capacity did not recover", result, got)
	}
	if result := mcpCall(t, session, "edit", edit, &got); result.IsError || got.Document.Revision != 2 {
		t.Fatal("edit capacity did not recover", result, got)
	}
	// Store errors must release the handler's slot as well.
	edit["new"] = "different"
	mcpError(t, session, "edit", edit, "idempotency_conflict")
	if len(f.server.writes) != 1 {
		t.Fatal("mutation handler leaked shared capacity", len(f.server.writes))
	}
}

func TestRESTAndMCPShareLargeBodyCapacity(t *testing.T) {
	f, _ := newOperatorFixture(t)
	// Synthetic REST credentials stay in the fixture's asserted scratch HOME.
	token := strings.Repeat("r", 32)
	f.server.cfg.TokensFile = filepath.Join(os.Getenv("HOME"), "rest-tokens")
	if err := os.WriteFile(f.server.cfg.TokensFile, []byte(`{"rest-fixture":"`+token+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < cap(f.server.writeBodies); i++ {
		f.server.writeBodies <- struct{}{}
	}
	defer func() {
		for len(f.server.writeBodies) > 0 {
			<-f.server.writeBodies
		}
	}()
	body := &observedBody{body: strings.NewReader(`{"operation":"create","path":"capacity/rest","content":"rest"}`)}
	req := httptest.NewRequest("POST", "/v1/mutate", nil)
	req.Body = body
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", "rest-capacity")
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, req)
	if w.Code != 503 || body.read || len(f.server.writes) != 0 {
		t.Fatal("REST allocated beyond shared body budget or leaked mutation capacity", w.Code, body.read, len(f.server.writes))
	}
	<-f.server.writeBodies
	w = httptest.NewRecorder()
	f.server.ServeHTTP(w, req)
	if w.Code != 200 || len(f.server.writes) != 0 || len(f.server.writeBodies) != 1 {
		t.Fatal("REST capacity did not recover", w.Code, w.Body.String())
	}
}

func testOperatorMCPResponseCapacity(t *testing.T, heldTool string) {
	t.Helper()
	f, o := newOperatorFixture(t)
	token := signed(t, o.key, operatorClaims(f, o, true))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Exercise legacy native-client startup too. The SDK Go client defaults
	// to the newer stateless protocol and does not need this handshake.
	post := func(body string, status int) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, "POST", f.http.URL+"/mcp", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != status {
			t.Fatalf("startup: status=%d body=%s error=%v", res.StatusCode, raw, err)
		}
	}
	post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"native-fixture","version":"1"}}}`, 200)
	post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, 202)
	post(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, 200)
	post(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list","arguments":{"limit":1}}}`, 200)

	session := f.client(t, &mcpBearerTransport{token: token})
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.server.ServeHTTP(&heldMCPResponse{ResponseWriter: w, entered: entered, release: release}, r)
	}))
	defer slow.Close()
	done := make(chan error, 2)
	defer func() {
		close(release)
		for i := 0; i < 2; i++ {
			select {
			case err := <-done:
				if err != nil {
					t.Error("held read:", err)
				}
			case <-ctx.Done():
				t.Error("held read cleanup:", ctx.Err())
			}
		}
	}()
	// Two actual tool calls finish their store work but remain in SDK
	// response delivery. No document mutation is running.
	for i := 0; i < 2; i++ {
		args := map[string]any{"limit": 1}
		if heldTool == "mutate" {
			args = map[string]any{"operation": "create", "path": fmt.Sprintf("capacity/held-%d", i), "content": "held", "idempotency_key": fmt.Sprintf("held-%d", i)}
		}
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": i + 4, "method": "tools/call", "params": map[string]any{"name": heldTool, "arguments": args}})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			req, err := http.NewRequestWithContext(ctx, "POST", slow.URL+"/mcp", bytes.NewReader(body))
			if err == nil {
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
				var res *http.Response
				res, err = http.DefaultClient.Do(req)
				if err == nil {
					var reply struct {
						Result *mcp.CallToolResult `json:"result"`
					}
					err = json.NewDecoder(res.Body).Decode(&reply)
					res.Body.Close()
					if err == nil && (res.StatusCode != 200 || reply.Result == nil || reply.Result.IsError) {
						err = fmt.Errorf("held %s failed: status=%d result=%+v", heldTool, res.StatusCode, reply.Result)
					}
				}
			}
			done <- err
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("read response did not reach SDK delivery", ctx.Err())
		}
	}
	// Existing native sessions must still read and create with both completed
	// responses held. Transport admission must not report write capacity busy.
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list", Arguments: map[string]any{"limit": 1}})
	if err != nil || result.IsError {
		t.Error("list while reads deliver:", result, err)
	}
	var receipt api.Receipt
	if result := mcpCall(t, session, "mutate", map[string]any{
		"operation": "create", "path": "capacity/native", "content": "exact",
		"idempotency_key": "native-capacity",
	}, &receipt); result.IsError || receipt.Document.Revision != 1 {
		raw, _ := json.Marshal(result)
		t.Fatalf("create while reads deliver: %s receipt=%+v", raw, receipt)
	}

}
