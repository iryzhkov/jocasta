package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// The resident instructions tell agents to reconcile a conflict rather than
// retry it, so the refusal has to be actionable without a second lookup: who
// wrote the newer revision, and the two commands that resolve it with the
// document and both revisions already filled in.
func TestConflictRefusalNamesTheActorAndTheResolvingCommands(t *testing.T) {
	conf := realServer(t)
	code, out, err := run(t, conf, "first", "put", "project/contested.md", "--create", "--file", "-", "--json")
	if code != 0 {
		t.Fatalf("create %d %s %s", code, out, err)
	}
	var r Receipt
	if e := json.Unmarshal([]byte(out), &r); e != nil {
		t.Fatal(e)
	}
	id := r.Document.ID
	if code, out, err = run(t, conf, "second", "put", "--id", id, "--if-revision", "1", "--file", "-"); code != 0 {
		t.Fatalf("update %d %s %s", code, out, err)
	}
	get := "jocasta get --id " + id + " --json"
	diff := "jocasta diff --id " + id + " --from 1 --to 2"

	code, out, err = run(t, conf, "third", "put", "--id", id, "--if-revision", "1", "--file", "-")
	if code != ExitConflict {
		t.Fatalf("expected a conflict: %d %s %s", code, out, err)
	}
	for _, want := range []string{"conflict", id, "expected revision 1", "current revision 2", "test-host", get, diff} {
		if !strings.Contains(err, want) {
			t.Fatalf("conflict text does not name %q:\n%s", want, err)
		}
	}

	code, out, err = run(t, conf, "fourth", "put", "--id", id, "--if-revision", "1", "--file", "-", "--json")
	if code != ExitConflict || out != "" {
		t.Fatalf("expected a JSON conflict on stderr: %d %q %q", code, out, err)
	}
	var envelope struct {
		Error struct {
			Code             string   `json:"code"`
			Actor            string   `json:"actor"`
			ID               string   `json:"id"`
			ExpectedRevision int64    `json:"expected_revision"`
			CurrentRevision  int64    `json:"current_revision"`
			Resolve          []string `json:"resolve"`
		} `json:"error"`
	}
	if e := json.Unmarshal([]byte(err), &envelope); e != nil {
		t.Fatalf("%v: %s", e, err)
	}
	if envelope.Error.Code != "conflict" || envelope.Error.Actor != "test-host" || envelope.Error.ID != id {
		t.Fatalf("conflict JSON: %s", err)
	}
	if envelope.Error.ExpectedRevision != 1 || envelope.Error.CurrentRevision != 2 {
		t.Fatalf("conflict JSON revisions: %s", err)
	}
	if len(envelope.Error.Resolve) != 2 || envelope.Error.Resolve[0] != get || envelope.Error.Resolve[1] != diff {
		t.Fatalf("conflict JSON does not carry the resolving commands: %s", err)
	}
}

// A write answers with the new revision. The content stays out of the response
// unless the caller asks for it, and --verbose re-reads the exact revision it
// just wrote rather than trusting an echo.
func TestWriteResponseIsProportionateAndVerboseRestoresContent(t *testing.T) {
	conf := realServer(t)
	body := strings.Repeat("payload line of an audit document\n", 1355)
	code, out, err := run(t, conf, body, "put", "project/proportion.md", "--create", "--file", "-", "--json")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, err)
	}
	if strings.Contains(out, "payload line") {
		t.Fatalf("write response echoed the %d-byte body in %d bytes", len(body), len(out))
	}
	if len(out) > 1024 {
		t.Fatalf("write response is %d bytes for a one-integer answer", len(out))
	}
	var r Receipt
	if e := json.Unmarshal([]byte(out), &r); e != nil {
		t.Fatal(e)
	}
	if r.Document.Revision != 1 || r.Document.ContentHash != digest([]byte(body)) || r.TotalLines != 1355 {
		t.Fatalf("write response lost its useful answer: %s", out)
	}
	code, verbose, err := run(t, conf, body, "put", "--id", r.Document.ID, "--if-revision", "1", "--file", "-", "--json", "--verbose")
	if code != 0 {
		t.Fatalf("%d %s %s", code, verbose, err)
	}
	var full Receipt
	if e := json.Unmarshal([]byte(verbose), &full); e != nil {
		t.Fatal(e)
	}
	if full.Document.Content != body {
		t.Fatalf("--verbose did not return the written content (%d bytes)", len(full.Document.Content))
	}
}

// Search says which ranking path answered, so an agent reading --json can tell
// a relevance-ranked page from a degraded one.
func TestSearchJSONNamesTheRankingPath(t *testing.T) {
	conf := realServer(t)
	code, out, err := run(t, conf, "# Agent Experience Framework\n\nPrinciples.\n", "put", "shared/references/agent-experience-framework.md", "--create", "--file", "-", "--json")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, err)
	}
	code, out, err = run(t, conf, "", "search", "agent experience framework", "--json")
	if code != 0 || !strings.Contains(out, `"ranking":"bm25"`) {
		t.Fatalf("search JSON does not name its ranking path: %d %s %s", code, out, err)
	}
	if !strings.Contains(out, `"title":"Agent Experience Framework"`) {
		t.Fatalf("search hit lost its derived title: %s", out)
	}
}
