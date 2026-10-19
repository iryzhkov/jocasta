package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixedDocument(t *testing.T, content string) string {
	t.Helper()
	return testConfig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, Receipt{Version: "jocasta/v1", Document: Document{ID: "id1", Path: "p/a.md", Revision: 1, Content: content, ContentHash: digest([]byte(content))}})
	}))
}

// A throwaway or refreshed output is written to a device or another
// non-regular file directly: there is nothing beside /dev/null to create a
// temporary file in, and nothing to rename over.
func TestOutputToADeviceWritesDirectly(t *testing.T) {
	conf := fixedDocument(t, "throwaway\n")
	code, out, err := run(t, conf, "", "get", "p/a.md", "--output", os.DevNull, "--json")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, err)
	}
	var receipt map[string]any
	if e := json.Unmarshal([]byte(out), &receipt); e != nil {
		t.Fatal(out)
	}
	if _, ok := receipt["document"].(map[string]any)["content"]; ok {
		t.Fatal("device receipt duplicated content")
	}
	for _, extra := range [][]string{nil, {"--overwrite"}} {
		code, _, err = run(t, conf, "", append([]string{"get", "p/a.md", "--output", os.DevNull}, extra...)...)
		if code != 0 {
			t.Fatalf("%v: %d %s", extra, code, err)
		}
	}
}

// An existing regular file is still protected, the refusal names the flag that
// lifts the protection, and that flag replaces the file atomically.
func TestExistingOutputRefusalNamesOverwrite(t *testing.T) {
	conf := fixedDocument(t, "fresh")
	output := filepath.Join(t.TempDir(), "out.md")
	if e := os.WriteFile(output, []byte("stale"), 0600); e != nil {
		t.Fatal(e)
	}
	code, _, err := run(t, conf, "", "get", "p/a.md", "--output", output)
	if code != ExitIO || !strings.Contains(err, "--overwrite") {
		t.Fatalf("refusal does not name --overwrite: %d %q", code, err)
	}
	if b, _ := os.ReadFile(output); string(b) != "stale" {
		t.Fatal("refused write changed the file")
	}
	code, _, err = run(t, conf, "", "get", "p/a.md", "--output", output, "--overwrite")
	if code != 0 {
		t.Fatalf("%d %s", code, err)
	}
	if b, _ := os.ReadFile(output); string(b) != "fresh" {
		t.Fatal("--overwrite did not replace the file")
	}
	if entries, _ := os.ReadDir(filepath.Dir(output)); len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestHelpDocumentsExistsAndOverwrite(t *testing.T) {
	_, out, _ := run(t, "", "", "help")
	for _, want := range []string{"exists PATH|jocasta:ID[@REV]", "--overwrite"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help does not document %q", want)
		}
	}
}
