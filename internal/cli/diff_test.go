package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func diffFixture(t *testing.T, conf, path string, revisions ...string) string {
	t.Helper()
	code, out, err := run(t, conf, revisions[0], "put", path, "--create", "--file", "-", "--json")
	if code != 0 {
		t.Fatalf("create %d %s %s", code, out, err)
	}
	var r Receipt
	if e := json.Unmarshal([]byte(out), &r); e != nil {
		t.Fatal(e)
	}
	for i, body := range revisions[1:] {
		code, out, err = run(t, conf, body, "put", "--id", r.Document.ID, "--if-revision", fmt.Sprint(i+1), "--file", "-", "--json")
		if code != 0 {
			t.Fatalf("update %d: %d %s %s", i, code, out, err)
		}
	}
	return r.Document.ID
}

func countPrefix(text, prefix string, except string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) && (except == "" || !strings.HasPrefix(line, except)) {
			n++
		}
	}
	return n
}

// The resident instructions tell every agent to reconcile a conflict through
// diff, so the cost of a diff must scale with the change and not with the
// document: one appended line is one small hunk, and the lines that did not
// change are context rather than a removal and a re-addition.
func TestDiffReportsOnlyTheChangedLines(t *testing.T) {
	conf := realServer(t)
	var body strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&body, "line %d\n", i)
	}
	base := body.String()
	id := diffFixture(t, conf, "project/appended.md", base, base+"appended tail line\n")
	code, out, err := run(t, conf, "", "diff", "--id", id, "--from", "1", "--to", "2")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, err)
	}
	if hunks := countPrefix(out, "@@", ""); hunks != 1 {
		t.Fatalf("one appended line produced %d hunks:\n%s", hunks, out)
	}
	if removed := countPrefix(out, "-", "---"); removed != 0 {
		t.Fatalf("%d unchanged lines were reported as removed:\n%s", removed, out)
	}
	if added := countPrefix(out, "+", "+++"); added != 1 {
		t.Fatalf("%d added lines for one appended line:\n%s", added, out)
	}
	if !strings.Contains(out, "+appended tail line\n") {
		t.Fatalf("the added line is missing:\n%s", out)
	}
	if !strings.Contains(out, " line 200\n") || !strings.Contains(out, " line 198\n") {
		t.Fatalf("the unchanged neighbours are not context:\n%s", out)
	}
	if strings.Contains(out, " line 1\n") {
		t.Fatalf("context reaches the far end of the document:\n%s", out)
	}
	if len(out) > len(base)/4 {
		t.Fatalf("diff is %d bytes for a %d-byte document and a one-line change", len(out), len(base))
	}
}

// Revisions that differ in the middle produce a hunk per changed region, and
// two revisions that do not differ produce no hunks at all.
func TestDiffHunksAndEmptyDiff(t *testing.T) {
	conf := realServer(t)
	var first, second strings.Builder
	for i := 1; i <= 120; i++ {
		fmt.Fprintf(&first, "line %d\n", i)
		switch i {
		case 10:
			fmt.Fprintf(&second, "line %d changed\n", i)
		case 90:
			fmt.Fprintf(&second, "line %d\ninserted after %d\n", i, i)
		default:
			fmt.Fprintf(&second, "line %d\n", i)
		}
	}
	id := diffFixture(t, conf, "project/hunks.md", first.String(), second.String())
	code, out, err := run(t, conf, "", "diff", "--id", id, "--from", "1", "--to", "2")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, err)
	}
	if hunks := countPrefix(out, "@@", ""); hunks != 2 {
		t.Fatalf("two distant changes produced %d hunks:\n%s", hunks, out)
	}
	if removed := countPrefix(out, "-", "---"); removed != 1 {
		t.Fatalf("%d removals for one replaced line:\n%s", removed, out)
	}
	if !strings.Contains(out, "-line 10\n") || !strings.Contains(out, "+line 10 changed\n") || !strings.Contains(out, "+inserted after 90\n") {
		t.Fatalf("changed lines missing:\n%s", out)
	}
	code, out, err = run(t, conf, "", "diff", "--id", id, "--from", "2", "--to", "2", "--json")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, err)
	}
	var payload struct {
		Diff  string `json:"diff"`
		Hunks int    `json:"hunks"`
	}
	if json.Unmarshal([]byte(out), &payload) != nil {
		t.Fatal(out)
	}
	if payload.Diff != "" || payload.Hunks != 0 {
		t.Fatalf("identical revisions produced a diff: %s", out)
	}
}

// A document whose last line has no terminator keeps the marker, and the guard
// on combined size stays.
func TestDiffMarksMissingFinalNewlineAndKeepsTheGuard(t *testing.T) {
	conf := realServer(t)
	id := diffFixture(t, conf, "project/newline.md", "alpha\nbeta\ngamma", "alpha\nbeta\ngamma changed")
	code, out, err := run(t, conf, "", "diff", "--id", id, "--from", "1", "--to", "2")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, err)
	}
	if strings.Count(out, "\\ No newline at end of file") != 2 {
		t.Fatalf("missing final newline not marked on both sides:\n%s", out)
	}
	big := strings.Repeat("x", 600<<10) + "\n"
	huge := diffFixture(t, conf, "project/huge.md", big, big+"tail\n")
	code, _, err = run(t, conf, "", "diff", "--id", huge, "--from", "1", "--to", "2")
	if code != ExitInvalid || !strings.Contains(err, "1 MiB") {
		t.Fatalf("size guard lost: %d %s", code, err)
	}
}
