package server

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/iryzhkov/jocasta/internal/store"
	"github.com/iryzhkov/jocasta/pkg/api"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthenticationRevocationAndLimits(t *testing.T) {
	dir := t.TempDir()
	token := strings.Repeat("a", 40)
	tokens := filepath.Join(dir, "tokens")
	if e := os.WriteFile(tokens, []byte("{\"test-host\":\""+token+"\"}"), 0600); e != nil {
		t.Fatal(e)
	}
	s, e := store.Open(store.Config{Path: filepath.Join(dir, "db"), MinFreeBytes: 1, MaxDocumentBytes: 1024})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ts := httptest.NewServer(New(s, Config{TokensFile: tokens, MaxDocumentBytes: 1024}))
	defer ts.Close()
	request := func(method, path, auth, key, body string) *http.Response {
		t.Helper()
		r, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		r.Header.Set("Idempotency-Key", key)
		res, e := ts.Client().Do(r)
		if e != nil {
			t.Fatal(e)
		}
		return res
	}
	res := request("GET", "/v1/list", "", "", "")
	if res.StatusCode != 401 {
		t.Fatal(res.StatusCode)
	}
	res.Body.Close()
	res = request("POST", "/v1/mutate", token, "one", `{"operation":"create","path":"demo/a","content":"exact\r\nπ"}`)
	var receipt api.Receipt
	json.NewDecoder(res.Body).Decode(&receipt)
	res.Body.Close()
	// A write answers with the new revision and the hash of what was stored;
	// the bytes themselves come back from get.
	if res.StatusCode != 200 || receipt.Document.Actor != "test-host" || receipt.Document.Content != "" || receipt.Document.ContentHash != store.Hash("exact\r\nπ") {
		t.Fatalf("%d %+v", res.StatusCode, receipt)
	}
	if stored, e := s.Get(context.Background(), "demo/a", "", 0); e != nil || stored.Document.Content != "exact\r\nπ" {
		t.Fatalf("stored bytes %+v %v", stored, e)
	}
	res = request("GET", "/v1/get?path=demo/a", token, "", "")
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !bytes.Contains(raw, []byte("content_hash")) {
		t.Fatal(string(raw))
	}
	for _, body := range []string{`{"operation":"create","path":"demo/b","content":"\ud800"}`, "{\"operation\":\"create\",\"path\":\"demo/b\",\"content\":\"\xff\"}", `{"operation":"create","path":"demo/b","content":"x","unknown":true}`, `{"operation":"create","path":"demo/b","content":"x"}{}`, `{"operation":"create","path":"demo/b","content":"` + strings.Repeat("x", 1025) + `"}`} {
		res = request("POST", "/v1/mutate", token, "bad", body)
		if res.StatusCode != 400 {
			t.Fatal(res.StatusCode)
		}
		res.Body.Close()
	}
	if _, e = s.Get(context.Background(), "demo/b", "", 0); e == nil {
		t.Fatal("rejected body left a document")
	}
	if e = os.WriteFile(tokens, []byte("{}"), 0600); e != nil {
		t.Fatal(e)
	}
	res = request("GET", "/v1/get?path=demo/a", token, "", "")
	if res.StatusCode != 401 {
		t.Fatal("revoked token accepted")
	}
	res.Body.Close()
}
