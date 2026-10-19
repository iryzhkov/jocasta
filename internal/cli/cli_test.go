package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/iryzhkov/jocasta/internal/client"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if e := os.WriteFile(token, []byte("secret"), 0600); e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(client.Config{URL: srv.URL, TokenFile: "token", AllowLoopbackHTTP: true})
	p := filepath.Join(dir, "config.json")
	if e := os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
	return p
}
func run(t *testing.T, conf, input string, args ...string) (int, string, string) {
	t.Helper()
	var out, err bytes.Buffer
	if conf != "" {
		args = append(args, "--config", conf)
	}
	code := Run(context.Background(), args, strings.NewReader(input), &out, &err, "test")
	return code, out.String(), err.String()
}
func TestExactGetAndFileRules(t *testing.T) {
	content := "# Test\r\nUnicode 世界  \r\nlast"
	conf := testConfig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("id") != "id123" || r.URL.Query().Get("revision") != "7" {
			t.Errorf("wrong pinned reference: %s", r.URL)
		}
		jsonOut(w, Receipt{Version: "jocasta/v1", Document: Document{ID: "id123", Path: "p/a.md", Revision: 7, Content: content, ContentHash: digest([]byte(content))}})
	}))
	code, out, err := run(t, conf, "", "get", "jocasta:id123@7")
	if code != 0 || out != content || err != "" {
		t.Fatalf("%d %q %q", code, out, err)
	}
	output := filepath.Join(t.TempDir(), "out")
	code, out, err = run(t, conf, "", "get", "jocasta:id123@7", "--output", output, "--json")
	if code != 0 {
		t.Fatal(err)
	}
	var receipt map[string]any
	if json.Unmarshal([]byte(out), &receipt) != nil {
		t.Fatal(out)
	}
	if _, ok := receipt["document"].(map[string]any)["content"]; ok {
		t.Fatal("file receipt duplicated content")
	}
	b, _ := os.ReadFile(output)
	if string(b) != content {
		t.Fatal("file bytes changed")
	}
	code, _, _ = run(t, conf, "", "get", "jocasta:id123@7", "--output", output)
	if code != ExitIO {
		t.Fatalf("existing output not refused: %d", code)
	}
	code, _, _ = run(t, conf, "", "get", "jocasta:id123@7", "--revision", "8")
	if code != ExitInvalid {
		t.Fatal("contradictory revision accepted")
	}
}
func TestMutationConflictAndMetadataPreservation(t *testing.T) {
	requests := 0
	conf := testConfig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var m map[string]any
		if json.NewDecoder(r.Body).Decode(&m) != nil {
			t.Fatal("bad JSON")
		}
		if m["if_revision"] != float64(7) || m["id"] != "id123" || m["content"] != "exact\r\n" {
			t.Errorf("bad mutation %#v", m)
		}
		if _, ok := m["title"]; ok {
			t.Error("omitted metadata would be overwritten")
		}
		if r.Header.Get("Idempotency-Key") != "known" {
			t.Error("idempotency key")
		}
		w.WriteHeader(409)
		io.WriteString(w, `{"version":"jocasta/v1","error":{"code":"conflict","message":"revision mismatch","expected_revision":7,"current_revision":8,"id":"id123"}}`)
	}))
	code, out, err := run(t, conf, "exact\r\n", "put", "jocasta:id123@7", "--file", "-", "--request-id", "known", "--json")
	if code != ExitConflict || out != "" || !strings.Contains(err, `"current_revision":8`) || requests != 1 {
		t.Fatalf("%d %q %q requests=%d", code, out, err, requests)
	}
	code, _, _ = run(t, conf, "", "put", "p/a.md", "--file", "-")
	if code != ExitInvalid || requests != 1 {
		t.Fatal("unchecked mutation sent")
	}
	code, _, _ = run(t, conf, "", "restore", "jocasta:id123@7", "--revision", "2")
	if code != ExitInvalid {
		t.Fatal("pinned restore accepted")
	}
}
func TestRangeAndSearchPagination(t *testing.T) {
	calls := 0
	conf := testConfig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/v1/get" {
			if r.URL.Query().Get("lines") != "2:9" {
				t.Error("missing lines")
			}
			jsonOut(w, Receipt{Version: "jocasta/v1", Document: Document{Content: "b\n", Revision: 3}, Partial: true, TotalLines: 2, RequestedLines: "2:9", ReturnedLines: "2:2", FullDocumentHash: "full"})
		} else {
			if r.URL.Query().Get("cursor") != "cursor1" || r.URL.Query().Get("mode") != "literal" {
				t.Error("lost query params")
			}
			jsonOut(w, Page{Version: "jocasta/v1", Documents: []Document{}, HasMore: true, Complete: false, NextCursor: "cursor2", ScannedCount: 20})
		}
	}))
	for _, args := range [][]string{{"get", "p/a", "--lines", "1:2"}, {"get", "p/a", "--lines", "0:2", "--json"}, {"get", "p/a", "--lines", "1:2", "--json", "--output", "ignored"}} {
		code, _, _ := run(t, conf, "", args...)
		if code != ExitInvalid {
			t.Fatal("invalid range accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid range performed request")
	}
	code, out, err := run(t, conf, "", "get", "p/a", "--lines", "2:9", "--json")
	if code != 0 || !strings.Contains(out, `"partial":true`) {
		t.Fatalf("%d %s %s", code, out, err)
	}
	code, out, err = run(t, conf, "", "search", "x.y/z", "--mode", "literal", "--cursor", "cursor1", "--json")
	if code != 0 || !strings.Contains(out, `"has_more":true`) || !strings.Contains(out, "cursor2") {
		t.Fatalf("%d %s %s", code, out, err)
	}
}
func TestImportDryRunRepeatAndChangedSource(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "a.md")
	original := []byte("# Plan\r\nend")
	if e := os.WriteFile(source, original, 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(source, filepath.Join(root, "linked.md")); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "invalid"), []byte{0xff}, 0600); e != nil {
		t.Fatal(e)
	}
	manifest := filepath.Join(t.TempDir(), "manifest.json")
	code, out, err := run(t, "", "", "import", root, "--dry-run", "--manifest", manifest, "--migration-id", "mig")
	if code != 0 || !strings.Contains(out, "symlink") {
		t.Fatalf("%d %s %s", code, out, err)
	}
	mutations := 0
	keys := []string{}
	var d Document
	conf := testConfig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			mutations++
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			d = Document{ID: "doc1", Revision: 1, Path: b["path"].(string), Content: b["content"].(string), ContentHash: digest([]byte(b["content"].(string)))}
		}
		jsonOut(w, Receipt{Version: "jocasta/v1", Document: d})
	}))
	for i := 0; i < 2; i++ {
		code, out, err = run(t, conf, "", "import", root, "--manifest", manifest, "--migration-id", "mig")
		if code != 0 || !strings.Contains(out, `"imported_count":1`) || !strings.Contains(out, `"skipped_count":2`) {
			t.Fatalf("%d %s %s", code, out, err)
		}
	}
	if keys[0] != keys[1] {
		t.Fatal("import keys unstable")
	}
	b, _ := os.ReadFile(source)
	if !bytes.Equal(b, original) {
		t.Fatal("source changed")
	}
	os.WriteFile(source, []byte("changed"), 0600)
	code, _, err = run(t, conf, "", "import", root, "--manifest", manifest, "--migration-id", "mig")
	if code != ExitConflict || mutations != 2 {
		t.Fatalf("changed inventory written: %d %s mutations=%d", code, err, mutations)
	}
}
func TestVersionJSON(t *testing.T) {
	code, out, err := run(t, "", "", "version", "--json")
	if code != 0 || !strings.Contains(out, `"version":"test"`) || !strings.Contains(out, `"commit":`) || err != "" {
		t.Fatalf("%d %s %s", code, out, err)
	}
}
