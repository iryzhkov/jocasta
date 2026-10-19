package cli

import (
	"encoding/json"
	"github.com/iryzhkov/jocasta/internal/client"
	"github.com/iryzhkov/jocasta/internal/server"
	"github.com/iryzhkov/jocasta/internal/store"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func realServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, e := store.Open(store.Config{Path: filepath.Join(dir, "data.sqlite")})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	token := strings.Repeat("t", 48)
	tokens := filepath.Join(dir, "tokens.json")
	b, _ := json.Marshal(map[string]string{"test-host": token})
	if e = os.WriteFile(tokens, b, 0600); e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(server.New(s, server.Config{TokensFile: tokens}))
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(dir, "token")
	if e = os.WriteFile(tokenFile, []byte(token), 0600); e != nil {
		t.Fatal(e)
	}
	conf := filepath.Join(dir, "config.json")
	b, _ = json.Marshal(client.Config{URL: srv.URL, TokenFile: tokenFile, AllowLoopbackHTTP: true})
	if e = os.WriteFile(conf, b, 0600); e != nil {
		t.Fatal(e)
	}
	return conf
}
func TestServiceLifecycle(t *testing.T) {
	conf := realServer(t)
	content := "# Unicode 世界\r\ncontent   \nlast"
	code, out, err := run(t, conf, content, "put", "project/plan.md", "--create", "--file", "-", "--title", "My plan", "--kind", "plan", "--json")
	if code != 0 {
		t.Fatalf("create %d %s %s", code, out, err)
	}
	var r Receipt
	if e := json.Unmarshal([]byte(out), &r); e != nil {
		t.Fatal(e)
	}
	id := r.Document.ID
	if id == "" || r.Document.Revision != 1 {
		t.Fatal(out)
	}
	for _, ref := range []string{"project/plan.md", "jocasta:" + id + "@1"} {
		code, out, err = run(t, conf, "", "get", ref)
		if code != 0 || out != content {
			t.Fatalf("get %d %q %s", code, out, err)
		}
	}
	code, out, err = run(t, conf, "updated", "put", "project/plan.md", "--if-revision", "1", "--file", "-", "--json")
	if code != 0 {
		t.Fatalf("update %d %s", code, err)
	}
	json.Unmarshal([]byte(out), &r)
	if r.Document.Title != "My plan" || r.Document.Kind != "plan" {
		t.Fatal("content update erased metadata")
	}
	code, _, _ = run(t, conf, "stale", "put", "jocasta:"+id+"@1", "--file", "-")
	if code != ExitConflict {
		t.Fatal("stale reference accepted")
	}
	code, _, err = run(t, conf, "", "mv", "project/plan.md", "project/new.md", "--if-revision", "2")
	if code != 0 {
		t.Fatal(err)
	}
	code, out, err = run(t, conf, "", "get", "project/plan.md", "--json")
	if code != 0 {
		t.Fatal(err)
	}
	json.Unmarshal([]byte(out), &r)
	if !r.Alias || r.Document.Path != "project/new.md" {
		t.Fatal("alias broken")
	}
	code, _, err = run(t, conf, "", "archive", "--id", id, "--if-revision", "3")
	if code != 0 {
		t.Fatal(err)
	}
	code, out, err = run(t, conf, "", "search", "My plan", "--json")
	if code != 0 || !strings.Contains(out, id) {
		t.Fatalf("archived title not discoverable %d %s %s", code, out, err)
	}
	code, _, err = run(t, conf, "", "restore", "--id", id, "--revision", "1", "--if-revision", "4")
	if code != 0 {
		t.Fatal(err)
	}
	code, out, err = run(t, conf, "", "get", "--id", id, "--json")
	if code != 0 {
		t.Fatal(err)
	}
	json.Unmarshal([]byte(out), &r)
	if !r.Document.Archived || r.Document.Path != "project/new.md" || r.Document.Content != content || r.Document.Revision != 5 {
		t.Fatal(out)
	}
	code, out, err = run(t, conf, "", "get", "--id", id, "--lines", "2:99", "--json")
	if code != 0 {
		t.Fatal(err)
	}
	json.Unmarshal([]byte(out), &r)
	if !r.Partial || r.Document.Content != "content   \nlast" || r.FullDocumentHash != digest([]byte(content)) {
		t.Fatal(out)
	}
	code, out, err = run(t, conf, "", "history", "--id", id, "--limit", "2", "--json")
	if code != 0 {
		t.Fatal(err)
	}
	var page Page
	json.Unmarshal([]byte(out), &page)
	if len(page.Documents) != 2 || page.NextCursor == "" {
		t.Fatal(out)
	}
	code, out, err = run(t, conf, "", "diff", "--id", id, "--from", "1", "--to", "2", "--json")
	if code != 0 || !strings.Contains(out, "updated") {
		t.Fatalf("diff %d %s %s", code, out, err)
	}
}

// exists answers presence alone, for wait checks and scripts that must not
// fetch or write anything: 0 when the document is there, 2 when it is not.
func TestExistsReportsPresenceAndAbsence(t *testing.T) {
	conf := realServer(t)
	code, out, err := run(t, conf, "answer", "put", "project/answer.md", "--create", "--file", "-", "--json")
	if code != 0 {
		t.Fatalf("create %d %s %s", code, out, err)
	}
	var r Receipt
	if e := json.Unmarshal([]byte(out), &r); e != nil {
		t.Fatal(e)
	}
	for _, ref := range []string{"project/answer.md", "jocasta:" + r.Document.ID, "jocasta:" + r.Document.ID + "@1"} {
		code, out, err = run(t, conf, "", "exists", ref)
		if code != 0 || err != "" {
			t.Fatalf("exists %s: %d %q %q", ref, code, out, err)
		}
	}
	code, out, err = run(t, conf, "", "exists", "project/answer.md", "--json")
	if code != 0 || err != "" {
		t.Fatalf("%d %q %q", code, out, err)
	}
	var present struct {
		Version  string         `json:"version"`
		Exists   bool           `json:"exists"`
		Document map[string]any `json:"document"`
	}
	if e := json.Unmarshal([]byte(out), &present); e != nil {
		t.Fatal(out)
	}
	if present.Version != "jocasta/v1" || !present.Exists || present.Document["id"] != r.Document.ID ||
		present.Document["path"] != "project/answer.md" || present.Document["revision"] != float64(1) || present.Document["archived"] != false {
		t.Fatalf("exists document: %s", out)
	}
	if _, ok := present.Document["content"]; ok {
		t.Fatal("exists returned content")
	}
	code, out, err = run(t, conf, "", "exists", "project/missing.md")
	if code != ExitNotFound || out != "" {
		t.Fatalf("absent: %d %q %q", code, out, err)
	}
	code, out, err = run(t, conf, "", "exists", "project/missing.md", "--json")
	if code != ExitNotFound || !strings.Contains(out, `"exists":false`) || strings.Contains(out, `"document"`) || err != "" {
		t.Fatalf("absent json: %d %q %q", code, out, err)
	}
}
func TestServiceExactContentAndIdempotentImport(t *testing.T) {
	conf := realServer(t)
	for i, content := range []string{"", "no newline", "trailing\n", "CRLF\r\n", "é世🙂\n"} {
		name := []string{"empty", "plain", "lf", "crlf", "unicode"}[i]
		code, out, err := run(t, conf, content, "put", "test/"+name, "--create", "--file", "-")
		if code != 0 {
			t.Fatalf("%d %s %s", code, out, err)
		}
		code, out, err = run(t, conf, "", "get", "test/"+name)
		if code != 0 || out != content {
			t.Fatalf("roundtrip %d %q %s", code, out, err)
		}
	}
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "cafe\u0301.md"), []byte("import\r\n"), 0600)
	manifest := filepath.Join(t.TempDir(), "manifest.json")
	code, _, err := run(t, "", "", "import", root, "--dry-run", "--manifest", manifest, "--migration-id", "test-migration")
	if code != 0 {
		t.Fatal(err)
	}
	var first string
	for i := 0; i < 2; i++ {
		code, out, err := run(t, conf, "", "import", root, "--manifest", manifest, "--migration-id", "test-migration")
		if code != 0 {
			t.Fatalf("import %d %s", code, err)
		}
		var v struct {
			Receipts []ImportReceipt `json:"receipts"`
		}
		if json.Unmarshal([]byte(out), &v) != nil || len(v.Receipts) != 1 {
			t.Fatal(out)
		}
		if i == 0 {
			first = v.Receipts[0].ID
		} else if v.Receipts[0].ID != first || v.Receipts[0].Revision != 1 {
			t.Fatal("repeat import changed identity")
		}
	}
}
